package fetch

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

const (
	maxDocumentBytes = 1 << 20
	maxCodeBytes     = 256 << 10
	maxYAMLBytes     = 256 << 10
)

// skippedDirs never contribute deployment facts and can be enormous.
var skippedDirs = []string{
	".git", "node_modules", "vendor", "third_party", "test", "tests", "__tests__", "testdata", "fixtures",
	"e2e", "spec", "dist", "target", ".devcontainer", "playwright", "cypress", ".idea", ".vscode", "locale", "locales", "i18n", "translations",
}

// docsDirectories hold documentation pages, at the root or in a monorepo
// package such as apps/docs.
var docsDirectories = []string{"docs", "doc"}

// translationDirectories are the language folders of a docs site, such as
// apps/docs/de or docs/zh-CN: they repeat the English pages, which are the
// ones read, so a translated description never wins.
var translationDirectories = []string{
	"ar", "bg", "bn", "ca", "cs", "da", "de", "el", "es", "et", "fa", "fi", "fr", "he", "hi", "hr", "hu", "id", "it", "ja", "ko",
	"lt", "lv", "ms", "nb", "nl", "no", "pl", "pt", "pt-br", "pt-pt", "ro", "ru", "sk", "sl", "sr", "sv", "th", "tr", "uk", "ur",
	"vi", "zh", "zh-cn", "zh-hans", "zh-hant", "zh-hk", "zh-tw",
}

var codeExtensions = []string{".go", ".js", ".mjs", ".cjs", ".ts", ".py", ".rs", ".java", ".kt", ".rb", ".php", ".cs", ".ex", ".exs"}

var stackHintFiles = []string{"package.json", "go.mod", "pyproject.toml", "Cargo.toml", "composer.json", "Gemfile", "requirements.txt"}

var (
	dockerfileName = regexp.MustCompile(`(?i)^(dockerfile|containerfile)([._-].*)?$|\.dockerfile$`)
	envSampleName  = regexp.MustCompile(`(?i)^(\.env(\..+)?|.+\.env|env\.(example|sample|template|dist)|example\.env)$`)
	exampleName    = regexp.MustCompile(`(?i)(^config.*\.example\..+$|\.(sample|example)$)`)
	readmeName     = regexp.MustCompile(`(?i)^readme(\.(md|markdown|rst|txt))?$`)
	historyName    = regexp.MustCompile(`(?i)^(changelog|changes|history|release[-_]?notes|migration).*\.(md|markdown)$`)
	testCodeName   = regexp.MustCompile(`(?i)(_test\.(go|py|rb|exs)|\.(test|spec)\.[cm]?[jt]sx?|^test_.*\.py|Tests?\.(java|kt|cs))$`)
	// EnvReference finds environment variable reads in common languages; a
	// code file is kept only when it contains one.
	EnvReference = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\(\s*"([A-Za-z_][A-Za-z0-9_]*)"|` +
		`process\.env\.([A-Za-z_][A-Za-z0-9_]*)|process\.env\[\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]\s*\]|` +
		`os\.(?:environ\.get|getenv)\(\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]|os\.environ\[\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]\s*\]|` +
		`env::var(?:_os)?\(\s*"([A-Za-z_][A-Za-z0-9_]*)"|System\.getenv\(\s*"([A-Za-z_][A-Za-z0-9_]*)"|` +
		`ENV(?:\.fetch\(|\[)\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]|getenv\(\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]|` +
		`\$_ENV\[\s*['"]([A-Za-z_][A-Za-z0-9_]*)['"]\s*\]|GetEnvironmentVariable\(\s*"([A-Za-z_][A-Za-z0-9_]*)"|` +
		`System\.get_env\(\s*"([A-Za-z_][A-Za-z0-9_]*)"`)
)

// Kind is why a file is kept; it decides the size limit.
type Kind int

const (
	Skip Kind = iota
	Document
	Config
	Code
)

// Classify decides from the path alone whether a repository file can carry
// deployment facts. Compose files are recognised by content, so every YAML
// file is a candidate.
func Classify(name string) Kind {
	name = path.Clean(strings.ReplaceAll(name, "\\", "/"))
	directories := strings.Split(path.Dir(name), "/")
	base := path.Base(name)
	skipped := slices.ContainsFunc(directories, func(directory string) bool { return slices.Contains(skippedDirs, strings.ToLower(directory)) })
	if skipped || isTranslation(directories) {
		return Skip
	}
	if isWorkflow(name) {
		return Config
	}
	if strings.HasPrefix(name, ".github/") {
		return Skip
	}
	switch {
	case historyName.MatchString(base):
		return Skip
	case readmeName.MatchString(base), isDocsMarkdown(directories, base):
		return Document
	case dockerfileName.MatchString(base), envSampleName.MatchString(base), exampleName.MatchString(base),
		slices.Contains(stackHintFiles, base), isYAML(base):
		return Config
	case slices.Contains(codeExtensions, strings.ToLower(path.Ext(base))) && !testCodeName.MatchString(base):
		return Code
	}
	return Skip
}

// Limit is the largest file of a kind worth reading.
func Limit(kind Kind) int64 {
	switch kind {
	case Document:
		return maxDocumentBytes
	case Code:
		return maxCodeBytes
	}
	return maxYAMLBytes
}

func isYAML(base string) bool {
	extension := strings.ToLower(path.Ext(base))
	return extension == ".yml" || extension == ".yaml"
}

func isWorkflow(name string) bool {
	return strings.HasPrefix(name, ".github/workflows/") && isYAML(path.Base(name))
}

func isDocsMarkdown(directories []string, base string) bool {
	extension := strings.ToLower(path.Ext(base))
	return slices.ContainsFunc(directories, isDocsDirectory) && (extension == ".md" || extension == ".markdown")
}

// isTranslation is true inside a language folder of a docs site.
func isTranslation(directories []string) bool {
	docs := slices.IndexFunc(directories, isDocsDirectory)
	return docs >= 0 && slices.ContainsFunc(directories[docs+1:], func(directory string) bool {
		return slices.Contains(translationDirectories, strings.ReplaceAll(strings.ToLower(directory), "_", "-"))
	})
}

func isDocsDirectory(directory string) bool {
	return slices.Contains(docsDirectories, strings.ToLower(directory))
}
