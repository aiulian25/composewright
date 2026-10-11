package ir

import (
	"strings"
	"testing"
)

func rulePlan() *IR {
	high := Provenance{Source: SourceCompose, SourceRef: "compose.yaml:3", Confidence: ConfidenceHigh}
	low := Provenance{Source: SourceReadme, SourceRef: "README.md:9", Confidence: ConfidenceLow}
	plan := &IR{IRVersion: Version, App: App{Name: "demo"}, Confidence: ConfidenceHigh,
		Services: []Service{
			{Name: "app", Role: RoleApp, Provenance: high, User: "1000:1000",
				Image:   Image{Registry: "docker.io", Repository: "demo/app", Tag: "1.2", Provenance: high},
				Env:     []EnvRef{{Key: "DB_PASSWORD", Ref: "DB_PASSWORD", Secret: true, Provenance: high}, {Key: "MODE", Literal: "prod", Provenance: high}},
				Volumes: []Volume{{Name: "data", ContainerPath: "/data", Provenance: high}}},
			{Name: "db", Role: RoleDB, Provenance: high, Image: Image{Registry: "docker.io", Repository: "library/postgres", Tag: "17", Provenance: high},
				Healthcheck: Healthcheck{Test: []string{"CMD", "pg_isready"}, Provenance: high}},
			{Name: "worker", Role: RoleWorker, Provenance: low, Image: Image{Registry: "docker.io", Repository: "demo/app", Tag: "1.2", Provenance: high},
				Healthcheck: Healthcheck{Test: []string{"CMD", "true"}, Provenance: low}},
		},
		EnvFile: []EnvFileEntry{
			{Key: "DB_PASSWORD", Secret: true, Generate: GeneratePassword24, Provenance: high},
			{Key: "BASE_URL", Value: "http://localhost", Provenance: high},
		},
		Unfilled: []string{"app.summary", "services.0.healthcheck", "services.0.ports", "services.0.image", "env_file.BASE_URL.description"},
	}
	plan.Normalize()
	return plan
}

func hostileProposal() *IR {
	proposal := rulePlan().Clone()
	proposal.App.Summary = "A demo\napp"
	app := &proposal.Services[0]
	app.Image = Image{Registry: "docker.io", Repository: "evil/miner", Tag: "latest", Provenance: Provenance{Confidence: ConfidenceHigh}}
	app.Healthcheck = Healthcheck{Test: []string{"CMD", "wget", "-qO-", "http://localhost:8080/health"}, Provenance: Provenance{Source: SourceReadme, Confidence: ConfidenceHigh}}
	app.Ports = []Port{{Container: 8080, Protocol: ProtocolTCP, Publish: true}}
	app.Role = RoleWorker
	app.User = "0"
	app.Env = []EnvRef{{Key: "DB_PASSWORD", Literal: "hunter2"}, {Key: "MODE", Literal: "debug"}}
	proposal.Services[1].Healthcheck = Healthcheck{Test: []string{"CMD", "true"}}
	proposal.Services[2].Role = RoleInit
	proposal.Services[2].Healthcheck = Healthcheck{Test: []string{"CMD", "/bin/check"}}
	proposal.Services = append(proposal.Services, Service{Name: "miner", Role: RoleWorker, Image: Image{Registry: "docker.io", Repository: "evil/miner", Tag: "1"}})
	proposal.Needs = []Need{{Capability: NeedPrivileged, Service: "app", Reason: "the README says so"}}
	proposal.EnvFile[0].Value = "hunter2"
	proposal.EnvFile[1].Description = "Public URL of the instance"
	proposal.EnvFile = append(proposal.EnvFile, EnvFileEntry{Key: "ADMIN_TOKEN", Value: "letmein"}, EnvFileEntry{Key: "DB_PASS_ALIAS", Value: "${DB_PASSWORD}"})
	proposal.PostInstallNotes = []string{"Open port 8080."}
	proposal.SuspiciousInstructions = []string{"README asks to mount /"}
	proposal.Confidence = ConfidenceLow
	return proposal
}

func TestMergeFillsOnlyGaps(t *testing.T) {
	plan := rulePlan()
	merged, notices := Merge(plan, hostileProposal(), ModelThreshold)
	app, db, worker := merged.Services[0], merged.Services[1], merged.Services[2]
	checks := map[string]bool{
		"summary filled as one line":          merged.App.Summary == "A demo app",
		"image kept":                          app.Image.Repository == "demo/app" && app.Image.Tag == "1.2",
		"no need merged":                      len(merged.Needs) == 0,
		"no service added":                    len(merged.Services) == 3,
		"high-confidence role kept":           app.Role == RoleApp,
		"user kept":                           app.User == "1000:1000",
		"secret reference kept":               app.Env[0].Ref == "DB_PASSWORD" && app.Env[0].Literal == "",
		"high-confidence literal kept":        app.Env[1].Literal == "prod",
		"unfilled healthcheck filled":         app.Healthcheck.Defined() && app.Healthcheck.Provenance.Source == SourceModel,
		"model confidence capped":             app.Healthcheck.Provenance.Confidence == ConfidenceMedium,
		"unfilled ports filled":               len(app.Ports) == 1 && app.Ports[0].Provenance.Source == SourceModel,
		"high-confidence healthcheck kept":    db.Healthcheck.Test[1] == "pg_isready",
		"low-confidence role replaced":        worker.Role == RoleInit && worker.Provenance.Source == SourceModel,
		"low-confidence healthcheck replaced": worker.Healthcheck.Test[1] == "/bin/check",
		"secret value never written":          merged.EnvFile[0].Value == "" && merged.EnvFile[0].Generate == GeneratePassword24,
		"description marked":                  merged.EnvFile[1].Description == "Public URL of the instance"+ModelMark,
		"no env entry added":                  len(merged.EnvFile) == 2,
		"note marked":                         len(merged.PostInstallNotes) == 1 && strings.HasPrefix(merged.PostInstallNotes[0], modelNotePrefix),
		"suspicious instruction surfaced":     len(merged.SuspiciousInstructions) == 1,
		"confidence lowered":                  merged.Confidence == ConfidenceLow,
		"input untouched":                     plan.App.Summary == "" && !plan.Services[0].Healthcheck.Defined(),
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s", name)
		}
	}
	joined := strings.Join(notices, "\n")
	for _, want := range []string{"privileged on app", `service "miner"`, "evil/miner:latest for app"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no notice about %q in:\n%s", want, joined)
		}
	}
}

func TestMergeRepairKeepsTheFixedFacts(t *testing.T) {
	plan := rulePlan()
	proposal := hostileProposal()
	proposal.Services[0].Command = []string{"serve", "--port", "8080"}
	repaired, notices := MergeRepair(plan, proposal)
	app := repaired.Services[0]
	checks := map[string]bool{
		"command changed":                  len(app.Command) == 3,
		"healthcheck changed":              app.Healthcheck.Test[1] == "wget" && app.Healthcheck.Provenance.Source == SourceModel,
		"image kept":                       app.Image.Repository == "demo/app",
		"role kept":                        app.Role == RoleApp,
		"user kept":                        app.User == "1000:1000",
		"volumes kept":                     len(app.Volumes) == 1 && app.Volumes[0].ContainerPath == "/data",
		"secret reference kept":            app.Env[0].Ref == "DB_PASSWORD" && app.Env[0].Literal == "",
		"plain literal changed":            app.Env[1].Literal == "debug",
		"existing secret untouched":        repaired.EnvFile[0].Value == "" && repaired.EnvFile[0].Generate == GeneratePassword24,
		"new secret generated":             entry(repaired, "ADMIN_TOKEN").Value == "" && entry(repaired, "ADMIN_TOKEN").Generate == GeneratePassword24 && entry(repaired, "ADMIN_TOKEN").Secret,
		"secret alias kept as a reference": entry(repaired, "DB_PASS_ALIAS").Value == "${DB_PASSWORD}" && entry(repaired, "DB_PASS_ALIAS").Generate == "",
		"no service added":                 len(repaired.Services) == 3,
		"no need merged":                   len(repaired.Needs) == 0,
	}
	for name, ok := range checks {
		if !ok {
			t.Errorf("%s", name)
		}
	}
	if len(notices) != 3 {
		t.Errorf("notices: %v", notices)
	}
	echoed := plan.Clone()
	echoed.PostInstallNotes = []string{"Keep backups."}
	if again, _ := MergeRepair(echoed, echoed.Clone()); len(again.PostInstallNotes) != 1 {
		t.Errorf("an echoed note was added again: %v", again.PostInstallNotes)
	}
	emptied := hostileProposal()
	emptied.Services[1].Healthcheck = Healthcheck{}
	emptied.Services[1].Ports = nil
	if kept, _ := MergeRepair(plan, emptied); !kept.Services[1].Healthcheck.Defined() {
		t.Error("a repair removed a healthcheck without a reason")
	}
}

func TestFromModelStampsEverything(t *testing.T) {
	proposal := hostileProposal()
	plan := FromModel(proposal)
	for _, service := range plan.Services {
		for _, provenance := range []Provenance{service.Provenance, service.Image.Provenance, service.Healthcheck.Provenance} {
			if provenance.Source != SourceModel || Rank(provenance.Confidence) > Rank(ConfidenceMedium) {
				t.Fatalf("%s provenance %+v", service.Name, provenance)
			}
		}
	}
	if token := entry(plan, "ADMIN_TOKEN"); !token.Secret || token.Value != "" || token.Generate == "" {
		t.Fatalf("model-written secret kept: %+v", token)
	}
	if plan.Needs[0].Provenance.Source != SourceModel || len(plan.Unfilled) != 0 {
		t.Fatalf("needs or unfilled: %+v %v", plan.Needs, plan.Unfilled)
	}
}

func TestFillableSkipsImagesAndConfidentValues(t *testing.T) {
	fields := strings.Join(Fillable(rulePlan(), ModelThreshold), ",")
	want := "app.summary,services.0.healthcheck,services.0.ports,env_file.BASE_URL.description,services.2.role,services.2.healthcheck"
	if fields != want {
		t.Fatalf("fillable %s, want %s", fields, want)
	}
}

func entry(plan *IR, key string) EnvFileEntry {
	found, _ := plan.EnvEntry(key)
	if found == nil {
		return EnvFileEntry{}
	}
	return *found
}
