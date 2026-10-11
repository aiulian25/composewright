package web

import (
	"math"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	textContrast      = 4.5
	nonTextContrast   = 3.0
	lightnessContrast = 1.25
)

// designPalettes is the web UI design plus the logo's amber; tokens.css
// must match it exactly. Two values are lighter or darker than the design
// where it failed WCAG text contrast.
var designPalettes = map[string]map[string]string{
	"dark": {"bg": "#26231E", "surface": "#1F1D19", "surface-2": "#181613", "raised": "#2E2A24", "border": "#36322B",
		"text": "#EEE8DE", "text-soft": "#B5AD9F", "text-muted": "#A0988C", "text-faint": "#6F685D",
		"accent": "#F2A93B", "accent-text": "#F2A93B", "on-accent": "#1E1A15", "accent-soft": "#3A2F1D",
		"ok": "#9CCF84", "ok-bg": "#2E3225", "warn": "#F2A93B", "warn-bg": "#3A2F1D", "danger": "#EF8270", "danger-bg": "#3B2D26",
		"danger-line": "#A35E51", "info": "#8DB9DD", "info-bg": "#2C2F30", "code-bg": "#141210", "focus": "#F2A93B", "brand": "#F2A93B"},
	"light": {"bg": "#F0EBE2", "surface": "#F8F5EF", "surface-2": "#E9E4DA", "raised": "#E6E0D5", "border": "#DCD5C8",
		"text": "#1E1A15", "text-soft": "#544C41", "text-muted": "#625A4E", "text-faint": "#8F8676",
		"accent": "#A15A00", "accent-text": "#8A4D00", "on-accent": "#FFFFFF", "accent-soft": "#EEE5D6",
		"ok": "#2E6E1E", "ok-bg": "#E3EEDB", "warn": "#8A4D00", "warn-bg": "#F5E8D2", "danger": "#951C15", "danger-bg": "#F7E3DE",
		"danger-line": "#C0705F", "info": "#2A6396", "info-bg": "#E5E7E7", "code-bg": "#1C1A17", "focus": "#A15A00", "brand": "#C27408"},
}

type pair struct {
	foreground, background string
	minimum                float64
}

// pairs are every foreground and background that meet in the UI: text at
// 4.5:1, focus rings, component boundaries and primary buttons at 3:1.
var pairs = []pair{
	{"text", "bg", textContrast}, {"text", "surface", textContrast}, {"text", "surface-2", textContrast},
	{"text", "raised", textContrast}, {"text", "accent-soft", textContrast}, {"text", "warn-soft", textContrast},
	{"text-soft", "bg", textContrast}, {"text-soft", "surface", textContrast}, {"text-soft", "surface-2", textContrast},
	{"text-muted", "bg", textContrast}, {"text-muted", "surface", textContrast}, {"text-muted", "surface-2", textContrast},
	{"text-muted", "raised", textContrast}, {"text-muted", "accent-soft", textContrast}, {"text-muted", "warn-soft", textContrast},
	{"accent-text", "bg", textContrast}, {"accent-text", "surface", textContrast}, {"accent-text", "accent-soft", textContrast},
	{"on-accent", "accent", textContrast},
	{"ok", "ok-bg", textContrast}, {"ok", "surface", textContrast},
	{"warn", "warn-bg", textContrast}, {"warn", "surface", textContrast},
	{"danger", "danger-bg", textContrast}, {"danger", "surface", textContrast},
	{"info", "info-bg", textContrast}, {"info", "surface", textContrast},
	{"code-text", "code-bg", textContrast}, {"code-muted", "code-bg", textContrast}, {"code-key", "code-bg", textContrast},
	{"code-name", "code-bg", textContrast}, {"code-value", "code-bg", textContrast}, {"code-alt", "code-bg", textContrast},
	{"code-danger", "code-bg", textContrast}, {"code-text", "code-line", textContrast}, {"code-text", "code-error", textContrast},
	{"code-text", "code-footer", textContrast},
	{"code-muted", "code-footer", textContrast},
	{"focus", "bg", nonTextContrast}, {"focus", "surface", nonTextContrast}, {"focus", "surface-2", nonTextContrast},
	{"input-border", "bg", nonTextContrast}, {"input-border", "surface", nonTextContrast},
	{"accent", "bg", nonTextContrast}, {"accent", "surface", nonTextContrast}, {"danger-line", "surface", nonTextContrast},
}

// decorative tokens draw hairlines, separators, gutters and the logo, which
// identify no component and WCAG exempts; text never uses them.
var decorative = map[string]bool{"border": true, "hairline": true, "text-faint": true, "code-border": true, "brand": true}

var (
	comment     = regexp.MustCompile(`(?s)/\*.*?\*/`)
	mediaRule   = regexp.MustCompile(`@media\s*\(prefers-color-scheme:\s*light\)\s*\{\s*([^{}]*)\{([^{}]*)\}\s*\}`)
	rule        = regexp.MustCompile(`([^{}]+)\{([^{}]*)\}`)
	declaration = regexp.MustCompile(`--([a-z0-9-]+)\s*:\s*([^;]+);`)
	colourMix   = regexp.MustCompile(`^color-mix\(in srgb, var\(--([a-z0-9-]+)\) (\d+)%, var\(--([a-z0-9-]+)\)\)$`)
)

type rgb [3]float64

func declarations(body string) map[string]string {
	values := map[string]string{}
	for _, match := range declaration.FindAllStringSubmatch(body, -1) {
		values[match[1]] = strings.TrimSpace(match[2])
	}
	return values
}

// stylesheet returns the dark, light, system-light and shared declarations.
func stylesheet(t *testing.T) (dark, light, systemLight, shared map[string]string) {
	t.Helper()
	content, err := Files.ReadFile("static/tokens.css")
	if err != nil {
		t.Fatal(err)
	}
	css := comment.ReplaceAllString(string(content), "")
	media := mediaRule.FindStringSubmatch(css)
	if media == nil || strings.TrimSpace(media[1]) != `:root[data-theme="system"]` {
		t.Fatal("no light palette for the system theme")
	}
	systemLight = declarations(media[2])
	for _, match := range rule.FindAllStringSubmatch(mediaRule.ReplaceAllString(css, ""), -1) {
		selector := strings.Join(strings.Fields(match[1]), " ")
		switch selector {
		case `:root[data-theme="dark"], :root[data-theme="system"]`:
			dark = declarations(match[2])
		case `:root[data-theme="light"]`:
			light = declarations(match[2])
		case `:root`:
			shared = declarations(match[2])
		default:
			t.Fatalf("unexpected rule %q in tokens.css", selector)
		}
	}
	return dark, light, systemLight, shared
}

func palette(t *testing.T, theme map[string]string, shared map[string]string) map[string]rgb {
	t.Helper()
	merged := map[string]string{}
	for _, values := range []map[string]string{shared, theme} {
		for name, value := range values {
			merged[name] = value
		}
	}
	colours := map[string]rgb{}
	for name := range merged {
		colours[name] = resolve(t, merged, name)
	}
	return colours
}

func resolve(t *testing.T, values map[string]string, name string) rgb {
	t.Helper()
	value := values[name]
	if mix := colourMix.FindStringSubmatch(value); mix != nil {
		share, _ := strconv.ParseFloat(mix[2], 64)
		first, second := resolve(t, values, mix[1]), resolve(t, values, mix[3])
		var mixed rgb
		for channel := range mixed {
			mixed[channel] = first[channel]*share/100 + second[channel]*(100-share)/100
		}
		return mixed
	}
	hex := strings.TrimPrefix(value, "#")
	if len(hex) != 6 {
		t.Fatalf("--%s: %q is neither #RRGGBB nor a colour-mix of tokens", name, value)
	}
	var colour rgb
	for channel := range colour {
		component, err := strconv.ParseUint(hex[channel*2:channel*2+2], 16, 8)
		if err != nil {
			t.Fatalf("--%s: %v", name, err)
		}
		colour[channel] = float64(component)
	}
	return colour
}

// luminance is WCAG 2.x relative luminance.
func luminance(colour rgb) float64 {
	var linear [3]float64
	for channel, value := range colour {
		srgb := value / 255
		linear[channel] = srgb / 12.92
		if srgb > 0.04045 {
			linear[channel] = math.Pow((srgb+0.055)/1.055, 2.4)
		}
	}
	return 0.2126*linear[0] + 0.7152*linear[1] + 0.0722*linear[2]
}

func contrast(first, second rgb) float64 {
	lighter, darker := luminance(first), luminance(second)
	if darker > lighter {
		lighter, darker = darker, lighter
	}
	return (lighter + 0.05) / (darker + 0.05)
}

func TestTokensMatchTheDesign(t *testing.T) {
	dark, light, systemLight, _ := stylesheet(t)
	for theme, declared := range map[string]map[string]string{"dark": dark, "light": light, "system on a light OS": systemLight} {
		want := designPalettes["light"]
		if theme == "dark" {
			want = designPalettes["dark"]
		}
		if len(declared) != len(want) {
			t.Errorf("%s declares %d tokens, the design has %d", theme, len(declared), len(want))
		}
		for name, value := range want {
			if !strings.EqualFold(declared[name], value) {
				t.Errorf("%s --%s is %q, the design says %s", theme, name, declared[name], value)
			}
		}
	}
}

func TestContrast(t *testing.T) {
	dark, light, _, shared := stylesheet(t)
	for theme, declared := range map[string]map[string]string{"dark": dark, "light": light} {
		colours := palette(t, declared, shared)
		checked := map[string]bool{}
		for _, check := range pairs {
			ratio := contrast(colours[check.foreground], colours[check.background])
			if ratio < check.minimum {
				t.Errorf("%s: --%s on --%s is %.2f:1, needs %.1f:1", theme, check.foreground, check.background, ratio, check.minimum)
			}
			checked[check.foreground], checked[check.background] = true, true
		}
		for name := range colours {
			if !checked[name] && !decorative[name] {
				t.Errorf("%s: --%s is in no checked pair; add one", theme, name)
			}
		}
	}
}

// TestStatesDifferInLightness: a failure must not look like a success or a
// warning by hue alone; templates still spell every state out.
func TestStatesDifferInLightness(t *testing.T) {
	dark, light, _, shared := stylesheet(t)
	for theme, declared := range map[string]map[string]string{"dark": dark, "light": light} {
		colours := palette(t, declared, shared)
		for _, other := range []string{"ok", "warn"} {
			if ratio := contrast(colours["danger"], colours[other]); ratio < lightnessContrast {
				t.Errorf("%s: --danger and --%s differ by %.2f:1 in lightness, needs %.2f:1", theme, other, ratio, lightnessContrast)
			}
		}
	}
}

func TestToggleKeepsStorageFailuresHarmless(t *testing.T) {
	script, err := Files.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	accesses := strings.Count(text, "window.localStorage.")
	guarded := len(regexp.MustCompile(`try\s*\{\s*(return\s+)?window\.localStorage\.`).FindAllString(text, -1))
	if accesses != 2 || guarded != accesses {
		t.Fatalf("%d localStorage accesses, %d directly inside a try block: each access needs its own", accesses, guarded)
	}
}
