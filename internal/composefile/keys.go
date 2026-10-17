package composefile

// Service keys for the limits and the hardening, shared by the packages
// that read, write and label them.
const (
	KeyMemLimit  = "mem_limit"
	KeyCPUs      = "cpus"
	KeyPIDsLimit = "pids_limit"
	KeyShmSize   = "shm_size"
	KeyCapDrop   = "cap_drop"
	KeyReadOnly  = "read_only"
	KeyTmpfs     = "tmpfs"
)

// Service keys for the privileges a service asks for, which a user grants.
const (
	KeyDevices     = "devices"
	KeyCapAdd      = "cap_add"
	KeyPrivileged  = "privileged"
	KeyNetworkMode = "network_mode"
)

// NetworkModeHost is the network_mode that shares the host's network.
const NetworkModeHost = "host"

// DeviceSeparator parts a device mapping's host path, container path and
// permissions, as in /dev/ttyUSB0:/dev/ttyUSB0:rwm.
const DeviceSeparator = ":"

// TmpfsOptionsSeparator parts a tmpfs entry's path from its options, as in
// /run:size=65536,mode=1777.
const TmpfsOptionsSeparator = ":"
