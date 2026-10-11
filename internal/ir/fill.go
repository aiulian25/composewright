package ir

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	fillSchemaURL  = "composewright-fill-v1.json"
	fillAnswerName = "fill"
)

// FillAnswer is a model's answer in an assisted run: only the fields such
// a run may merge. Asking for this instead of a whole plan keeps the answer
// short, which is what makes a small local model usable.
type FillAnswer struct {
	Summary                string        `json:"summary"`
	Services               []ServiceFill `json:"services"`
	EnvFile                []Description `json:"env_file"`
	PostInstallNotes       []string      `json:"post_install_notes"`
	SuspiciousInstructions []string      `json:"suspicious_instructions"`
	Confidence             string        `json:"confidence"`
}

type ServiceFill struct {
	Name        string      `json:"name"`
	Role        string      `json:"role"`
	Healthcheck Healthcheck `json:"healthcheck"`
	Ports       []Port      `json:"ports"`
}

type Description struct {
	Key         string `json:"key"`
	Description string `json:"description"`
}

// planSchemaParts are the plan schema's definitions, top-level properties and
// service properties: the answer schemas are built from them, so they
// cannot drift apart from the plan.
func planSchemaParts() (definitions, properties, serviceProperties map[string]any, err error) {
	var plan map[string]any
	if err := json.Unmarshal(schemaJSON, &plan); err != nil {
		return nil, nil, nil, err
	}
	definitions, _ = plan["$defs"].(map[string]any)
	properties, _ = plan["properties"].(map[string]any)
	service, _ := definitions["service"].(map[string]any)
	serviceProperties, _ = service["properties"].(map[string]any)
	return definitions, properties, serviceProperties, nil
}

// strictObject is an object in the strict form every provider accepts:
// each property required and no other allowed.
func strictObject(description string, fields map[string]any) map[string]any {
	required := make([]string, 0, len(fields))
	for name := range fields {
		required = append(required, name)
	}
	slices.Sort(required)
	return map[string]any{"type": "object", "description": description, "properties": fields, "required": required,
		"additionalProperties": false}
}

// compileAnswerSchema compiles an answer schema to check model output with.
func compileAnswerSchema(url string, content []byte) (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(content))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(url, document); err != nil {
		return nil, err
	}
	return compiler.Compile(url)
}

// decodeAnswer checks untrusted model output against an answer schema, then
// decodes it strictly into answer.
func decodeAnswer(data []byte, schema *jsonschema.Schema, name string, answer any) error {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("model output is not JSON: %w", err)
	}
	lowercaseEnums(instance)
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("model output does not match the %s schema: %w", name, err)
	}
	normalised, err := json.Marshal(instance)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(normalised))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(answer); err != nil {
		return fmt.Errorf("decode %s answer: %w", name, err)
	}
	return nil
}

// FillSchemaJSON is the JSON Schema of a FillAnswer.
var FillSchemaJSON = sync.OnceValues(func() ([]byte, error) {
	definitions, properties, serviceProperties, err := planSchemaParts()
	if err != nil {
		return nil, err
	}
	fill := strictObject("The fields of RULE_PLAN listed in FILL, filled from FACTS and REPO_CONTENT. Leave a value empty when no source supports it.", map[string]any{
		"summary":                 map[string]any{"type": "string", "description": "One sentence on what the app is, or empty."},
		"services":                map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/service_fill"}},
		"env_file":                map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/description"}},
		"post_install_notes":      properties["post_install_notes"],
		"suspicious_instructions": properties["suspicious_instructions"],
		"confidence":              properties["confidence"],
	})
	fill["title"] = "composewright fill answer v1"
	fill["$defs"] = map[string]any{
		"provenance":  definitions["provenance"],
		"port":        definitions["port"],
		"healthcheck": definitions["healthcheck"],
		"service_fill": strictObject("A service of RULE_PLAN, named as there.", map[string]any{
			"name":        serviceProperties["name"],
			"role":        serviceProperties["role"],
			"healthcheck": map[string]any{"$ref": "#/$defs/healthcheck"},
			"ports":       map[string]any{"type": "array", "items": map[string]any{"$ref": "#/$defs/port"}},
		}),
		"description": strictObject("What a variable of RULE_PLAN's env_file is for.", map[string]any{
			"key":         map[string]any{"type": "string", "description": "The variable's name exactly as in RULE_PLAN."},
			"description": map[string]any{"type": "string", "description": "One plain sentence, or empty."},
		}),
	}
	return json.Marshal(fill)
})

var compiledFillSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	content, err := FillSchemaJSON()
	if err != nil {
		return nil, err
	}
	return compileAnswerSchema(fillSchemaURL, content)
})

// DecodeFill checks untrusted model output against the fill schema, then
// decodes it strictly.
func DecodeFill(data []byte) (*FillAnswer, error) {
	schema, err := compiledFillSchema()
	if err != nil {
		return nil, err
	}
	var answer FillAnswer
	if err := decodeAnswer(data, schema, fillAnswerName, &answer); err != nil {
		return nil, err
	}
	return &answer, nil
}

// Proposal lays the answer over the plan so Merge can apply it with the
// usual precedence. It proposes no needs, images or services of its own;
// services and variables the plan does not have are ignored.
func (answer *FillAnswer) Proposal(plan *IR) *IR {
	proposal := plan.Clone()
	proposal.App.Summary = answer.Summary
	proposal.Needs = []Need{}
	proposal.PostInstallNotes, proposal.SuspiciousInstructions = answer.PostInstallNotes, answer.SuspiciousInstructions
	proposal.Confidence = answer.Confidence
	for _, offered := range answer.Services {
		service, found := proposal.Service(offered.Name)
		if !found {
			continue
		}
		service.Role, service.Healthcheck = offered.Role, offered.Healthcheck
		if len(offered.Ports) > 0 {
			service.Ports = offered.Ports
		}
	}
	for _, described := range answer.EnvFile {
		if entry, found := proposal.EnvEntry(described.Key); found {
			entry.Description = described.Description
		}
	}
	proposal.Normalize()
	return proposal
}
