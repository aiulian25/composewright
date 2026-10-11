package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"unicode"
)

// palette colours words that already carry the meaning on their own: a
// verdict is always printed as PASS/FAIL/warn/deny, colour only helps.
type palette struct {
	enabled bool
}

func newPalette(noColorFlag bool, output io.Writer) palette {
	if noColorFlag || os.Getenv(envNoColor) != "" {
		return palette{}
	}
	file, isFile := output.(*os.File)
	if !isFile {
		return palette{}
	}
	info, err := file.Stat()
	return palette{enabled: err == nil && info.Mode()&os.ModeCharDevice != 0}
}

func (colours palette) wrap(code, text string) string {
	if !colours.enabled {
		return text
	}
	return code + text + ansiReset
}

func (colours palette) ok(text string) string     { return colours.wrap(ansiGreen, text) }
func (colours palette) warn(text string) string   { return colours.wrap(ansiYellow, text) }
func (colours palette) danger(text string) string { return colours.wrap(ansiRed, text) }
func (colours palette) muted(text string) string  { return colours.wrap(ansiGray, text) }

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", jsonIndent)
	return encoder.Encode(value)
}

// table writes rows as aligned columns.
func table(output io.Writer, rows ...[]string) error {
	const padding = 2
	writer := tabwriter.NewWriter(output, 0, 0, padding, ' ', 0)
	for _, row := range rows {
		fmt.Fprintln(writer, strings.Join(row, "\t"))
	}
	return writer.Flush()
}

// sanitize strips control characters from text that may come from a
// repository or a model, so it cannot move the cursor or recolour the
// terminal when printed.
func sanitize(text string) string {
	return strings.Map(func(character rune) rune {
		if unicode.IsControl(character) && character != '\t' {
			return -1
		}
		return character
	}, text)
}
