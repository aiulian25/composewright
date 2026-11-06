package extract

import (
	"strconv"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/dotenv"
)

func TestDotenvValuesMatchCompose(t *testing.T) {
	for _, raw := range []string{
		"A=\"a\\\"b\"\n",
		"A='first\nsecond'\nB=tail\n",
		"A=\"first\nsecond\"\r\nB=tail\r\n",
		"A='literal$money'\nB=${A}\nA=last\nEMPTY=\nUNSET\n",
		"export A=first # comment\nB=\"x\\ny\"\nC=${A:-fallback}\n",
	} {
		want, err := dotenv.ParseWithLookup(strings.NewReader(raw), func(string) (string, bool) { return "", false })
		if err != nil {
			t.Fatal(err)
		}
		got := sampleValues(parseEnvSample(".env", []byte(raw)))
		if len(got) != len(want) {
			t.Errorf("keys: got %v want %v", got, want)
		}
		for key, value := range want {
			if got[key] != value {
				t.Errorf("%s = %q want %q", key, got[key], value)
			}
		}
	}
}

func TestDotenvEvidenceAndErrors(t *testing.T) {
	t.Setenv("CW_DOTENV_HOST_ONLY", "must-not-be-read")
	raw := "# Description of text\nTEXT='first\n# not a description\nlast' # inline\nNEXT=ok\nHOST=${CW_DOTENV_HOST_ONLY}\nBAD=\"dummy-private-marker\n"
	sample, problems := readEnvSample(".env", []byte(raw))
	if len(problems) != 1 || problems[0] != ".env:7: invalid dotenv statement" {
		t.Fatalf("diagnostics: %v", problems)
	}
	values := sampleValues(sample)
	if values["TEXT"] != "first\n# not a description\nlast" || values["HOST"] != "" {
		t.Fatalf("values: %v", values)
	}
	for _, entry := range sample.Entries {
		if entry.Key == "NEXT" && (entry.Location.Line != 5 || entry.Comment != "") {
			t.Fatalf("continuation leaked into evidence: %+v", entry)
		}
		if entry.Key == "TEXT" && (entry.Location.Line != 2 || entry.Comment != "inline") {
			t.Fatalf("span: %+v", entry)
		}
	}
}

func FuzzDotenvQuotedValueMatchesCompose(f *testing.F) {
	for _, value := range []string{"a\"b", "first\nsecond", "dollar$literal", "\r\n", "backslash\\"} {
		f.Add(value)
	}
	f.Fuzz(func(t *testing.T, value string) {
		if len(value) > 4096 {
			t.Skip()
		}
		raw := "VALUE=" + strconv.Quote(value) + "\nAFTER=tail\n"
		expected, err := dotenv.ParseWithLookup(strings.NewReader(raw), func(string) (string, bool) { return "", false })
		if err != nil {
			return
		}
		sample, problems := readEnvSample(".env", []byte(raw))
		if len(problems) > 0 {
			t.Fatalf("valid value rejected: %v", problems)
		}
		actual := sampleValues(sample)
		for key, want := range expected {
			if actual[key] != want {
				t.Fatalf("%s: got %q want %q", key, actual[key], want)
			}
		}
	})
}
