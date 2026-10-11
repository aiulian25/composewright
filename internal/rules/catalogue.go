package rules

import (
	_ "embed"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/aiulian25/composewright/internal/ir"
)

//go:embed catalogue.yaml
var catalogueYAML []byte

type family struct {
	Name     string   `yaml:"name"`
	Role     string   `yaml:"role"`
	Images   []string `yaml:"images"`
	DataPath string   `yaml:"data_path"`
	// DataPathSince is where newer versions of the image keep their data.
	DataPathSince *movedData     `yaml:"data_path_since"`
	Healthcheck   []string       `yaml:"healthcheck"`
	Sidecar       *addedDatabase `yaml:"sidecar"`
}

// addedDatabase is how R4 runs this database for an app that needs one.
type addedDatabase struct {
	Tag          string   `yaml:"tag"`
	Port         int      `yaml:"port"`
	Words        []string `yaml:"words"`
	User         string   `yaml:"user"`
	Password     string   `yaml:"password"`
	Database     string   `yaml:"database"`
	RootPassword string   `yaml:"root_password"`
}

// movedData is the data path an image moved to in a major version. A volume
// at the old path stays when UnlessEnv, such as PGDATA, says where the data
// goes.
type movedData struct {
	Major     int    `yaml:"major"`
	Path      string `yaml:"path"`
	UnlessEnv string `yaml:"unless_env"`
}

// pgMajor and leadingMajor read a major version from tags such as pg18,
// 0.8.0-pg17, 18.1-alpine and 17-3.5.
var (
	pgMajor      = regexp.MustCompile(`(?i)pg([0-9]+)`)
	leadingMajor = regexp.MustCompile(`^([0-9]+)`)
)

// dataPathOf is where this version of the image keeps its data.
func (candidate family) dataPathOf(image ir.Image) string {
	moved := candidate.DataPathSince
	if moved == nil || tagMajor(image.Tag) < moved.Major {
		return candidate.DataPath
	}
	return moved.Path
}

// tagMajor is the major version a tag names. A tag without one, such as
// latest or alpine, follows the newest release, so it counts as newer than
// any.
func tagMajor(tag string) int {
	for _, pattern := range []*regexp.Regexp{pgMajor, leadingMajor} {
		if match := pattern.FindStringSubmatch(tag); match != nil {
			major, _ := strconv.Atoi(match[1])
			return major
		}
	}
	return math.MaxInt
}

type knownApp struct {
	Images      []string `yaml:"images"`
	Healthcheck []string `yaml:"healthcheck"`
	// MemoryLimit is what the image needs to run, at least.
	MemoryLimit string `yaml:"memory_limit"`
}

type catalogue struct {
	Families []family   `yaml:"families"`
	Apps     []knownApp `yaml:"apps"`
}

var known = mustLoadCatalogue()

func mustLoadCatalogue() catalogue {
	var loaded catalogue
	if err := yaml.Unmarshal(catalogueYAML, &loaded); err != nil {
		panic(fmt.Sprintf("rules catalogue is invalid: %v", err))
	}
	return loaded
}

func (loaded catalogue) family(image ir.Image) (family, bool) {
	for _, candidate := range loaded.Families {
		if slices.Contains(candidate.Images, image.Reference()) {
			return candidate, true
		}
	}
	return family{}, false
}

// database is the family an app's database settings name: by a word of
// their prefix, such as MYSQL in MYSQL_HOST, else by the port they set.
func (loaded catalogue) database(prefix, port string) (family, bool) {
	words := strings.Split(prefix, "_")
	named := func(candidate family) bool {
		return candidate.Sidecar != nil && slices.ContainsFunc(words, func(word string) bool { return slices.Contains(candidate.Sidecar.Words, word) })
	}
	onPort := func(candidate family) bool {
		return candidate.Sidecar != nil && strconv.Itoa(candidate.Sidecar.Port) == port
	}
	for _, matches := range []func(family) bool{named, onPort} {
		if found := slices.IndexFunc(loaded.Families, matches); found >= 0 {
			return loaded.Families[found], true
		}
	}
	return family{}, false
}

func (loaded catalogue) appHealthcheck(image ir.Image) []string {
	return loaded.app(image).Healthcheck
}

func (loaded catalogue) appMemory(image ir.Image) string {
	return loaded.app(image).MemoryLimit
}

func (loaded catalogue) app(image ir.Image) knownApp {
	found := slices.IndexFunc(loaded.Apps, func(app knownApp) bool { return slices.Contains(app.Images, image.Reference()) })
	if found < 0 {
		return knownApp{}
	}
	return loaded.Apps[found]
}
