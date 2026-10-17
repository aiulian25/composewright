package fetch

import (
	"archive/zip"
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const zipCommit = "c52bdef6e9811f7c20cebc034a6c0ec6799dc6df"

type zipEntry struct {
	name string
	body string
	mode fs.FileMode
}

func zipArchive(t testing.TB, entries ...zipEntry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	if err := writer.SetComment(zipCommit); err != nil {
		t.Fatal(err)
	}
	for _, item := range entries {
		header := &zip.FileHeader{Name: item.name, Method: zip.Deflate}
		header.SetMode(item.mode | 0o644)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(item.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func unzipBytes(content []byte, destination string) (Extracted, error) {
	return Unzip(bytes.NewReader(content), int64(len(content)), destination)
}

func TestUnzipKeepsOnlyDeploymentFiles(t *testing.T) {
	destination := t.TempDir()
	content := zipArchive(t,
		zipEntry{name: "v2-main/", mode: fs.ModeDir},
		zipEntry{name: "v2-main/README.md", body: "# app"},
		zipEntry{name: "v2-main/docker/", mode: fs.ModeDir},
		zipEntry{name: "v2-main/docker/Dockerfile", body: "FROM alpine"},
		zipEntry{name: "v2-main/main.go", body: `package main; func main() { _ = os.Getenv("PORT") }`},
		zipEntry{name: "v2-main/util.go", body: "package main"},
		zipEntry{name: "v2-main/logo.png", body: "png"},
		zipEntry{name: "v2-main/compose.yaml", body: "/etc/passwd", mode: fs.ModeSymlink},
	)
	result, err := unzipBytes(content, destination)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"README.md", "docker/Dockerfile", "main.go"}
	if strings.Join(result.Kept, ",") != strings.Join(want, ",") {
		t.Fatalf("kept %v, want %v", result.Kept, want)
	}
	if result.RootName != "v2-main" || result.Commit != zipCommit || result.Skipped != 3 {
		t.Fatalf("metadata %+v", result)
	}
	if _, err := os.Lstat(filepath.Join(destination, "compose.yaml")); err == nil {
		t.Fatal("the symlink was written")
	}
	info, err := os.Stat(filepath.Join(destination, "docker", "Dockerfile"))
	if err != nil || info.Mode().Perm() != keptFileMode {
		t.Fatalf("Dockerfile mode %v %v", info, err)
	}
}

func TestUnzipRejectsEscapingPaths(t *testing.T) {
	cases := map[string][]zipEntry{
		"dot-dot":   {{name: "repo/README.md", body: "x"}, {name: "repo/../../etc/passwd/README.md", body: "x"}},
		"slip":      {{name: "../etc/README.md", body: "x"}},
		"absolute":  {{name: "/etc/README.md", body: "x"}},
		"backslash": {{name: "repo/..\\..\\README.md", body: "x"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			outside := t.TempDir()
			destination := filepath.Join(outside, "inside")
			if err := os.Mkdir(destination, keptDirMode); err != nil {
				t.Fatal(err)
			}
			if _, err := unzipBytes(zipArchive(t, entries...), destination); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("escaping archive: got %v, want ErrUnsafePath", err)
			}
			_ = filepath.WalkDir(outside, func(path string, entry fs.DirEntry, err error) error {
				if err == nil && !entry.IsDir() && !strings.HasPrefix(path, destination+string(filepath.Separator)) {
					t.Errorf("file written outside the destination: %s", path)
				}
				return nil
			})
		})
	}
}

func TestUnzipRejectsOversizedArchives(t *testing.T) {
	var many []zipEntry
	for index := 0; index <= maxKeptFiles; index++ {
		many = append(many, zipEntry{name: "repo/d/" + itoa(index) + ".yml", body: "a: 1"})
	}
	if _, err := unzipBytes(zipArchive(t, many...), t.TempDir()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too many kept files: got %v", err)
	}
	if _, err := Unzip(bytes.NewReader(nil), MaxArchiveBytes+1, t.TempDir()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("past the archive cap: got %v", err)
	}
	big := zipEntry{name: "repo/README.md", body: strings.Repeat("x", int(Limit(Document))+1)}
	result, err := unzipBytes(zipArchive(t, big), t.TempDir())
	if err != nil || len(result.Kept) != 0 || result.Skipped != 1 {
		t.Fatalf("a file past its kind's limit: %+v %v", result, err)
	}
	if _, err := unzipBytes([]byte("not a zip"), t.TempDir()); err == nil {
		t.Fatal("garbage accepted")
	}
}

// TestUnzipFolders: Finder's __MACOSX folder is ignored, and an archive
// without one folder for the repository says so plainly.
func TestUnzipFolders(t *testing.T) {
	finder := zipArchive(t,
		zipEntry{name: "app/README.md", body: "# app"},
		zipEntry{name: "__MACOSX/app/._README.md", body: "fork"},
	)
	result, err := unzipBytes(finder, t.TempDir())
	if err != nil || strings.Join(result.Kept, ",") != "README.md" || result.RootName != "app" {
		t.Fatalf("a Finder zip: %+v %v", result, err)
	}
	cases := map[string][]zipEntry{
		"no folder":       {{name: "README.md", body: "x"}, {name: "compose.yaml", body: "x"}},
		"several folders": {{name: "repo/README.md", body: "x"}, {name: "other/README.md", body: "x"}},
	}
	for name, entries := range cases {
		if _, err := unzipBytes(zipArchive(t, entries...), t.TempDir()); !errors.Is(err, ErrSeveralFolders) {
			t.Fatalf("%s: got %v, want ErrSeveralFolders", name, err)
		}
	}
}

// TestLocalUnpacksArchives: a .zip or .tar.gz path is a source like a
// folder, unpacked into a temporary folder that Close removes.
func TestLocalUnpacksArchives(t *testing.T) {
	folder := t.TempDir()
	archives := map[string][]byte{
		"app-main.zip":    zipArchive(t, zipEntry{name: "app-main/README.md", body: "# app"}, zipEntry{name: "app-main/logo.png", body: "png"}),
		"app-main.tar.gz": archive(t, entry{name: "app-main/README.md", body: "# app"}, entry{name: "app-main/logo.png", body: "png"}),
	}
	for name, content := range archives {
		path := filepath.Join(folder, name)
		if err := os.WriteFile(path, content, keptFileMode); err != nil {
			t.Fatal(err)
		}
		source, err := Local(path, "https://github.com/acme/app")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(source.Directory, "README.md")); err != nil {
			t.Fatalf("%s: README not unpacked: %v", name, err)
		}
		if source.Note != "unpacked 2 files, kept 1" || source.Commit == "" || source.Owner != "acme" || source.Kind != SourceLocal {
			t.Fatalf("%s: source %+v", name, source)
		}
		if err := source.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(source.Directory); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: the unpacked folder was kept: %v", name, err)
		}
	}
	other := filepath.Join(folder, "app.7z")
	if err := os.WriteFile(other, []byte("7z"), keptFileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := Local(other, ""); !errors.Is(err, ErrUnsupportedSource) || !strings.Contains(err.Error(), ".tar.gz") {
		t.Fatalf("a .7z: got %v", err)
	}
}

// FuzzUnzip feeds mutated zip archives to the extractor: it may reject them
// but must never panic or write outside the destination.
func FuzzUnzip(f *testing.F) {
	f.Add(zipArchive(f, zipEntry{name: "repo/README.md", body: "# x"}))
	f.Add(zipArchive(f, zipEntry{name: "repo/../evil/README.md", body: "x"}))
	f.Add(zipArchive(f, zipEntry{name: "repo/link", body: "../../x", mode: fs.ModeSymlink}))
	f.Fuzz(func(t *testing.T, data []byte) {
		outside := t.TempDir()
		destination := filepath.Join(outside, "inside")
		if err := os.Mkdir(destination, keptDirMode); err != nil {
			t.Fatal(err)
		}
		_, _ = unzipBytes(data, destination)
		entries, _ := os.ReadDir(outside)
		if len(entries) != 1 {
			t.Fatalf("archive wrote outside the destination: %v", entries)
		}
	})
}
