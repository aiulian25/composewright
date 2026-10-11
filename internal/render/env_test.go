package render

import (
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"

	"github.com/aiulian25/composewright/internal/ir"
)

func parseOne(t *testing.T, line string, known map[string]string) string {
	t.Helper()
	values, err := dotenv.ParseWithLookup(strings.NewReader(line), func(name string) (string, bool) {
		value, found := known[name]
		return value, found
	})
	if err != nil {
		t.Fatalf("parse %q: %v", line, err)
	}
	return values["KEY"]
}

func TestFormatValueRoundTrips(t *testing.T) {
	for _, value := range []string{"", "plain", "http://localhost:8080", "with space", "hash # sign", "a$b", "$HOME",
		`quote"d`, "it's", `back\slash`, `both'\"`, "trailing\\", "tab\there", "a=b", "3,33", "172.16.0.1/12 192.168.0.1/16", "${NOT_A_REF}"} {
		formatted := FormatValue(value, false)
		if got := parseOne(t, "KEY="+formatted+"\n", nil); got != value {
			t.Errorf("value %q written as %s read back as %q", value, formatted, got)
		}
	}
}

func TestFormatValueInterpolates(t *testing.T) {
	known := map[string]string{"POSTGRES_PASSWORD": "abc123"}
	formatted := FormatValue("postgres://u:${POSTGRES_PASSWORD}@db/x?sslmode=disable&a=$b", true)
	if got := parseOne(t, "KEY="+formatted+"\n", known); got != "postgres://u:abc123@db/x?sslmode=disable&a=$b" {
		t.Fatalf("interpolated value read back as %q (written %s)", got, formatted)
	}
}

func TestEnvOrdersReferencesFirst(t *testing.T) {
	plan := &ir.IR{EnvFile: []ir.EnvFileEntry{
		{Key: "DATABASE_URL", Value: "postgres://u:${POSTGRES_PASSWORD}@db/x", Secret: true},
		{Key: "POSTGRES_PASSWORD", Generate: ir.GenerateHex32, Secret: true, Description: "Database password"},
	}}
	values := map[string]string{"DATABASE_URL": "postgres://u:${POSTGRES_PASSWORD}@db/x", "POSTGRES_PASSWORD": "f00"}
	content, err := Env(plan, values, "# header\n")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	if strings.Index(text, "POSTGRES_PASSWORD=f00") > strings.Index(text, "DATABASE_URL=") {
		t.Fatalf("reference used before definition:\n%s", text)
	}
	parsed, err := dotenv.UnmarshalBytesWithLookup(content, nil)
	if err != nil || parsed["DATABASE_URL"] != "postgres://u:f00@db/x" {
		t.Fatalf("compose reads %v, %v", parsed, err)
	}
	if !strings.Contains(text, "# Database password\n# secret · generated (hex32)\n") {
		t.Fatalf("comments missing:\n%s", text)
	}
	looped := &ir.IR{EnvFile: []ir.EnvFileEntry{{Key: "A", Value: "${B}"}, {Key: "B", Value: "${A}"}}}
	if _, err := Env(looped, map[string]string{"A": "${B}", "B": "${A}"}, ""); err == nil {
		t.Fatal("reference cycle accepted")
	}
}

// FuzzFormatValue proves any single-line value survives the .env round trip.
func FuzzFormatValue(f *testing.F) {
	for _, seed := range []string{"plain", "a b", `x"'\$`, "# not a comment", "$${A}", "\\'"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if strings.ContainsAny(value, "\n\r\x00") || !isUTF8(value) {
			return
		}
		formatted := FormatValue(value, false)
		values, err := dotenv.ParseWithLookup(strings.NewReader("KEY="+formatted+"\n"), func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatalf("value %q written as %s does not parse: %v", value, formatted, err)
		}
		if values["KEY"] != value {
			t.Fatalf("value %q written as %s read back as %q", value, formatted, values["KEY"])
		}
	})
}

func isUTF8(value string) bool {
	return strings.ToValidUTF8(value, "�") == value
}
