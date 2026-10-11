package pipeline

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/dotenv"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
	"github.com/aiulian25/composewright/internal/policy"
	"github.com/aiulian25/composewright/internal/render"
	"github.com/aiulian25/composewright/internal/store"
)

const (
	composeFileName = "compose.yaml"
	// FileCompose and FileEnv name the file an EditProblem is in.
	FileCompose = "compose"
	FileEnv     = "env"
	// SeverityError blocks saving; SeverityWarning only says so.
	SeverityError   = "error"
	SeverityWarning = "warning"
	editorSource    = "editor"
	addedInEditor   = "added in the editor"
	commentStart    = "#"
	tabCharacter    = "\t"
	lineBreak       = "\n"
	escapedDollar   = "$$"
)

var (
	// ErrComposeInvalid is an edited compose.yaml that Compose cannot load.
	ErrComposeInvalid = errors.New("compose.yaml does not load")
	// ErrEditRefused is an edit that a check refuses; EditCheck says why.
	ErrEditRefused = errors.New("the edit does not pass its checks")

	envAssignment = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=`)
	// variableUse is ${NAME}, ${NAME:-default} and the like, or $NAME;
	// $$ is an escaped dollar and is removed before matching.
	variableUse = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)([^}]*)\}|([A-Za-z_][A-Za-z0-9_]*))`)
	errorAtLine = regexp.MustCompile(`line (\d+)`)
)

// Edit is compose.yaml and .env as a person edited them side by side. Mask
// is the placeholder the page showed for secrets.
type Edit struct {
	Compose []byte
	Env     string
	Mask    string
}

// EditProblem is something wrong with an edited compose.yaml or .env, at a
// line of that file (0 when it has none). Review marks what only a grant
// on the Review step can allow: an edit never grants a privilege.
type EditProblem struct {
	File     string `json:"file"`
	Line     int    `json:"line"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Review   bool   `json:"review,omitempty"`
}

// EditCheck is what saving an edit checks, shown while it is typed and
// enforced when it is saved.
type EditCheck struct {
	Parses      bool          `json:"parses"`
	RulesPassed int           `json:"rules_passed"`
	Rules       int           `json:"rules"`
	Defined     bool          `json:"defined"`
	Problems    []EditProblem `json:"problems"`
}

// Refused reports whether a problem blocks saving.
func (check EditCheck) Refused() bool {
	return slices.ContainsFunc(check.Problems, func(problem EditProblem) bool { return problem.Severity == SeverityError })
}

// CheckEdit checks an edited compose.yaml and .env as saving them does:
// compose-go loads the file, the plan and YAML policy gates pass, every
// ${VAR} it uses is defined in the .env, and the YAML has no tabs.
func CheckEdit(ctx context.Context, dependencies Dependencies, stored *Stored, edit Edit) (EditCheck, error) {
	_, check, err := applyEdit(ctx, dependencies, stored, edit)
	return check, err
}

// SaveEdit stores an edited compose.yaml and .env as a new version made by
// hand: engine manual, not tested yet, earlier versions kept. It checks the
// edit again rather than trust the page, and refuses one with an error; an
// edit that changes nothing adds no version.
func SaveEdit(ctx context.Context, dependencies Dependencies, name string, edit Edit) (*Result, EditCheck, error) {
	stored, err := Load(ctx, dependencies.Workspace, name)
	if err != nil {
		return nil, EditCheck{}, err
	}
	result, check, err := applyEdit(ctx, dependencies, stored, edit)
	if err != nil {
		return nil, check, err
	}
	if check.Refused() {
		return nil, check, ErrEditRefused
	}
	if unchanged(stored, edit) {
		return result, check, nil
	}
	options := stored.Options
	options.Edited = true
	return result, check, persist(ctx, dependencies, options, result, 1, store.ActionEdit)
}

// unchanged reports whether an edit is the stored version as the page showed
// it, so saving it adds no version.
func unchanged(stored *Stored, edit Edit) bool {
	if string(edit.Compose) != stored.Generation.ComposeYAML {
		return false
	}
	for _, mask := range []string{edit.Mask, Revealed} {
		shown, err := stored.EnvFile(mask)
		if err == nil && string(shown) == edit.Env {
			return true
		}
	}
	return false
}

// applyEdit lays an edit over the stored version and checks it.
func applyEdit(ctx context.Context, dependencies Dependencies, stored *Stored, edit Edit) (*Result, EditCheck, error) {
	result, err := stored.result()
	if err != nil {
		return nil, EditCheck{}, err
	}
	check := EditCheck{Rules: len(policy.Rules)}
	plan, values, problems, err := editedEnv(stored, edit)
	if err != nil {
		return nil, check, err
	}
	check.Problems = append(problems, tabProblems(string(edit.Compose))...)
	result.Plan, result.Values, result.Engine = plan, values, EngineManual
	result.Files.Compose = edit.Compose
	project, err := composefile.Load(ctx, composeFileName, edit.Compose, nil, false)
	if err != nil {
		check.Problems = append(check.Problems, EditProblem{File: FileCompose, Line: errorLine(err), Severity: SeverityError,
			Message: fmt.Sprintf("%v: %v", ErrComposeInvalid, err)})
		return result, check, nil
	}
	check.Parses = true
	profile, err := loadProfile(dependencies.Config.Profile, stored.Options.ProfileFile)
	if err != nil {
		return nil, check, err
	}
	input := policyInput(dependencies.Config, plan, result.Source, stored.Options)
	input.Images, input.Profile = result.Checks, profile
	result.Report = policy.Evaluate(input).Merge(policy.EvaluateYAML(project, input))
	check.RulesPassed = check.Rules - len(failingRules(result.Report))
	check.Problems = append(check.Problems, gateProblems(result.Report, string(edit.Compose))...)
	variables := variableProblems(string(edit.Compose), edit.Env)
	check.Defined = !slices.ContainsFunc(variables, func(problem EditProblem) bool { return problem.Severity == SeverityError })
	check.Problems = append(check.Problems, variables...)
	return result, check, nil
}

// editedEnv lays an edited .env over the stored version. A line left as the
// page showed it keeps the stored value, a secret's placeholder included;
// a changed line is read as Compose reads it; a new variable joins the plan
// and a removed one leaves it.
func editedEnv(stored *Stored, edit Edit) (*ir.IR, map[string]string, []EditProblem, error) {
	shown, err := shownEnvLines(stored, edit.Mask)
	if err != nil {
		return nil, nil, nil, err
	}
	plan, values := stored.Plan.Clone(), maps.Clone(stored.Values)
	if values == nil {
		values = map[string]string{}
	}
	present := map[string]bool{}
	var problems []EditProblem
	for index, line := range strings.Split(edit.Env, lineBreak) {
		match := envAssignment.FindStringSubmatch(line)
		trimmed := strings.TrimSpace(line)
		if match == nil {
			if trimmed != "" && !strings.HasPrefix(trimmed, commentStart) {
				problems = append(problems, EditProblem{File: FileEnv, Line: index + 1, Severity: SeverityError, Message: "not a KEY=value line"})
			}
			continue
		}
		key := match[1]
		present[key] = true
		if shown[key][line] {
			continue
		}
		value, err := envValue(line, key)
		if err == nil {
			err = setVariable(plan, values, key, value)
		}
		if err != nil {
			problems = append(problems, EditProblem{File: FileEnv, Line: index + 1, Severity: SeverityError, Message: err.Error()})
		}
	}
	for _, entry := range stored.Plan.EnvFile {
		if !present[entry.Key] {
			removeVariable(plan, values, entry.Key)
		}
	}
	return plan, values, problems, nil
}

// shownEnvLines are, for each variable, the lines the page may have shown:
// with secrets masked, and revealed.
func shownEnvLines(stored *Stored, mask string) (map[string]map[string]bool, error) {
	shown := map[string]map[string]bool{}
	for _, variant := range []string{mask, Revealed} {
		env, err := stored.EnvFile(variant)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(env), lineBreak) {
			match := envAssignment.FindStringSubmatch(line)
			if match == nil {
				continue
			}
			if shown[match[1]] == nil {
				shown[match[1]] = map[string]bool{}
			}
			shown[match[1]][line] = true
		}
	}
	return shown, nil
}

// envValue reads one KEY=value line as Compose reads .env, keeping ${NAME}
// references as they are written.
func envValue(line, key string) (string, error) {
	values, err := dotenv.ParseWithLookup(strings.NewReader(line+lineBreak), func(name string) (string, bool) {
		return "${" + name + "}", true
	})
	if err != nil {
		return "", fmt.Errorf("%s does not read as a .env value: %w", key, err)
	}
	return values[key], nil
}

// setVariable stores a changed value: in the plan too when it is not a
// secret the plan generates or a person supplies, so later renders keep it.
func setVariable(plan *ir.IR, values map[string]string, key, value string) error {
	edited := ir.Provenance{Source: ir.SourceUser, SourceRef: editorSource, Confidence: ir.ConfidenceHigh}
	values[key] = value
	entry, found := plan.EnvEntry(key)
	if found && Editable(*entry) {
		entry.Value, entry.Provenance = value, edited
	}
	if found {
		return nil
	}
	if err := addVariable(plan, Variable{Key: key, Value: value}, edited); err != nil {
		return err
	}
	added, _ := plan.EnvEntry(key)
	added.Description = addedInEditor
	return nil
}

// removeVariable drops a variable the .env no longer has, with the plan's
// references to it.
func removeVariable(plan *ir.IR, values map[string]string, key string) {
	plan.EnvFile = slices.DeleteFunc(plan.EnvFile, func(entry ir.EnvFileEntry) bool { return entry.Key == key })
	for index := range plan.Services {
		service := &plan.Services[index]
		service.Env = slices.DeleteFunc(service.Env, func(variable ir.EnvRef) bool { return variable.Ref == key })
	}
	delete(values, key)
}

// tabProblems: YAML indents with spaces only.
func tabProblems(compose string) []EditProblem {
	var problems []EditProblem
	for index, line := range strings.Split(compose, lineBreak) {
		if strings.Contains(line, tabCharacter) {
			problems = append(problems, EditProblem{File: FileCompose, Line: index + 1, Severity: SeverityError, Message: "a tab: YAML indents with spaces"})
		}
	}
	return problems
}

// gateProblems are the gate's warnings and blocks at their compose.yaml
// lines; what it fills in by itself is no problem. A need, or a key the YAML
// gate forbids, can only be granted on the Review step.
func gateProblems(report policy.Report, compose string) []EditProblem {
	lines := composefile.KeyLines(compose)
	var problems []EditProblem
	for _, finding := range report.Findings {
		if finding.Severity < policy.Warn {
			continue
		}
		severity := SeverityWarning
		if finding.Severity >= policy.Need {
			severity = SeverityError
		}
		problems = append(problems, EditProblem{File: FileCompose, Line: finding.Line(lines), Severity: severity,
			Message: finding.Rule + ": " + finding.Message, Review: finding.Severity == policy.Need || finding.Rule == yamlGateRule})
	}
	return problems
}

// failingRules are the rules with a finding that blocks the plan.
func failingRules(report policy.Report) []string {
	var failing []string
	for _, finding := range report.Findings {
		if finding.Severity >= policy.Need && !slices.Contains(failing, finding.Rule) {
			failing = append(failing, finding.Rule)
		}
	}
	return failing
}

// variableProblems: every ${VAR} compose.yaml uses without a default must
// be defined in the .env; a variable nothing uses is only a warning.
func variableProblems(compose, env string) []EditProblem {
	defined, envLines := map[string]bool{}, strings.Split(env, lineBreak)
	for _, line := range envLines {
		if match := envAssignment.FindStringSubmatch(line); match != nil {
			defined[match[1]] = true
		}
	}
	used := map[string]bool{}
	var problems []EditProblem
	for index, line := range strings.Split(compose, lineBreak) {
		for _, use := range variableUse.FindAllStringSubmatch(strings.ReplaceAll(line, escapedDollar, ""), -1) {
			name := use[1] + use[3]
			used[name] = true
			hasDefault := strings.HasPrefix(use[2], ":-") || strings.HasPrefix(use[2], "-")
			if defined[name] || hasDefault {
				continue
			}
			problems = append(problems, EditProblem{File: FileCompose, Line: index + 1, Severity: SeverityError,
				Message: "${" + name + "} is not defined in .env"})
		}
	}
	for _, use := range variableUse.FindAllStringSubmatch(strings.ReplaceAll(env, escapedDollar, ""), -1) {
		used[use[1]+use[3]] = true
	}
	for index, line := range envLines {
		match := envAssignment.FindStringSubmatch(line)
		if match == nil || used[match[1]] {
			continue
		}
		problems = append(problems, EditProblem{File: FileEnv, Line: index + 1, Severity: SeverityWarning, Message: match[1] + " is not used in compose.yaml"})
	}
	return problems
}

// errorLine is the line a YAML or Compose error names, or 0.
func errorLine(err error) int {
	match := errorAtLine.FindStringSubmatch(err.Error())
	if match == nil {
		return 0
	}
	line, _ := strconv.Atoi(match[1])
	return line
}

// EditCompose stores compose.yaml as a person wrote it on the Review page,
// as a new version. The plan stays as it was. The file must load with
// compose-go and is checked by the plan's gate and by the YAML gate, the
// same check every rendered file gets, so an edit can add no privilege,
// host path or network the gate would refuse. The test runs this file and
// it is what is saved; changing the Review's options afterwards renders a
// new one from the plan, and this version stays in the history.
func EditCompose(ctx context.Context, dependencies Dependencies, name string, compose []byte) (*Result, error) {
	stored, err := Load(ctx, dependencies.Workspace, name)
	if err != nil {
		return nil, err
	}
	result, err := stored.result()
	if err != nil {
		return nil, err
	}
	project, err := composefile.Load(ctx, composeFileName, compose, nil, false)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrComposeInvalid, err)
	}
	options := stored.Options
	options.Edited = true
	profile, err := loadProfile(dependencies.Config.Profile, options.ProfileFile)
	if err != nil {
		return nil, err
	}
	input := policyInput(dependencies.Config, result.Plan, result.Source, options)
	input.Images, input.Profile = result.Checks, profile
	result.Report = policy.Evaluate(input).Merge(policy.EvaluateYAML(project, input))
	result.Files.Compose, result.Engine = compose, EngineManual
	return result, persist(ctx, dependencies, options, result, 1, store.ActionEdit)
}

// PlanCompose renders the stored plan's own compose.yaml: the file without
// the person's edits, to show which lines they changed.
func PlanCompose(configuration config.Config, version string, stored *Stored) ([]byte, error) {
	profile, err := loadProfile(configuration.Profile, stored.Options.ProfileFile)
	if err != nil {
		return nil, err
	}
	options, err := render.Options{Profile: profile, ExposeVia: stored.Options.ExposeVia, Networks: stored.Options.Networks, Storage: stored.Options.Storage,
		GrantedNeeds: stored.Options.AllowNeeds, Header: header(version, stored.source(), stored.Plan)}.Resolve()
	if err != nil {
		return nil, err
	}
	return render.Compose(stored.Plan, options)
}
