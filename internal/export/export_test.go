package export

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
)

func bundle() Bundle {
	return Bundle{Project: "miniflux", Compose: []byte("services: {}\n"), Env: []byte("PASSWORD=s3cret\n"),
		EnvExample: []byte("PASSWORD=\n"), Deploy: []byte("# miniflux\n")}
}

func TestWritePlainFolder(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "miniflux")
	written, err := Write(directory, config.Default().Profile, bundle(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(written.Files) != 3 || !written.ContainsSecret {
		t.Fatalf("written %+v", written)
	}
	for _, name := range []string{ComposeName, EnvName, DeployName} {
		info, err := os.Stat(filepath.Join(directory, name))
		if err != nil || info.Mode().Perm() != fileMode {
			t.Fatalf("%s: %v %v", name, info, err)
		}
	}
	if _, err := Write(directory, config.Default().Profile, bundle(), false); !errors.Is(err, ErrExists) {
		t.Fatalf("overwrite without --overwrite: %v", err)
	}
	if _, err := Write(directory, config.Default().Profile, bundle(), true); err != nil {
		t.Fatalf("--overwrite refused: %v", err)
	}
}

func TestWriteArcaneTemplateHasNoSecrets(t *testing.T) {
	templates := t.TempDir()
	written, err := Write("arcane:"+templates, config.Default().Profile, bundle(), false)
	if err != nil {
		t.Fatal(err)
	}
	if written.ContainsSecret || written.Directory != filepath.Join(templates, "miniflux") {
		t.Fatalf("written %+v", written)
	}
	if _, err := os.Stat(filepath.Join(templates, "miniflux", EnvName)); err == nil {
		t.Fatal("a template contains .env with secrets")
	}
	content, _ := os.ReadFile(filepath.Join(templates, "miniflux", EnvExampleName))
	if string(content) != "PASSWORD=\n" {
		t.Fatalf(".env.example %q", content)
	}
}

func TestDefaultDestinationUsesTheProfile(t *testing.T) {
	profile := config.Default().Profile
	if _, err := Write("", profile, bundle(), false); err == nil {
		t.Fatal("exported without a destination")
	}
	profile.Storage.BasePath = t.TempDir()
	written, err := Write("", profile, bundle(), false)
	if err != nil || written.Directory != filepath.Join(profile.Storage.BasePath, "miniflux") {
		t.Fatalf("profile save_dir not used: %+v %v", written, err)
	}
}

// TestWriteRefusesBeforeWritingAnything: when any file the export would
// write exists already, nothing is written, so a new compose.yaml never ends
// up beside an old .env.
func TestWriteRefusesBeforeWritingAnything(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, EnvName), []byte("PASSWORD=old\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(directory, config.Default().Profile, bundle(), false); !errors.Is(err, ErrExists) {
		t.Fatalf("an existing .env: %v, want ErrExists", err)
	}
	if _, err := os.Stat(filepath.Join(directory, ComposeName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("compose.yaml was written beside the old .env: %v", err)
	}
}

// TestWriteRefusesAnExistingSecretFile: a secret file left by an earlier
// export stops the export before anything is written.
func TestWriteRefusesAnExistingSecretFile(t *testing.T) {
	directory := t.TempDir()
	if err := os.MkdirAll(filepath.Join(directory, "secrets"), dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "secrets", "db_password"), []byte("old"), fileMode); err != nil {
		t.Fatal(err)
	}
	withSecret := bundle()
	withSecret.Secrets = map[string][]byte{"secrets/db_password": []byte("new")}
	if _, err := Write(directory, config.Default().Profile, withSecret, false); !errors.Is(err, ErrExists) {
		t.Fatalf("an existing secret file: %v, want ErrExists", err)
	}
	if _, err := os.Stat(filepath.Join(directory, ComposeName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("compose.yaml was written beside the old secret file: %v", err)
	}
}

// TestShellWritesEveryFileAsItIs: the commands write compose.yaml and .env
// through heredocs and each secret with printf, quoted so the shell keeps a
// quote in it, without a newline, and set the modes an export gives them.
func TestShellWritesEveryFileAsItIs(t *testing.T) {
	withSecret := bundle()
	withSecret.Secrets = map[string][]byte{"secrets/db_password": []byte("it's")}
	script := string(Shell(withSecret))
	for _, want := range []string{"cat > compose.yaml <<'COMPOSEWRIGHT_EOF'\n", "cat > .env <<'COMPOSEWRIGHT_EOF'\n", "chmod 600 .env\n",
		"mkdir -p -m 700 secrets\n", `printf '%s' 'it'\''s' > secrets/db_password` + "\nchmod 600 secrets/db_password\n"} {
		if !strings.Contains(script, want) {
			t.Errorf("the commands lack %q:\n%s", want, script)
		}
	}
	if strings.Contains(string(Shell(bundle())), "secrets") {
		t.Error("a bundle without secret files makes a secrets folder")
	}
}
