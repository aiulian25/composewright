package extract

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/aiulian25/composewright/internal/fetch"
)

const (
	maxFiles = 5000
	// maxUnreadableNamed is how many unreadable files a problem names.
	maxUnreadableNamed = 5
)

// suspiciousText catches repository text addressed to a language model. It is
// reported to the user; nothing ever acts on it.
var suspiciousText = regexp.MustCompile(`(?i)(ignore (all |any |the )?(previous|prior|above|earlier) (instructions|prompts?)|disregard (all |any |the )?(previous|prior|above) |you are (an? )?(ai|assistant|language model|llm)\b|system prompt|as an ai model)`)

type sourceFile struct {
	relative string
	kind     fetch.Kind
	content  []byte
}

// Extract reads the repository at directory. Files are read through os.Root,
// so a symlink inside a local checkout cannot point extraction elsewhere.
func Extract(ctx context.Context, directory string) (*Facts, error) {
	files, unreadable, err := readFiles(directory)
	if err != nil {
		return nil, err
	}
	facts := &Facts{}
	if len(unreadable) > 0 {
		facts.Problems = append(facts.Problems, unreadableProblem(unreadable))
	}
	var mentions []Mention
	samples := map[string]map[string]string{}
	for _, file := range files {
		facts.Files = append(facts.Files, file.relative)
		if !isEnvSample(file.relative) {
			continue
		}
		sample := parseEnvSample(file.relative, file.content)
		facts.EnvSamples = append(facts.EnvSamples, sample)
		mentions = append(mentions, sampleMentions(sample)...)
		samples[path.Dir(file.relative)] = sampleValues(sample)
	}
	for _, file := range files {
		mentions = append(mentions, facts.read(ctx, file, samples)...)
	}
	facts.Variables = mergeVariables(mentions)
	facts.Images = dedupeImages(facts.Images)
	return facts, nil
}

func (facts *Facts) read(ctx context.Context, file sourceFile, samples map[string]map[string]string) []Mention {
	base := path.Base(file.relative)
	switch {
	case isEnvSample(file.relative):
		return nil
	case file.kind == fetch.Document:
		return facts.readMarkdown(file)
	case file.kind == fetch.Code:
		return codeMentions(file.relative, file.content)
	case isDockerfile(base):
		dockerfile := parseDockerfile(file.relative, file.content)
		facts.Dockerfiles = append(facts.Dockerfiles, dockerfile)
		return dockerfileMentions(dockerfile)
	case slices.Contains([]string{"package.json", "go.mod", "pyproject.toml", "Cargo.toml", "composer.json", "Gemfile", "requirements.txt"}, base):
		facts.StackHints = append(facts.StackHints, file.relative)
		return nil
	case strings.HasPrefix(file.relative, ".github/workflows/"):
		facts.Images = append(facts.Images, workflowImages(file.relative, file.content)...)
		return nil
	case isHelmValues(file.relative):
		return helmMentions(file.relative, file.content)
	case isYAMLFile(base):
		return facts.readCompose(ctx, file, samples)
	}
	return nil
}

func (facts *Facts) readMarkdown(file sourceFile) []Mention {
	scan := scanMarkdown(file.relative, file.content)
	facts.Images = append(facts.Images, scan.images...)
	facts.DockerRuns = append(facts.DockerRuns, scan.dockerRuns...)
	facts.Notes = append(facts.Notes, scan.notes...)
	facts.FileMentions = append(facts.FileMentions, scan.fileMentions...)
	if isReadme(file.relative) {
		facts.Readmes = append(facts.Readmes, file.relative)
	}
	if file.relative == facts.rootReadme() && facts.Summary.Text == "" {
		facts.Summary = scan.summary
	}
	for index, line := range strings.Split(string(file.content), "\n") {
		if match := suspiciousText.FindString(line); match != "" {
			facts.Suspicious = append(facts.Suspicious, Note{Text: strings.TrimSpace(line), Location: Location{File: file.relative, Line: index + 1}})
		}
	}
	return scan.mentions
}

func (facts *Facts) readCompose(ctx context.Context, file sourceFile, samples map[string]map[string]string) []Mention {
	interpolation := map[string]string{}
	for _, directory := range []string{".", path.Dir(file.relative)} {
		for name, value := range samples[directory] {
			interpolation[name] = value
		}
	}
	compose, mentions, err := extractCompose(ctx, file.relative, file.content, interpolation)
	if errors.Is(err, errNotCompose) {
		return nil
	}
	if err != nil {
		facts.Problems = append(facts.Problems, err.Error())
		return mentions
	}
	facts.ComposeFiles = append(facts.ComposeFiles, compose)
	for _, service := range compose.Services {
		if service.Image != "" && !strings.Contains(service.Image, "$") {
			facts.Images = append(facts.Images, ImageMention{Reference: service.Image, Kind: KindCompose, Location: service.ImageAt})
		}
	}
	return mentions
}

func dockerfileMentions(dockerfile Dockerfile) []Mention {
	mentions := make([]Mention, 0, len(dockerfile.Env))
	for _, variable := range dockerfile.Env {
		mentions = append(mentions, Mention{Name: variable.Key, Kind: KindDockerfile, Default: variable.Value, Location: variable.Location})
	}
	return mentions
}

func sampleValues(sample EnvSample) map[string]string {
	values := map[string]string{}
	for _, entry := range sample.Entries {
		if !entry.Commented {
			values[entry.Key] = entry.Value
		}
	}
	return values
}

// readFiles lists the files fact extraction reads, applying the same rules
// as tarball extraction so a local directory and a fetched repository give
// the same facts. Files and folders it may not read, such as a mounted
// checkout's 0600 files in the container, are left out and listed.
func readFiles(directory string) ([]sourceFile, []string, error) {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, nil, err
	}
	defer root.Close()
	var files []sourceFile
	var unreadable []string
	leaveOut := func(name string) error {
		unreadable = append(unreadable, filepath.ToSlash(name))
		return nil
	}
	err = fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrPermission) {
			return leaveOut(name)
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return skipDirectory(name)
		}
		kind := fetch.Classify(name)
		if kind == fetch.Skip || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > fetch.Limit(kind) {
			return err
		}
		content, err := root.ReadFile(name)
		if errors.Is(err, fs.ErrPermission) {
			return leaveOut(name)
		}
		if err != nil {
			return err
		}
		if kind == fetch.Code && !fetch.EnvReference.Match(content) {
			return nil
		}
		if len(files) >= maxFiles {
			return fetch.ErrTooLarge
		}
		files = append(files, sourceFile{relative: filepath.ToSlash(name), kind: kind, content: content})
		return nil
	})
	sort.Slice(files, func(left, right int) bool { return files[left].relative < files[right].relative })
	return files, unreadable, err
}

// unreadableProblem names the first files composewright may not read.
func unreadableProblem(unreadable []string) string {
	named := strings.Join(unreadable[:min(len(unreadable), maxUnreadableNamed)], ", ")
	if extra := len(unreadable) - maxUnreadableNamed; extra > 0 {
		named += fmt.Sprintf(" and %d more", extra)
	}
	return "permission denied, left out: " + named
}

func skipDirectory(name string) error {
	if name != "." && fetch.Classify(name+"/probe.yml") == fetch.Skip {
		return fs.SkipDir
	}
	return nil
}

// rootReadme is the README at the repository root, preferring Markdown.
func (facts *Facts) rootReadme() string {
	for _, name := range facts.Files {
		if !strings.Contains(name, "/") && isReadme(name) {
			return name
		}
	}
	return ""
}

func isEnvSample(relative string) bool {
	base := strings.ToLower(path.Base(relative))
	return strings.HasPrefix(base, ".env") || strings.HasSuffix(base, ".env") || strings.HasPrefix(base, "env.") ||
		base == "example.env" || strings.Contains(base, ".env.")
}

func isDockerfile(base string) bool {
	lower := strings.ToLower(base)
	return strings.HasPrefix(lower, "dockerfile") || strings.HasPrefix(lower, "containerfile") || strings.HasSuffix(lower, ".dockerfile")
}

func isYAMLFile(base string) bool {
	extension := strings.ToLower(path.Ext(base))
	return extension == ".yml" || extension == ".yaml"
}

func isReadme(relative string) bool {
	return strings.HasPrefix(strings.ToLower(path.Base(relative)), "readme")
}

func dedupeImages(images []ImageMention) []ImageMention {
	seen := map[string]bool{}
	var unique []ImageMention
	for _, image := range images {
		key := image.Kind + " " + image.Reference
		if seen[key] {
			continue
		}
		seen[key] = true
		unique = append(unique, image)
	}
	return unique
}
