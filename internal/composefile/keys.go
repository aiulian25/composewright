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

// TmpfsOptionsSeparator parts a tmpfs entry's path from its options, as in
// /run:size=65536,mode=1777.
const TmpfsOptionsSeparator = ":"
