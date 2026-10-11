package cli

import (
	"bytes"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
)

var updateDocs = flag.Bool("update-docs", false, "write docs/cli.md from the command tree (make docs)")

const (
	cliReference = "../../docs/cli.md"
	cliHeader    = "# CLI reference\n\nGenerated from the command tree by `make docs`; edit the commands' help, not this file.\n\n"
	docsDirMode  = 0o755
	docsFileMode = 0o644
)

// TestCLIReferenceRenders renders every command's help as the CLI reference,
// so a command whose help cannot be rendered fails `make test`. docs/ stays
// out of the repository: only `make docs` writes the file.
func TestCLIReferenceRenders(t *testing.T) {
	root := (&app{stdout: io.Discard, stderr: io.Discard}).rootCommand()
	root.DisableAutoGenTag = true
	reference := markdownReference(t, root)
	if !*updateDocs {
		return
	}
	if err := os.MkdirAll(filepath.Dir(cliReference), docsDirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cliReference, reference, docsFileMode); err != nil {
		t.Fatal(err)
	}
}

// markdownReference renders every available command into one page, with
// links between commands turned into anchors.
func markdownReference(t *testing.T, root *cobra.Command) []byte {
	t.Helper()
	anchor := func(file string) string {
		return "#" + strings.ReplaceAll(strings.TrimSuffix(file, ".md"), "_", "-")
	}
	var reference bytes.Buffer
	reference.WriteString(cliHeader)
	for _, command := range allCommands(root) {
		if !command.IsAvailableCommand() && command != root {
			continue
		}
		if err := doc.GenMarkdownCustom(command, &reference, anchor); err != nil {
			t.Fatal(err)
		}
		reference.WriteString("\n")
	}
	return append(bytes.TrimRight(reference.Bytes(), "\n"), '\n')
}
