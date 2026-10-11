package ir

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed schema.json
var schemaJSON []byte

const schemaURL = "composewright-ir-v1.json"

// enumKeys are the properties whose values are case-normalised before
// validation; providers may return "High" for an enum value "high".
var enumKeys = map[string]bool{
	"role": true, "protocol": true, "condition": true, "capability": true,
	"generate": true, "confidence": true, "source": true,
}

var compiledSchema = sync.OnceValues(func() (*jsonschema.Schema, error) {
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		return nil, err
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(schemaURL, document); err != nil {
		return nil, err
	}
	return compiler.Compile(schemaURL)
})

func SchemaJSON() []byte {
	return schemaJSON
}

// Decode parses an IR strictly: an unknown property anywhere is an error, so
// keys such as "privileged" or "network_mode" can never be smuggled in.
func Decode(data []byte) (*IR, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var plan IR
	if err := decoder.Decode(&plan); err != nil {
		return nil, fmt.Errorf("decode plan: %w", err)
	}
	if decoder.More() {
		return nil, errors.New("decode plan: trailing data after the JSON object")
	}
	plan.Normalize()
	return &plan, nil
}

// DecodeModelOutput validates untrusted model output against the JSON schema
// before decoding, so a missing field is rejected instead of zero-filled.
func DecodeModelOutput(data []byte) (*IR, error) {
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("model output is not JSON: %w", err)
	}
	lowercaseEnums(instance)
	schema, err := compiledSchema()
	if err != nil {
		return nil, err
	}
	if err := schema.Validate(instance); err != nil {
		return nil, fmt.Errorf("model output does not match the plan schema: %w", err)
	}
	normalised, err := json.Marshal(instance)
	if err != nil {
		return nil, err
	}
	return Decode(normalised)
}

// ValidateSchema checks a plan produced in Go against the same schema the
// model must satisfy; tests use it to keep types and schema in step.
func ValidateSchema(plan *IR) error {
	plan.Normalize()
	encoded, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	schema, err := compiledSchema()
	if err != nil {
		return err
	}
	return schema.Validate(instance)
}

func lowercaseEnums(node any) {
	switch value := node.(type) {
	case map[string]any:
		for key, child := range value {
			text, isString := child.(string)
			if isString && enumKeys[key] {
				value[key] = strings.ToLower(text)
				continue
			}
			lowercaseEnums(child)
		}
	case []any:
		for _, child := range value {
			lowercaseEnums(child)
		}
	}
}

// Normalize lower-cases enum values and service names and replaces nil
// slices with empty ones, because the schema (and therefore every provider)
// forbids null. Compose accepts capitals in a service name and the plan does
// not; Docker's DNS ignores case, so a service renamed this way is still
// reached by the spelling other services use.
func (plan *IR) Normalize() {
	plan.Services = nonNil(plan.Services)
	plan.EnvFile = nonNil(plan.EnvFile)
	plan.Needs = nonNil(plan.Needs)
	plan.PostInstallNotes = nonNil(plan.PostInstallNotes)
	plan.Evidence = nonNil(plan.Evidence)
	plan.SuspiciousInstructions = nonNil(plan.SuspiciousInstructions)
	plan.Unfilled = nonNil(plan.Unfilled)
	plan.Probe.Expect = nonNil(plan.Probe.Expect)
	plan.Probe.Service = strings.ToLower(plan.Probe.Service)
	plan.Confidence = strings.ToLower(plan.Confidence)
	for index := range plan.Services {
		plan.Services[index].normalize()
	}
	for index := range plan.EnvFile {
		entry := &plan.EnvFile[index]
		entry.Generate = strings.ToLower(entry.Generate)
		entry.Provenance.normalize()
	}
	for index := range plan.Needs {
		plan.Needs[index].Capability = strings.ToLower(plan.Needs[index].Capability)
		plan.Needs[index].Service = strings.ToLower(plan.Needs[index].Service)
		plan.Needs[index].Provenance.normalize()
	}
}

func (service *Service) normalize() {
	service.Name = strings.ToLower(service.Name)
	service.Role = strings.ToLower(service.Role)
	service.Command = nonNil(service.Command)
	service.Entrypoint = nonNil(service.Entrypoint)
	service.Env = nonNil(service.Env)
	service.Ports = nonNil(service.Ports)
	service.Volumes = nonNil(service.Volumes)
	service.DependsOn = nonNil(service.DependsOn)
	service.Notes = nonNil(service.Notes)
	service.Healthcheck.Test = nonNil(service.Healthcheck.Test)
	service.Image.Provenance.normalize()
	service.Healthcheck.Provenance.normalize()
	service.Provenance.normalize()
	for index := range service.Env {
		service.Env[index].Provenance.normalize()
	}
	for index := range service.Ports {
		service.Ports[index].Protocol = strings.ToLower(service.Ports[index].Protocol)
		service.Ports[index].Provenance.normalize()
	}
	for index := range service.Volumes {
		service.Volumes[index].Provenance.normalize()
	}
	for index := range service.DependsOn {
		service.DependsOn[index].Service = strings.ToLower(service.DependsOn[index].Service)
		service.DependsOn[index].Condition = strings.ToLower(service.DependsOn[index].Condition)
	}
}

// normalize gives an unset value the provenance of a default, so an empty
// healthcheck still carries a valid provenance and ranks as low confidence.
func (provenance *Provenance) normalize() {
	provenance.Source = strings.ToLower(provenance.Source)
	provenance.Confidence = strings.ToLower(provenance.Confidence)
	if provenance.Source == "" {
		provenance.Source = SourceDefault
	}
	if provenance.Confidence == "" {
		provenance.Confidence = ConfidenceLow
	}
}

func nonNil[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
