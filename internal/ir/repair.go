package ir

import (
	"encoding/json"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	repairSchemaURL  = "composewright-repair-v1.json"
	repairAnswerName = "repair"
)

// repairServiceFields are the service fields a repair may change, as
// MergeRepair applies them.
var repairServiceFields = []string{"command", "entrypoint", "env", "ports", "depends_on", "healthcheck"}

// RepairAnswer is a model's answer after a failed sandbox run: only what
// MergeRepair may apply. The whole plan's schema is too large for some
// providers to compile into a grammar; this one stays as small as the fill.
type RepairAnswer struct {
	Services         []ServiceRepair `json:"services"`
	EnvFile          []EnvFileEntry  `json:"env_file"`
	PostInstallNotes []string        `json:"post_install_notes"`
	Confidence       string          `json:"confidence"`
}

// ServiceRepair is a service of the plan as a repair would leave it.
type ServiceRepair struct {
	Name        string       `json:"name"`
	Command     []string     `json:"command"`
	Entrypoint  []string     `json:"entrypoint"`
	Env         []EnvRef     `json:"env"`
	Ports       []Port       `json:"ports"`
	DependsOn   []Dependency `json:"depends_on"`
	Healthcheck Healthcheck  `json:"healthcheck"`
}

// RepairSchemaJSON is the JSON Schema of a RepairAnswer.
var RepairSchemaJSON = sync.OnceValues(func() ([]byte, error) {
	definitions, properties, serviceProperties, err := planSchemaParts()
	if err != nil {
		return nil, err
	}
	fields := map[string]any{"name": serviceProperties["name"]}
	for _, name := range repairServiceFields {
		fields[name] = serviceProperties[name]
	}
	repair := strictObject("What must change in CURRENT_PLAN for the next run to pass. Services left out stay as they are.", map[string]any{
		"services":           map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/service_repair"}},
		"env_file":           properties["env_file"],
		"post_install_notes": properties["post_install_notes"],
		"confidence":         properties["confidence"],
	})
	repair["title"] = "composewright repair answer v1"
	repair["$defs"] = map[string]any{
		"provenance":     definitions["provenance"],
		"env_ref":        definitions["env_ref"],
		"port":           definitions["port"],
		"dependency":     definitions["dependency"],
		"healthcheck":    definitions["healthcheck"],
		"env_file_entry": definitions["env_file_entry"],
		"service_repair": strictObject("A service of CURRENT_PLAN, named as there, with every field as it should be after the repair.", fields),
	}
	return json.Marshal(repair)
})

var compiledRepairSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	content, err := RepairSchemaJSON()
	if err != nil {
		return nil, err
	}
	return compileAnswerSchema(repairSchemaURL, content)
})

// DecodeRepair checks untrusted model output against the repair schema, then
// decodes it strictly.
func DecodeRepair(data []byte) (*RepairAnswer, error) {
	schema, err := compiledRepairSchema()
	if err != nil {
		return nil, err
	}
	var answer RepairAnswer
	if err := decodeAnswer(data, schema, repairAnswerName, &answer); err != nil {
		return nil, err
	}
	return &answer, nil
}

// Proposal lays the answer over the plan for MergeRepair: the services it
// names take its fields, its env_file entries are offered as they are, and it
// proposes no needs, images or services of its own.
func (answer *RepairAnswer) Proposal(plan *IR) *IR {
	proposal := plan.Clone()
	proposal.Needs = []Need{}
	proposal.EnvFile = answer.EnvFile
	proposal.PostInstallNotes, proposal.SuspiciousInstructions = answer.PostInstallNotes, []string{}
	proposal.Confidence = answer.Confidence
	for _, offered := range answer.Services {
		service, found := proposal.Service(offered.Name)
		if !found {
			continue
		}
		service.Command, service.Entrypoint, service.Env = offered.Command, offered.Entrypoint, offered.Env
		service.Ports, service.DependsOn, service.Healthcheck = offered.Ports, offered.DependsOn, offered.Healthcheck
	}
	proposal.Normalize()
	return proposal
}
