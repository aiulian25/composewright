package export

import (
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aiulian25/composewright/internal/config"
	"github.com/aiulian25/composewright/internal/ir"
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

// TestShellWritesEveryFileAsItIs: run by sh under a permissive or a strict
// umask, the commands write every file of the bundle with exactly its bytes
// and mode, DEPLOY.md and a path with a space included, and nothing in a
// file runs: not quotes, dollars, backticks, a line that ended the old
// heredocs, nor a missing final newline.
func TestShellWritesEveryFileAsItIs(t *testing.T) {
	hostile := "a'b\"c $HOME `touch ran-backtick` $(touch ran-dollar)\nCOMPOSEWRIGHT_EOF\ntouch ran-delimiter\n"
	written := bundle()
	written.Compose = []byte("services: {}\nCOMPOSEWRIGHT_EOF\ntouch ran-compose\n")
	written.Env = []byte("SMTP_PASSWORD=" + hostile)
	written.Secrets = map[string][]byte{"secrets/db_password": []byte(hostile + "no final newline")}
	written.Config = map[string][]byte{"config/my app.conf": []byte("listen 80;\n")}
	want := map[string]struct {
		content []byte
		mode    fs.FileMode
	}{
		ComposeName: {written.Compose, fileMode}, EnvName: {written.Env, fileMode}, DeployName: {written.Deploy, fileMode},
		"secrets/db_password": {written.Secrets["secrets/db_password"], fileMode}, "config/my app.conf": {written.Config["config/my app.conf"], ir.ConfigFileMode},
	}
	for _, umask := range []string{"000", "022", "077"} {
		directory := t.TempDir()
		command := exec.Command("sh", "-c", "umask "+umask+" && sh")
		command.Dir, command.Stdin = directory, strings.NewReader(string(Shell(written)))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("umask %s: %v: %s", umask, err, output)
		}
		for name, expected := range want {
			content, err := os.ReadFile(filepath.Join(directory, name))
			info, statErr := os.Stat(filepath.Join(directory, name))
			if err != nil || statErr != nil {
				t.Errorf("umask %s: %s was not written: %v", umask, name, errors.Join(err, statErr))
				continue
			}
			if string(content) != string(expected.content) || info.Mode().Perm() != expected.mode {
				t.Errorf("umask %s: %s is %q (%v), want %q %v", umask, name, content, info.Mode().Perm(), expected.content, expected.mode)
			}
		}
		entries, _ := os.ReadDir(directory)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "ran-") {
				t.Errorf("umask %s: file content ran as a command: %s", umask, entry.Name())
			}
		}
	}
	if strings.Contains(string(Shell(bundle())), "secrets") {
		t.Error("a bundle without secret files makes a secrets folder")
	}
}

// TestDeployCommandQuotesTheFolder: a folder with spaces, quotes or shell
// characters is entered as it is, and nothing in its name runs.
func TestDeployCommandQuotesTheFolder(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "my app's $(touch ran) ; `x`")
	if err := os.Mkdir(directory, dirMode); err != nil {
		t.Fatal(err)
	}
	entered := strings.TrimSuffix(DeployCommand(directory), " && docker compose up -d") + " && pwd"
	output, err := exec.Command("sh", "-c", entered).Output()
	if err != nil || strings.TrimSpace(string(output)) != directory {
		t.Fatalf("entered %q (%v), want %q", output, err, directory)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(directory), "ran")); err == nil {
		t.Fatal("the folder name ran as a command")
	}
}

// TestWriteStaysInTheChosenFolder: a folder in the destination that is a
// link out of it, or a dangling one, refuses the export before anything is
// written, there or where it leads; a link to another folder inside the
// destination, and a destination that is itself a link, as a mount can be,
// still work.
func TestWriteStaysInTheChosenFolder(t *testing.T) {
	withFiles := bundle()
	withFiles.Secrets = map[string][]byte{"secrets/db_password": []byte("s3cret")}
	withFiles.Config = map[string][]byte{"config/app.conf": []byte("listen 80;\n")}
	for _, linked := range []string{"config", "secrets"} {
		for _, dangling := range []bool{false, true} {
			destination, outside := t.TempDir(), t.TempDir()
			pointed := outside
			if dangling {
				pointed = filepath.Join(outside, "missing")
			}
			if err := os.Symlink(pointed, filepath.Join(destination, linked)); err != nil {
				t.Fatal(err)
			}
			for _, overwrite := range []bool{false, true} {
				if _, err := Write(destination, config.Default().Profile, withFiles, overwrite); err == nil || !strings.Contains(err.Error(), linked) {
					t.Fatalf("%s linked out (dangling %v, overwrite %v): %v", linked, dangling, overwrite, err)
				}
			}
			for _, folder := range []string{destination, outside} {
				entries, _ := os.ReadDir(folder)
				if len(entries) > 1 || (folder == outside && len(entries) != 0) {
					t.Fatalf("%s linked out: a refused export wrote into %s: %v", linked, folder, entries)
				}
			}
		}
	}
	destination := t.TempDir()
	if err := os.Mkdir(filepath.Join(destination, "shared"), dirMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("shared", filepath.Join(destination, "config")); err != nil {
		t.Fatal(err)
	}
	mounted := filepath.Join(t.TempDir(), "mounted")
	if err := os.Symlink(destination, mounted); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(mounted, config.Default().Profile, withFiles, false); err != nil {
		t.Fatalf("a link inside the folder, or a linked folder: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(destination, "shared", "app.conf")); err != nil || string(content) != "listen 80;\n" {
		t.Fatalf("the config file: %q %v", content, err)
	}
}

// TestWriteCommitsTheWholeBundleOrNothing: a save that fails at any point,
// while staging or while putting files in place, leaves the folder holding
// the earlier bundle as it was; a file that appears after the check is
// never replaced without overwrite; a save that works leaves no staging
// behind and keeps unrelated files; and two saves at once never mix.
func TestWriteCommitsTheWholeBundleOrNothing(t *testing.T) {
	profile := config.Default().Profile
	older, newer := bundle(), bundle()
	newer.Compose, newer.Env, newer.Deploy = []byte("services: {new: {}}\n"), []byte("PASSWORD=n3w\n"), []byte("# new\n")
	newer.Secrets = map[string][]byte{"secrets/db_password": []byte("n3w")}
	saved := func(directory string) map[string]string {
		t.Helper()
		contents := map[string]string{}
		err := filepath.WalkDir(directory, func(name string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			content, readErr := os.ReadFile(name)
			relative, _ := filepath.Rel(directory, name)
			contents[relative] = string(content)
			return readErr
		})
		if err != nil {
			t.Fatal(err)
		}
		return contents
	}
	directory := t.TempDir()
	if _, err := Write(directory, profile, older, false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("mine"), fileMode); err != nil {
		t.Fatal(err)
	}
	before := saved(directory)
	blocked := newer
	blocked.Config = map[string][]byte{"compose.yaml/inner.conf": []byte("x")}
	if _, err := Write(directory, profile, blocked, true); err == nil {
		t.Fatal("a bundle that cannot be staged was saved")
	}
	if after := saved(directory); !maps.Equal(after, before) {
		t.Fatalf("a staging failure changed the folder: %v", after)
	}
	if err := os.Remove(filepath.Join(directory, DeployName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, DeployName), dirMode); err != nil {
		t.Fatal(err)
	}
	before = saved(directory)
	if _, err := Write(directory, profile, newer, true); err == nil || !strings.Contains(err.Error(), "nothing in the folder was changed") {
		t.Fatalf("a commit failing at DEPLOY.md: %v", err)
	}
	if after := saved(directory); !maps.Equal(after, before) {
		t.Fatalf("a failed commit left a mixed bundle: %v", after)
	}
	if err := os.Remove(filepath.Join(directory, DeployName)); err != nil {
		t.Fatal(err)
	}
	if _, err := Write(directory, profile, newer, true); err != nil {
		t.Fatal(err)
	}
	if after := saved(directory); after[ComposeName] != string(newer.Compose) || after["notes.txt"] != "mine" || len(after) != 5 {
		t.Fatalf("an overwrite: %v", after)
	}
	if info, err := os.Stat(filepath.Join(directory, EnvName)); err != nil || info.Mode().Perm() != fileMode {
		t.Fatalf(".env after an overwrite: %v", err)
	}

	racing := t.TempDir()
	root, err := os.OpenRoot(racing)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files := map[string]file{ComposeName: {newer.Compose, fileMode}, EnvName: {newer.Env, fileMode}}
	names := []string{ComposeName, EnvName}
	staged, err := stage(root, names, files)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(racing, EnvName), []byte("theirs"), fileMode); err != nil {
		t.Fatal(err)
	}
	if err := commit(root, names, staged, false); !errors.Is(err, ErrExists) {
		t.Fatalf("a file that appeared after the check: %v", err)
	}
	if err := removeStaged(root, staged); err != nil {
		t.Fatal(err)
	}
	if after := saved(racing); len(after) != 1 || after[EnvName] != "theirs" {
		t.Fatalf("a refused save left %v", after)
	}

	concurrent := t.TempDir()
	done := make(chan error, 2)
	for _, each := range []Bundle{older, newer} {
		go func() {
			_, err := Write(concurrent, profile, each, true)
			done <- err
		}()
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	final := saved(concurrent)
	fromOlder := final[ComposeName] == string(older.Compose) && final[EnvName] == string(older.Env) && final[DeployName] == string(older.Deploy)
	fromNewer := final[ComposeName] == string(newer.Compose) && final[EnvName] == string(newer.Env) && final[DeployName] == string(newer.Deploy)
	if !fromOlder && !fromNewer {
		t.Fatalf("two saves at once mixed their files: %v", final)
	}
}
