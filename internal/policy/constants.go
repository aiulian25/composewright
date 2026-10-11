package policy

const (
	maxServices       = 8
	lowPortLimit      = 1024
	maxPort           = 65535
	defaultRegistry   = "docker.io"
	officialNamespace = "docker.io/library/"
	hostWideSuffix    = "/*"
	wildcard          = "*"
	secretsDirPrefix  = "./secrets/"
	digestPrefix      = "sha256:"
	latestTag         = "latest"
	rootUser          = "root"
	rootUID           = "0"
	networkModeHost   = "host"
	volumeTypeBind    = "bind"
	volumeTypeVolume  = "volume"
	volumeTypeTmpfs   = "tmpfs"
	localVolumeDriver = "local"
	noNewPrivileges   = "no-new-privileges"
)

// ownerRegistries are the hosts where a GitHub owner's namespace is trusted
// automatically (P1): projects publish under their own name there.
var ownerRegistries = []string{"docker.io", "ghcr.io", "quay.io"}

var forbiddenTargets = []string{"/var/run/docker.sock", "/run/docker.sock", "/proc", "/sys", "/dev"}

var allowedNoNewPrivileges = []string{"no-new-privileges", "no-new-privileges:true", "no-new-privileges=true"}

// localLogDrivers keep a container's logs on this host, or keep none; any
// other driver lets the Docker daemon send them elsewhere.
var localLogDrivers = []string{"", "json-file", "local", "journald", "none"}
