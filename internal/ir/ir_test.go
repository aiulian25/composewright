package ir

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func loadSchema(t *testing.T) map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(SchemaJSON(), &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func jsonFields(goType reflect.Type) []string {
	var names []string
	for index := 0; index < goType.NumField(); index++ {
		name, _, _ := strings.Cut(goType.Field(index).Tag.Get("json"), ",")
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func schemaProperties(object map[string]any) []string {
	var names []string
	for name := range object["properties"].(map[string]any) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func TestGoTypesMatchSchema(t *testing.T) {
	schema := loadSchema(t)
	definitions := schema["$defs"].(map[string]any)
	app := schema["properties"].(map[string]any)["app"].(map[string]any)
	pairs := map[string]struct {
		goType reflect.Type
		object map[string]any
	}{
		"root": {reflect.TypeFor[IR](), schema},
		"app":  {reflect.TypeFor[App](), app},
	}
	for name, goType := range map[string]reflect.Type{
		"provenance": reflect.TypeFor[Provenance](), "image": reflect.TypeFor[Image](), "env_ref": reflect.TypeFor[EnvRef](),
		"port": reflect.TypeFor[Port](), "volume": reflect.TypeFor[Volume](), "dependency": reflect.TypeFor[Dependency](),
		"healthcheck": reflect.TypeFor[Healthcheck](), "resources": reflect.TypeFor[Resources](), "service": reflect.TypeFor[Service](),
		"env_file_entry": reflect.TypeFor[EnvFileEntry](), "need": reflect.TypeFor[Need](), "probe": reflect.TypeFor[Probe](),
		"evidence": reflect.TypeFor[Evidence](),
	} {
		pairs[name] = struct {
			goType reflect.Type
			object map[string]any
		}{goType, definitions[name].(map[string]any)}
	}
	if len(pairs) != len(definitions)+2 {
		t.Fatalf("schema has %d definitions, test maps %d", len(definitions), len(pairs)-2)
	}
	for name, pair := range pairs {
		if got, want := jsonFields(pair.goType), schemaProperties(pair.object); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: Go fields %v, schema properties %v", name, got, want)
		}
	}
}

// TestSchemaIsProviderCompatible walks the schema and enforces the strictest
// intersection of Anthropic, OpenAI strict mode and Ollama.
func TestSchemaIsProviderCompatible(t *testing.T) {
	checkProviderCompatible(t, loadSchema(t))
	for _, answerSchema := range []func() ([]byte, error){FillSchemaJSON, RepairSchemaJSON} {
		content, err := answerSchema()
		if err != nil {
			t.Fatal(err)
		}
		var answer map[string]any
		if err := json.Unmarshal(content, &answer); err != nil {
			t.Fatal(err)
		}
		checkProviderCompatible(t, answer)
	}
}

// checkProviderCompatible walks a schema for what Anthropic, OpenAI strict
// mode and Ollama's grammars reject.
func checkProviderCompatible(t *testing.T, schema map[string]any) {
	t.Helper()
	const maxDepth, maxProperties = 10, 5000
	forbidden := []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf", "minLength", "maxLength",
		"pattern", "format", "minItems", "maxItems", "uniqueItems", "anyOf", "oneOf", "allOf", "not", "if", "then", "else"}
	properties := 0
	var walk func(path string, node map[string]any, depth int)
	walk = func(path string, node map[string]any, depth int) {
		if depth > maxDepth {
			t.Errorf("%s nests deeper than %d", path, maxDepth)
		}
		for _, keyword := range forbidden {
			if _, found := node[keyword]; found {
				t.Errorf("%s uses unsupported keyword %q", path, keyword)
			}
		}
		if _, isList := node["type"].([]any); isList {
			t.Errorf("%s uses a union type", path)
		}
		if node["type"] == "null" {
			t.Errorf("%s allows null", path)
		}
		if reference, found := node["$ref"].(string); found && reference == "#" {
			t.Errorf("%s is recursive", path)
		}
		if node["type"] == "object" {
			if node["additionalProperties"] != false {
				t.Errorf("%s lacks additionalProperties: false", path)
			}
			required := map[string]bool{}
			for _, name := range node["required"].([]any) {
				required[name.(string)] = true
			}
			for name, child := range node["properties"].(map[string]any) {
				properties++
				if !required[name] {
					t.Errorf("%s.%s is not required", path, name)
				}
				walk(path+"."+name, child.(map[string]any), depth+1)
			}
		}
		if items, found := node["items"].(map[string]any); found {
			walk(path+"[]", items, depth+1)
		}
	}
	walk("root", schema, 0)
	for name, definition := range schema["$defs"].(map[string]any) {
		walk("$defs."+name, definition.(map[string]any), 1)
	}
	if properties > maxProperties {
		t.Errorf("schema has %d properties, limit %d", properties, maxProperties)
	}
}

func minimalPlan() *IR {
	provenance := Provenance{Source: SourceCompose, SourceRef: "compose.yaml:3", Confidence: ConfidenceHigh}
	return &IR{
		IRVersion: Version, App: App{Name: "demo", Summary: "A demo app."}, Confidence: ConfidenceHigh,
		Services: []Service{{
			Name: "demo", Role: RoleApp, Provenance: provenance,
			Image: Image{Registry: "docker.io", Repository: "library/nginx", Tag: "1.27", Provenance: provenance},
		}},
	}
}

func TestPlansProducedInGoValidateAgainstSchema(t *testing.T) {
	if err := ValidateSchema(minimalPlan()); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeModelOutput(t *testing.T) {
	plan := minimalPlan()
	plan.Normalize()
	valid, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	shouted := strings.Replace(string(valid), `"confidence":"high"`, `"confidence":"High"`, 1)
	decoded, err := DecodeModelOutput([]byte(shouted))
	if err != nil {
		t.Fatalf("valid output rejected: %v", err)
	}
	if decoded.Confidence != ConfidenceHigh {
		t.Fatalf("enum not normalised: %q", decoded.Confidence)
	}
	missing := strings.Replace(string(valid), `,"unfilled":[]`, ``, 1)
	if !strings.Contains(string(valid), `"unfilled":[]`) || missing == string(valid) {
		t.Fatal("fixture does not contain the field the test removes")
	}
	if _, err := DecodeModelOutput([]byte(missing)); err == nil {
		t.Fatal("output with a missing field accepted")
	}
	smuggled := strings.Replace(string(valid), `"role":"app"`, `"role":"app","privileged":true`, 1)
	if _, err := DecodeModelOutput([]byte(smuggled)); err == nil {
		t.Fatal("output with an extra field accepted")
	}
	if _, err := Decode([]byte(smuggled)); err == nil {
		t.Fatal("strict decoder accepted an unknown field")
	}
	if _, err := Decode(append(valid, []byte(`{}`)...)); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestParseImage(t *testing.T) {
	cases := map[string]string{
		"postgres:17":                         "docker.io/library/postgres:17",
		"miniflux/miniflux:2.2.3":             "docker.io/miniflux/miniflux:2.2.3",
		"ghcr.io/paperless-ngx/paperless-ngx": "ghcr.io/paperless-ngx/paperless-ngx:",
		"index.docker.io/library/redis:7":     "docker.io/library/redis:7",
		"lscr.io/linuxserver/freshrss:latest": "lscr.io/linuxserver/freshrss:latest",
		"nginx@sha256:0000000000000000000000000000000000000000000000000000000000000000": "docker.io/library/nginx:@sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}
	for input, want := range cases {
		image, err := ParseImage(input)
		if err != nil {
			t.Fatalf("%s: %v", input, err)
		}
		if got := image.String(); got != want {
			t.Errorf("ParseImage(%q) = %q, want %q", input, got, want)
		}
	}
	if _, err := ParseImage("UPPER/Case"); err == nil {
		t.Fatal("invalid reference accepted")
	}
}

func TestRank(t *testing.T) {
	ordered := []string{ConfidenceHigh, ConfidenceMedium, ConfidenceLow, ""}
	for index := 1; index < len(ordered); index++ {
		if Rank(ordered[index-1]) <= Rank(ordered[index]) {
			t.Fatalf("Rank(%q) must exceed Rank(%q)", ordered[index-1], ordered[index])
		}
	}
}
