package rules

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/docker/go-units"
	"go.yaml.in/yaml/v3"

	"github.com/aiulian25/composewright/internal/composefile"
	"github.com/aiulian25/composewright/internal/ir"
)

// problemSeparator joins an override file's name to what is wrong with it.
const problemSeparator = ": "

//go:embed catalogue.yaml
var catalogueYAML []byte

var (
	errNoImages         = errors.New("lists no images")
	errEmptyHealthcheck = errors.New("healthcheck is an empty list")
	errUnnamedFamily    = errors.New("a family has no name")
)

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

// check refuses a family the rules cannot use: one without a name, a known
// role or images that parse, with an empty healthcheck, or with a sidecar
// that lacks an image tag or a port. It writes each image as the rules
// compare them.
func (candidate *family) check() error {
	images, err := references(candidate.Images)
	switch {
	case candidate.Name == "":
		return errUnnamedFamily
	case !slices.Contains(ir.Roles, candidate.Role):
		return fmt.Errorf("family %s: role %q is not one of %s", candidate.Name, candidate.Role, strings.Join(ir.Roles, ", "))
	case err != nil:
		return fmt.Errorf("family %s: %w", candidate.Name, err)
	case candidate.Healthcheck != nil && len(candidate.Healthcheck) == 0:
		return fmt.Errorf("family %s: %w", candidate.Name, errEmptyHealthcheck)
	case candidate.Sidecar != nil && !candidate.Sidecar.runs(images[0]):
		return fmt.Errorf("family %s: the sidecar needs a port and a tag for %s", candidate.Name, images[0])
	}
	candidate.Images = images
	return nil
}

// runs reports whether R4 can start this database from image.
func (setup addedDatabase) runs(image string) bool {
	_, err := ir.ParseImage(image + ir.TagSeparator + setup.Tag)
	return err == nil && setup.Port > 0 && setup.Port <= composefile.MaxPort
}

type knownApp struct {
	Images      []string `yaml:"images"`
	Healthcheck []string `yaml:"healthcheck"`
	// MemoryLimit is what the image needs to run, at least.
	MemoryLimit string `yaml:"memory_limit"`
	// ProbePath answers a healthy app with a success, when the healthcheck
	// does not name it.
	ProbePath string `yaml:"probe_path"`
}

// check refuses an app entry the rules cannot use: one without images that
// parse, with a memory limit that is not a size or with an empty
// healthcheck. It writes each image as the rules compare them.
func (app *knownApp) check() error {
	images, err := references(app.Images)
	if err != nil {
		return err
	}
	_, memoryErr := units.RAMInBytes(app.MemoryLimit)
	switch {
	case app.MemoryLimit != "" && memoryErr != nil:
		return fmt.Errorf("app %s: memory_limit %q is not a size such as 512m", images[0], app.MemoryLimit)
	case app.Healthcheck != nil && len(app.Healthcheck) == 0:
		return fmt.Errorf("app %s: %w", images[0], errEmptyHealthcheck)
	}
	app.Images = images
	return nil
}

// references writes images as the rules compare them, by registry and
// repository: postgres and postgres:17 are docker.io/library/postgres.
func references(images []string) ([]string, error) {
	if len(images) == 0 {
		return nil, errNoImages
	}
	written := make([]string, 0, len(images))
	for _, text := range images {
		image, err := ir.ParseImage(text)
		if err != nil {
			return nil, err
		}
		written = append(written, image.Reference())
	}
	return written, nil
}

// Catalogue is what the rules know of images: the families of databases,
// caches and proxies, and the apps that need a healthcheck, memory or a
// probe path. The first entry that names an image wins.
type Catalogue struct {
	Families []family   `yaml:"families"`
	Apps     []knownApp `yaml:"apps"`
	// Overrides are the override files merged into it, by name; Problems
	// say why any other was left out, each after the file's name.
	Overrides []Override `yaml:"-"`
	Problems  []string   `yaml:"-"`
}

// Override is an override file merged into a catalogue, with how many
// families and apps it adds.
type Override struct {
	Name     string
	Families int
	Apps     int
}

// shipped is the catalogue built into composewright.
var shipped = mustLoadCatalogue()

func mustLoadCatalogue() *Catalogue {
	loaded, err := decodeCatalogue(catalogueYAML)
	if err != nil {
		panic(fmt.Sprintf("rules catalogue is invalid: %v", err))
	}
	return &loaded
}

// decodeCatalogue reads a catalogue file, refusing a key the catalogue does
// not have and any entry the rules cannot use.
func decodeCatalogue(content []byte) (Catalogue, error) {
	var decoded Catalogue
	decoder := yaml.NewDecoder(bytes.NewReader(content))
	decoder.KnownFields(true)
	if err := decoder.Decode(&decoded); err != nil && !errors.Is(err, io.EOF) {
		return Catalogue{}, err
	}
	for index := range decoded.Families {
		if err := decoded.Families[index].check(); err != nil {
			return Catalogue{}, err
		}
	}
	for index := range decoded.Apps {
		if err := decoded.Apps[index].check(); err != nil {
			return Catalogue{}, err
		}
	}
	return decoded, nil
}

// LoadCatalogue merges override files, keyed by name, over the shipped
// catalogue in name order, so a later file wins: a family replaces the one
// of its name, and an app entry wins for the images it lists. A file with a
// problem is left out, and named in the catalogue's Problems.
func LoadCatalogue(files map[string][]byte) *Catalogue {
	merged := &Catalogue{Families: slices.Clone(shipped.Families), Apps: slices.Clone(shipped.Apps)}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		override, err := decodeCatalogue(files[name])
		if err != nil {
			merged.Problems = append(merged.Problems, name+problemSeparator+strings.Join(strings.Fields(err.Error()), " "))
			continue
		}
		merged.merge(override)
		merged.Overrides = append(merged.Overrides, Override{Name: name, Families: len(override.Families), Apps: len(override.Apps)})
	}
	return merged
}

// merge puts each entry of override before the ones it wins over, a later
// entry before an earlier one.
func (loaded *Catalogue) merge(override Catalogue) {
	for _, added := range override.Families {
		sameName := func(existing family) bool { return existing.Name == added.Name }
		loaded.Families = slices.Insert(slices.DeleteFunc(loaded.Families, sameName), 0, added)
	}
	for _, added := range override.Apps {
		loaded.Apps = slices.Insert(loaded.Apps, 0, added)
	}
}

func (loaded *Catalogue) family(image ir.Image) (family, bool) {
	for _, candidate := range loaded.Families {
		if slices.Contains(candidate.Images, image.Reference()) {
			return candidate, true
		}
	}
	return family{}, false
}

// database is the family an app's database settings name: by a word of
// their prefix, such as MYSQL in MYSQL_HOST, else by the port they set.
func (loaded *Catalogue) database(prefix, port string) (family, bool) {
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

func (loaded *Catalogue) appHealthcheck(image ir.Image) []string {
	return loaded.app(image).Healthcheck
}

func (loaded *Catalogue) appMemory(image ir.Image) string {
	return loaded.app(image).MemoryLimit
}

func (loaded *Catalogue) appProbePath(image ir.Image) string {
	return loaded.app(image).ProbePath
}

func (loaded *Catalogue) app(image ir.Image) knownApp {
	found := slices.IndexFunc(loaded.Apps, func(app knownApp) bool { return slices.Contains(app.Images, image.Reference()) })
	if found < 0 {
		return knownApp{}
	}
	return loaded.Apps[found]
}
