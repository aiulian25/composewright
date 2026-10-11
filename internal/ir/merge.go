package ir

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"github.com/aiulian25/composewright/internal/redact"
)

const (
	// ModelThreshold: rule values at or above it are fixed facts for the
	// model.
	ModelThreshold = ConfidenceMedium
	// ModelMark ends every description a model wrote, so a reader of .env
	// knows which comments no rule or document backs.
	ModelMark       = " (model)"
	modelNotePrefix = "Model note: "
	maxModelNotes   = 10
	maxModelText    = 400
	imageGapSuffix  = ".image"
	provenanceKey   = "provenance"
)

var referencesOnly = regexp.MustCompile(`^(\$\{[A-Za-z_][A-Za-z0-9_]*\})+$`)

// Clone returns a deep copy.
func (plan *IR) Clone() *IR {
	encoded, err := json.Marshal(plan)
	if err != nil {
		panic(err)
	}
	var copied IR
	if err := json.Unmarshal(encoded, &copied); err != nil {
		panic(err)
	}
	return &copied
}

// Fillable lists the fields a model may fill in an assisted run: what the
// rules left unfilled, except images, and rule values below threshold.
func Fillable(plan *IR, threshold string) []string {
	var fields []string
	for _, field := range plan.Unfilled {
		if !strings.HasSuffix(field, imageGapSuffix) {
			fields = append(fields, field)
		}
	}
	for index, service := range plan.Services {
		prefix := servicePrefix(index)
		if below(service.Provenance, threshold) {
			fields = append(fields, prefix+"role")
		}
		if service.Healthcheck.Defined() && below(service.Healthcheck.Provenance, threshold) {
			fields = append(fields, prefix+"healthcheck")
		}
	}
	return fields
}

// Merge copies a model's proposal into the fields Fillable reports and
// nothing else: images, needs, services and every value the rules filled
// with enough confidence stay. Copied values carry model provenance. The
// returned notices say what the model asked for and did not get.
func Merge(plan, proposal *IR, threshold string) (*IR, []string) {
	merged := plan.Clone()
	fillable := map[string]bool{}
	for _, field := range Fillable(plan, threshold) {
		fillable[field] = true
	}
	if fillable["app.summary"] {
		merged.App.Summary = modelText(proposal.App.Summary)
	}
	for index := range merged.Services {
		service := &merged.Services[index]
		offered, found := proposal.Service(service.Name)
		if found {
			fillService(service, offered, fillable, servicePrefix(index))
		}
	}
	for index := range merged.EnvFile {
		entry := &merged.EnvFile[index]
		offered, found := proposal.EnvEntry(entry.Key)
		describe := found && offered.Description != "" && fillable["env_file."+entry.Key+".description"]
		if describe {
			entry.Description = modelText(offered.Description) + ModelMark
		}
	}
	addModelProse(merged, proposal)
	return merged, rejections(plan, proposal)
}

func fillService(service *Service, offered *Service, fillable map[string]bool, prefix string) {
	if fillable[prefix+"role"] && slices.Contains(Roles, offered.Role) {
		service.Role = offered.Role
		service.Provenance = fromModel(offered.Provenance)
	}
	healthcheckOffered := offered.Healthcheck.Defined() || offered.Healthcheck.OmittedReason != ""
	if fillable[prefix+"healthcheck"] && healthcheckOffered {
		service.Healthcheck = offered.Healthcheck
		service.Healthcheck.Provenance = fromModel(offered.Healthcheck.Provenance)
	}
	if fillable[prefix+"ports"] && len(offered.Ports) > 0 {
		service.Ports = modelPorts(offered.Ports)
	}
}

// MergeRepair applies a repair after a failed validation run. For services
// the plan already has, the model may change the command, entrypoint,
// healthcheck, ports, depends_on and environment references; in env_file it
// may change non-secret values and add variables, where a secret is always
// generated. Images, needs, services, roles, users, volumes, resources and
// the values of existing secrets stay as they are.
func MergeRepair(plan, proposal *IR) (*IR, []string) {
	merged := plan.Clone()
	for index := range merged.Services {
		service := &merged.Services[index]
		offered, found := proposal.Service(service.Name)
		if found {
			repairService(service, offered)
		}
	}
	for _, offered := range proposal.EnvFile {
		existing, found := merged.EnvEntry(offered.Key)
		switch {
		case !found:
			merged.EnvFile = append(merged.EnvFile, modelEnvEntry(offered))
		case !existing.Secret && !redact.IsSecretName(existing.Key) && existing.Value != offered.Value:
			existing.Value = offered.Value
			existing.Provenance = fromModel(offered.Provenance)
		}
	}
	addModelProse(merged, proposal)
	return merged, rejections(plan, proposal)
}

// repairService never lets a repair pass by deleting the evidence: a
// healthcheck goes only with a stated reason, and ports are only replaced.
func repairService(service *Service, offered *Service) {
	service.Command, service.Entrypoint = offered.Command, offered.Entrypoint
	service.DependsOn = offered.DependsOn
	healthcheckOffered := offered.Healthcheck.Defined() || offered.Healthcheck.OmittedReason != ""
	if healthcheckOffered && !sameValues(service.Healthcheck, offered.Healthcheck) {
		service.Healthcheck = offered.Healthcheck
		service.Healthcheck.Provenance = fromModel(offered.Healthcheck.Provenance)
	}
	if len(offered.Ports) > 0 && !sameValues(service.Ports, offered.Ports) {
		service.Ports = modelPorts(offered.Ports)
	}
	secrets := map[string]EnvRef{}
	current := map[string]EnvRef{}
	for _, variable := range service.Env {
		current[variable.Key] = variable
		if variable.Secret {
			secrets[variable.Key] = variable
		}
	}
	environment := make([]EnvRef, 0, len(offered.Env))
	for _, variable := range offered.Env {
		kept, secret := secrets[variable.Key]
		if secret {
			environment = append(environment, kept)
			delete(secrets, variable.Key)
			continue
		}
		if existing, found := current[variable.Key]; found && sameValues(existing, variable) {
			environment = append(environment, existing)
			continue
		}
		variable.Provenance = fromModel(variable.Provenance)
		environment = append(environment, variable)
	}
	for _, variable := range service.Env {
		if _, dropped := secrets[variable.Key]; dropped {
			environment = append(environment, variable)
		}
	}
	service.Env = environment
}

// FromModel adopts a plan the model drafted on its own (the "model"
// engine): every value carries model provenance, no value is above medium
// confidence, and secrets are generated rather than written.
func FromModel(proposal *IR) *IR {
	plan := proposal.Clone()
	for index := range plan.Services {
		service := &plan.Services[index]
		service.Provenance = fromModel(service.Provenance)
		service.Image.Provenance = fromModel(service.Image.Provenance)
		service.Healthcheck.Provenance = fromModel(service.Healthcheck.Provenance)
		service.Ports = modelPorts(service.Ports)
		for item := range service.Env {
			service.Env[item].Provenance = fromModel(service.Env[item].Provenance)
		}
		for item := range service.Volumes {
			service.Volumes[item].Provenance = fromModel(service.Volumes[item].Provenance)
		}
	}
	for index, entry := range plan.EnvFile {
		plan.EnvFile[index] = modelEnvEntry(entry)
	}
	for index := range plan.Needs {
		plan.Needs[index].Provenance = fromModel(plan.Needs[index].Provenance)
	}
	plan.Confidence = capped(plan.Confidence)
	plan.Unfilled = []string{}
	return plan
}

// modelEnvEntry keeps a model away from secret values: a variable whose name
// marks a secret is secret, and a secret is generated unless its value only
// refers to other variables.
func modelEnvEntry(entry EnvFileEntry) EnvFileEntry {
	entry.Provenance = fromModel(entry.Provenance)
	entry.Description = modelText(entry.Description)
	if entry.Description != "" {
		entry.Description += ModelMark
	}
	entry.Secret = entry.Secret || IsSecretSetting(entry.Key, entry.Value)
	writtenSecret := entry.Secret && !referencesOnly.MatchString(entry.Value)
	if !writtenSecret {
		return entry
	}
	entry.Value = ""
	if entry.Generate == "" {
		entry.Generate = GeneratePassword24
	}
	return entry
}

func modelPorts(ports []Port) []Port {
	stamped := slices.Clone(ports)
	for index := range stamped {
		stamped[index].Provenance = fromModel(stamped[index].Provenance)
	}
	return stamped
}

// addModelProse keeps the model's notes for DEPLOY.md, marked as such, and
// every instruction it reports as suspicious.
func addModelProse(merged, proposal *IR) {
	added := 0
	for _, note := range proposal.PostInstallNotes {
		text := modelText(note)
		known := slices.Contains(merged.PostInstallNotes, text) || slices.Contains(merged.PostInstallNotes, modelNotePrefix+text)
		if text == "" || known || added == maxModelNotes {
			continue
		}
		merged.PostInstallNotes = append(merged.PostInstallNotes, modelNotePrefix+text)
		added++
	}
	for _, instruction := range proposal.SuspiciousInstructions {
		text := modelText(instruction)
		if text != "" && !slices.Contains(merged.SuspiciousInstructions, text) {
			merged.SuspiciousInstructions = append(merged.SuspiciousInstructions, text)
		}
	}
	if Rank(proposal.Confidence) < Rank(merged.Confidence) {
		merged.Confidence = capped(proposal.Confidence)
	}
}

// rejections names what the model proposed outside its remit: needs are
// requests a user grants, and images and services come from the rules.
func rejections(plan, proposal *IR) []string {
	var notices []string
	for _, need := range proposal.Needs {
		notices = append(notices, fmt.Sprintf("the model asked for %s on %s (%s); a need is granted only with --allow-need",
			need.Capability, need.Service, modelText(need.Reason)))
	}
	for _, offered := range proposal.Services {
		service, found := plan.Service(offered.Name)
		if !found {
			notices = append(notices, fmt.Sprintf("the model proposed a service %q; services come from the rules", modelText(offered.Name)))
			continue
		}
		changed := offered.Image.Repository != "" && offered.Image.Reference()+":"+offered.Image.Tag != service.Image.Reference()+":"+service.Image.Tag
		if changed {
			notices = append(notices, fmt.Sprintf("the model proposed %s for %s; the rules' %s is kept",
				modelText(offered.Image.Reference()+":"+offered.Image.Tag), service.Name, service.Image.Reference()+":"+service.Image.Tag))
		}
	}
	return notices
}

func fromModel(provenance Provenance) Provenance {
	return Provenance{Source: SourceModel, SourceRef: modelText(provenance.SourceRef), Confidence: capped(provenance.Confidence)}
}

// capped keeps a model's self-reported confidence at medium or below.
func capped(confidence string) string {
	if Rank(confidence) == 0 {
		return ConfidenceLow
	}
	if Rank(confidence) > Rank(ModelThreshold) {
		return ModelThreshold
	}
	return confidence
}

func below(provenance Provenance, threshold string) bool {
	return Rank(provenance.Confidence) < Rank(threshold)
}

// modelText is one line of bounded length.
func modelText(text string) string {
	line := strings.Join(strings.Fields(text), " ")
	runes := []rune(line)
	if len(runes) > maxModelText {
		return string(runes[:maxModelText])
	}
	return line
}

func servicePrefix(index int) string {
	return fmt.Sprintf("services.%d.", index)
}

// sameValues compares two parts of a plan without their provenance, so a
// value the model repeats keeps the source it came from.
func sameValues(left, right any) bool {
	return reflect.DeepEqual(withoutProvenance(left), withoutProvenance(right))
}

func withoutProvenance(value any) any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var tree any
	if err := json.Unmarshal(encoded, &tree); err != nil {
		return value
	}
	return stripProvenance(tree)
}

func stripProvenance(node any) any {
	switch value := node.(type) {
	case map[string]any:
		delete(value, provenanceKey)
		for key := range value {
			value[key] = stripProvenance(value[key])
		}
	case []any:
		for index := range value {
			value[index] = stripProvenance(value[index])
		}
	}
	return node
}
