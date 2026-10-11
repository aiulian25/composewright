package fetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type entry struct {
	name     string
	body     string
	typeflag byte
	link     string
}

func archive(t testing.TB, entries ...entry) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	global := &tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header",
		PAXRecords: map[string]string{"comment": "c52bdef6e9811f7c20cebc034a6c0ec6799dc6df"}}
	if err := writer.WriteHeader(global); err != nil {
		t.Fatal(err)
	}
	for _, item := range entries {
		typeflag := item.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{Name: item.name, Typeflag: typeflag, Mode: 0o644, Size: int64(len(item.body)), Linkname: item.link}
		if typeflag != tar.TypeReg {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if typeflag == tar.TypeReg {
			if _, err := writer.Write([]byte(item.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestUntarKeepsOnlyDeploymentFiles(t *testing.T) {
	destination := t.TempDir()
	content := archive(t,
		entry{name: "repo-abc/", typeflag: tar.TypeDir},
		entry{name: "repo-abc/README.md", body: "# app"},
		entry{name: "repo-abc/docker-compose.yml", body: "services: {}"},
		entry{name: "repo-abc/docker/Dockerfile", body: "FROM alpine"},
		entry{name: "repo-abc/.env.example", body: "A=1"},
		entry{name: "repo-abc/main.go", body: `package main; func main() { _ = os.Getenv("PORT") }`},
		entry{name: "repo-abc/util.go", body: "package main"},
		entry{name: "repo-abc/logo.png", body: "png"},
		entry{name: "repo-abc/node_modules/x/README.md", body: "noise"},
		entry{name: "repo-abc/docs/CHANGELOG.md", body: "noise"},
		entry{name: "repo-abc/link", typeflag: tar.TypeSymlink, link: "/etc/passwd"},
		entry{name: "repo-abc/hard", typeflag: tar.TypeLink, link: "repo-abc/README.md"},
		entry{name: "repo-abc/device", typeflag: tar.TypeChar},
	)
	result, err := Untar(bytes.NewReader(content), destination)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"README.md", "docker-compose.yml", "docker/Dockerfile", ".env.example", "main.go"}
	if strings.Join(result.Kept, ",") != strings.Join(want, ",") {
		t.Fatalf("kept %v, want %v", result.Kept, want)
	}
	if result.Commit != "c52bdef6e9811f7c20cebc034a6c0ec6799dc6df" || result.RootName != "repo-abc" {
		t.Fatalf("metadata %+v", result)
	}
	for _, name := range []string{"link", "hard", "device", "util.go", "logo.png"} {
		if _, err := os.Lstat(filepath.Join(destination, name)); err == nil {
			t.Errorf("%s was written", name)
		}
	}
	info, err := os.Stat(filepath.Join(destination, "README.md"))
	if err != nil || info.Mode().Perm() != keptFileMode {
		t.Fatalf("README mode %v %v", info.Mode(), err)
	}
}

func TestUntarRejectsEscapingPaths(t *testing.T) {
	cases := map[string][]entry{
		"dot-dot":           {{name: "repo/README.md", body: "x"}, {name: "repo/../../etc/README.md", body: "x"}},
		"absolute":          {{name: "/etc/README.md", body: "x"}},
		"second top level":  {{name: "repo/README.md", body: "x"}, {name: "other/README.md", body: "x"}},
		"backslash":         {{name: "repo/..\\..\\README.md", body: "x"}},
		"nested dot-dot":    {{name: "repo/docs/../../../x/README.md", body: "x"}},
		"windows drive":     {{name: "repo/C:/README.md", body: "x"}},
		"leading dot slash": {{name: "./repo/README.md", body: "x"}, {name: "./repo/../README.md", body: "x"}},
	}
	for name, entries := range cases {
		t.Run(name, func(t *testing.T) {
			outside := t.TempDir()
			destination := filepath.Join(outside, "inside")
			if err := os.Mkdir(destination, keptDirMode); err != nil {
				t.Fatal(err)
			}
			_, err := Untar(bytes.NewReader(archive(t, entries...)), destination)
			if err == nil && name != "windows drive" {
				t.Fatal("escaping archive accepted")
			}
			matches, _ := filepath.Glob(filepath.Join(outside, "*", "README.md"))
			for _, match := range matches {
				if !strings.HasPrefix(match, destination) {
					t.Fatalf("file written outside the destination: %s", match)
				}
			}
			if _, err := os.Stat(filepath.Join(outside, "README.md")); err == nil {
				t.Fatal("file written into the parent directory")
			}
		})
	}
}

func TestUntarRejectsOversizedArchives(t *testing.T) {
	var huge []entry
	for index := 0; index <= maxKeptFiles; index++ {
		huge = append(huge, entry{name: "repo/" + strings.Repeat("d", 3) + "/" + string(rune('a'+index%26)) + strings.Repeat("x", index%50) + "-" + itoa(index) + ".yml", body: "a: 1"})
	}
	if _, err := Untar(bytes.NewReader(archive(t, huge...)), t.TempDir()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too many files: got %v", err)
	}
	limited := &countingReader{reader: bytes.NewReader(make([]byte, 64)), limit: 16}
	buffer := make([]byte, 64)
	if _, err := limited.Read(buffer); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expansion limit: got %v", err)
	}
	if _, err := Untar(strings.NewReader("not gzip"), t.TempDir()); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestUntarSaysTooLargePastTheDownloadCap(t *testing.T) {
	noise := make([]byte, 64<<10)
	if _, err := rand.Read(noise); err != nil {
		t.Fatal(err)
	}
	compressed := archive(t, entry{name: "repo/assets/sample.wav", body: string(noise)}, entry{name: "repo/README.md", body: "# app"})
	if _, err := untar(bytes.NewReader(compressed), t.TempDir(), int64(len(compressed)/2)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an archive past the download cap: got %v, want ErrTooLarge", err)
	}
}

func itoa(value int) string {
	digits := []byte{}
	for {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
		if value == 0 {
			return string(digits)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]Kind{
		"README.md": Document, "docs/setup.md": Document, "contrib/docker-compose/README.md": Document,
		"docs/changelog.md": Skip, "compose.yaml": Config, "deploy/stack.yml": Config, ".github/workflows/build.yml": Config,
		".github/ISSUE_TEMPLATE/bug.yml": Skip, "Dockerfile": Config, "docker/Dockerfile.alpine": Config,
		"docker/default.Dockerfile": Config, ".env.sample": Config, ".env.template": Config, "example.env": Config,
		"config.example.toml": Config, "src/main.go": Code, "src/main_test.go": Skip, "web/app.spec.ts": Skip, "node_modules/a/README.md": Skip, "test/compose.yml": Skip,
		"logo.png": Skip, "package.json": Config,
	}
	for name, want := range cases {
		if got := Classify(name); got != want {
			t.Errorf("Classify(%q) = %d, want %d", name, got, want)
		}
	}
}

// FuzzUntar feeds mutated archives to the extractor: it may reject them but
// must never panic or write outside the destination.
func FuzzUntar(f *testing.F) {
	f.Add(archive(f, entry{name: "repo/README.md", body: "# x"}))
	f.Add(archive(f, entry{name: "repo/../evil/README.md", body: "x"}))
	f.Add(archive(f, entry{name: "repo/link", typeflag: tar.TypeSymlink, link: "../../x"}))
	f.Fuzz(func(t *testing.T, data []byte) {
		outside := t.TempDir()
		destination := filepath.Join(outside, "inside")
		if err := os.Mkdir(destination, keptDirMode); err != nil {
			t.Fatal(err)
		}
		_, _ = Untar(bytes.NewReader(data), destination)
		entries, _ := os.ReadDir(outside)
		if len(entries) != 1 {
			t.Fatalf("archive wrote outside the destination: %v", entries)
		}
	})
}
