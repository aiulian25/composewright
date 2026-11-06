package policy

import "github.com/docker/go-units"

const (
	maxServices = 8
	// maxReplicas is how many copies of one service a stack may run; a
	// sandbox run tests one.
	maxReplicas       = 8
	lowPortLimit      = 1024
	defaultRegistry   = "docker.io"
	officialNamespace = "docker.io/library/"
	hostWideSuffix    = "/*"
	wildcard          = "*"
	secretsDirPrefix  = "./secrets/"
	// relativePrefix starts a mount source beside compose.yaml.
	relativePrefix    = "./"
	digestPrefix      = "sha256:"
	latestTag         = "latest"
	includeKey        = "include"
	composeFileName   = "compose.yaml"
	rootUser          = "root"
	rootUID           = "0"
	volumeTypeBind    = "bind"
	volumeTypeVolume  = "volume"
	volumeTypeTmpfs   = "tmpfs"
	localVolumeDriver = "local"
	// defaultDevicePermissions are what Docker gives a device mapping that
	// names none: read, write and mknod.
	defaultDevicePermissions = "rwm"
	noNewPrivileges          = "no-new-privileges"
	// tightFactor and generousFactor bound a mem_limit against a service's
	// observed peak; limitStep is what a suggested limit rounds up to.
	tightFactor    = 1.5
	generousFactor = 4
	limitStep      = 128 * units.MiB
	mebibyteSuffix = "m"
)

// ownerRegistries are the hosts where the repository owner's namespace is
// trusted automatically (P1): projects publish under their own name there.
var ownerRegistries = []string{"docker.io", "ghcr.io", "quay.io"}

// nameOnlyRegistries are the owner registries whose accounts are not the
// repository owner's accounts: the owner's name there may belong to
// someone else, so an image trusted by that name alone gets a P1 warning.
// ghcr.io namespaces are the GitHub accounts themselves.
var nameOnlyRegistries = []string{"docker.io", "quay.io"}

// portTakenMessage warns that a host port is taken, by what
// TakenPorts says, joined by portTakersSeparator.
const (
	portTakenMessage    = "host port %d is also %s"
	portTakersSeparator = " and "
)

// OutsideAllowlist and TrustedByName mark the P1 findings a person settles
// by trusting the image's namespace on the Review page. TrustedByName is
// the part of the warning that versions stored while it named only GitHub
// owners share.
const (
	OutsideAllowlist = "outside the registry allowlist"
	TrustedByName    = "trusted only because its namespace matches"
)

var forbiddenTargets = []string{"/var/run/docker.sock", "/run/docker.sock", "/proc", "/sys", "/dev"}

var allowedNoNewPrivileges = []string{"no-new-privileges", "no-new-privileges:true", "no-new-privileges=true"}

// localLogDrivers keep a container's logs on this host, or keep none; any
// other driver lets the Docker daemon send them elsewhere.
var localLogDrivers = []string{"", "json-file", "local", "journald", "none"}
