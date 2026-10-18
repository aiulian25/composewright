// Package ir defines the intermediate representation a deployment plan is
// expressed in. It cannot express privileged mode, capabilities, devices,
// host networking, host paths or build instructions; YAML is written only
// by internal/render.
package ir

import (
	"path"
	"regexp"
	"slices"
	"strings"
)

type IR struct {
	IRVersion              string            `json:"ir_version"`
	App                    App               `json:"app"`
	Services               []Service         `json:"services"`
	EnvFile                []EnvFileEntry    `json:"env_file"`
	OptionalEnv            []OptionalSetting `json:"optional_env"`
	OptionalServices       []OptionalService `json:"optional_services"`
	ConfigFiles            []ConfigFile      `json:"config_files"`
	Needs                  []Need            `json:"needs"`
	Probe                  Probe             `json:"probe"`
	PostInstallNotes       []string          `json:"post_install_notes"`
	Confidence             string            `json:"confidence"`
	Evidence               []Evidence        `json:"evidence"`
	SuspiciousInstructions []string          `json:"suspicious_instructions"`
	Unfilled               []string          `json:"unfilled"`
}

type App struct {
	Name        string `json:"name"`
	Summary     string `json:"summary"`
	UpstreamURL string `json:"upstream_url"`
	DocsURL     string `json:"docs_url"`
}

type Provenance struct {
	Source     string `json:"source"`
	SourceRef  string `json:"source_ref"`
	Confidence string `json:"confidence"`
}

type Image struct {
	Registry   string     `json:"registry"`
	Repository string     `json:"repository"`
	Tag        string     `json:"tag"`
	Digest     string     `json:"digest"`
	Provenance Provenance `json:"provenance"`
}

type EnvRef struct {
	Key         string     `json:"key"`
	Ref         string     `json:"ref"`
	Literal     string     `json:"literal"`
	Required    bool       `json:"required"`
	Secret      bool       `json:"secret"`
	Description string     `json:"description"`
	Provenance  Provenance `json:"provenance"`
}

type Port struct {
	Container   int        `json:"container"`
	Protocol    string     `json:"protocol"`
	Publish     bool       `json:"publish"`
	HostPort    int        `json:"host_port"`
	Description string     `json:"description"`
	Provenance  Provenance `json:"provenance"`
}

type Volume struct {
	Name          string     `json:"name"`
	ContainerPath string     `json:"container_path"`
	Description   string     `json:"description"`
	Provenance    Provenance `json:"provenance"`
}

type Dependency struct {
	Service   string `json:"service"`
	Condition string `json:"condition"`
}

type Healthcheck struct {
	Test          []string   `json:"test"`
	Interval      string     `json:"interval"`
	Timeout       string     `json:"timeout"`
	Retries       int        `json:"retries"`
	StartPeriod   string     `json:"start_period"`
	OmittedReason string     `json:"omitted_reason"`
	Provenance    Provenance `json:"provenance"`
}

type Resources struct {
	MemoryLimit string     `json:"memory_limit"`
	CPUs        string     `json:"cpus"`
	PIDsLimit   int        `json:"pids_limit"`
	ShmSize     string     `json:"shm_size"`
	Provenance  Provenance `json:"provenance"`
}

// Hardening is what upstream takes away from a service: capabilities it
// drops, a read-only root filesystem and the tmpfs mounts it writes to.
type Hardening struct {
	CapDrop    []string   `json:"cap_drop"`
	ReadOnly   bool       `json:"read_only"`
	Tmpfs      []string   `json:"tmpfs"`
	Provenance Provenance `json:"provenance"`
}

type Service struct {
	Name        string       `json:"name"`
	Role        string       `json:"role"`
	Image       Image        `json:"image"`
	Command     []string     `json:"command"`
	Entrypoint  []string     `json:"entrypoint"`
	Env         []EnvRef     `json:"env"`
	Ports       []Port       `json:"ports"`
	Volumes     []Volume     `json:"volumes"`
	DependsOn   []Dependency `json:"depends_on"`
	Healthcheck Healthcheck  `json:"healthcheck"`
	User        string       `json:"user"`
	Resources   Resources    `json:"resources"`
	Hardening   Hardening    `json:"hardening"`
	Notes       []string     `json:"notes"`
	Provenance  Provenance   `json:"provenance"`
}

type EnvFileEntry struct {
	Key         string     `json:"key"`
	Value       string     `json:"value"`
	Generate    string     `json:"generate"`
	Secret      bool       `json:"secret"`
	Required    bool       `json:"required"`
	Description string     `json:"description"`
	FileEnvKey  string     `json:"file_env_key"`
	Provenance  Provenance `json:"provenance"`
}

// ConfigFile is a file of the repository a service mounts, such as an
// nginx.conf: it travels with compose.yaml under config/ and is mounted
// read-only.
type ConfigFile struct {
	Service string `json:"service"`
	// Path is where the file is kept beside compose.yaml, such as
	// config/nginx.conf.
	Path string `json:"path"`
	// Source is the repository file it is copied from: the one upstream
	// mounts, or the example the repository ships for it.
	Source        string     `json:"source"`
	ContainerPath string     `json:"container_path"`
	Provenance    Provenance `json:"provenance"`
}

// configFileName is the name a bundled file may have under ConfigDirectory.
var configFileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// ConfigPath is where a file named name is kept beside compose.yaml, and
// whether composewright bundles a file of that name.
func ConfigPath(name string) (string, bool) {
	return path.Join(ConfigDirectory, name), configFileName.MatchString(name)
}

// IsConfigPath reports whether a path beside compose.yaml is one a bundled
// file is kept at.
func IsConfigPath(kept string) bool {
	directory, name := path.Split(kept)
	return directory == ConfigDirectory+"/" && configFileName.MatchString(name)
}

// OptionalSetting is a variable the upstream documents and the plan leaves
// unset: .env lists it commented out, and compose.yaml passes it to the
// service by name, so setting it in .env is the only edit.
type OptionalSetting struct {
	Service     string     `json:"service"`
	Key         string     `json:"key"`
	Default     string     `json:"default"`
	Description string     `json:"description"`
	Provenance  Provenance `json:"provenance"`
}

// OptionalService describes a service the upstream starts only with a
// Compose profile. A switched-on one is also an ordinary service of the
// plan; the others are not rendered, gated or run.
type OptionalService struct {
	Name     string   `json:"name"`
	Image    string   `json:"image"`
	Profiles []string `json:"profiles"`
	// DependsOn are the optional services it needs, switched on with it.
	DependsOn []string `json:"depends_on"`
}

// ProfileList names Compose profiles as a plan or a page lists them.
func ProfileList(profiles []string) string {
	return strings.Join(profiles, profileSeparator)
}

type Need struct {
	Capability string     `json:"capability"`
	Service    string     `json:"service"`
	Detail     string     `json:"detail"`
	Reason     string     `json:"reason"`
	Provenance Provenance `json:"provenance"`
}

type Probe struct {
	Service string `json:"service"`
	Port    int    `json:"port"`
	Path    string `json:"path"`
	Expect  []int  `json:"expect"`
}

type Evidence struct {
	Claim string `json:"claim"`
	File  string `json:"file"`
	Line  int    `json:"line"`
}

func (image Image) Reference() string {
	return image.Registry + "/" + image.Repository
}

// TagSeparator joins a repository and its tag.
const TagSeparator = ":"

// TaggedReference is the repository with its tag, without the digest.
func (image Image) TaggedReference() string {
	return image.Reference() + TagSeparator + image.Tag
}

// String is the pullable reference, pinned by digest when one is known.
func (image Image) String() string {
	reference := image.TaggedReference()
	if image.Digest != "" {
		reference += "@" + image.Digest
	}
	return reference
}

// IsDockerSocket reports whether a host path is a Docker daemon's socket,
// such as /var/run/docker.sock or a rootless daemon's
// /run/user/1000/docker.sock.
func IsDockerSocket(hostPath string) bool {
	return path.IsAbs(hostPath) && path.Base(hostPath) == dockerSocketName
}

func (healthcheck Healthcheck) Defined() bool {
	return len(healthcheck.Test) > 0
}

// Service finds a service by name, ignoring case as Docker's DNS does, so a
// name spelled as upstream wrote it still finds the lower-cased service.
func (plan *IR) Service(name string) (*Service, bool) {
	for index := range plan.Services {
		if strings.EqualFold(plan.Services[index].Name, name) {
			return &plan.Services[index], true
		}
	}
	return nil, false
}

func (plan *IR) EnvEntry(key string) (*EnvFileEntry, bool) {
	for index := range plan.EnvFile {
		if plan.EnvFile[index].Key == key {
			return &plan.EnvFile[index], true
		}
	}
	return nil, false
}

// Fields lists the provenance of every field the plan fills: each service
// with its image, healthcheck, ports and volumes, each .env entry and each
// need.
func (plan *IR) Fields() []Provenance {
	var fields []Provenance
	for _, service := range plan.Services {
		fields = append(fields, service.Provenance, service.Image.Provenance)
		if service.Healthcheck.Defined() {
			fields = append(fields, service.Healthcheck.Provenance)
		}
		for _, port := range service.Ports {
			fields = append(fields, port.Provenance)
		}
		for _, volume := range service.Volumes {
			fields = append(fields, volume.Provenance)
		}
	}
	for _, entry := range plan.EnvFile {
		fields = append(fields, entry.Provenance)
	}
	for _, need := range plan.Needs {
		fields = append(fields, need.Provenance)
	}
	return fields
}

// AppService returns the service with the app role, or nil when there is none.
func (plan *IR) AppService() *Service {
	for index := range plan.Services {
		if plan.Services[index].Role == RoleApp {
			return &plan.Services[index]
		}
	}
	return nil
}

// Rank orders confidence levels; unknown or empty values rank lowest.
func Rank(confidence string) int {
	index := slices.Index(Confidences, confidence)
	if index < 0 {
		return 0
	}
	return len(Confidences) - index
}
