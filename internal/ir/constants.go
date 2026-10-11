package ir

import "github.com/aiulian25/composewright/internal/secrets"

const (
	Version = "1"

	RoleApp    = "app"
	RoleDB     = "db"
	RoleCache  = "cache"
	RoleWorker = "worker"
	RoleProxy  = "proxy"
	RoleInit   = "init"

	ProtocolTCP = "tcp"
	ProtocolUDP = "udp"

	ConditionStarted   = "service_started"
	ConditionHealthy   = "service_healthy"
	ConditionCompleted = "service_completed_successfully"

	NeedHostNetwork = "host_network"
	NeedDevices     = "devices"
	NeedPrivileged  = "privileged"
	NeedBindMount   = "bind_mount"
	NeedLowPort     = "low_port"
	NeedCapAdd      = "cap_add"

	GeneratePassword24 = secrets.GeneratePassword24
	GenerateHex32      = secrets.GenerateHex32
	GenerateBase64     = secrets.GenerateBase64
	GenerateUUID       = secrets.GenerateUUID
	GenerateLaravelKey = secrets.GenerateLaravelKey

	ConfidenceHigh   = "high"
	ConfidenceMedium = "medium"
	ConfidenceLow    = "low"

	SourceCompose      = "compose"
	SourceEnvSample    = "env_sample"
	SourceDockerfile   = "dockerfile"
	SourceOCILabel     = "oci_label"
	SourceSidecarRule  = "sidecar_rule"
	SourceHealthchecks = "healthcheck_catalogue"
	SourceReadme       = "readme"
	SourceCode         = "code"
	SourceHelm         = "helm"
	SourceCIWorkflow   = "ci_workflow"
	SourceCatalogue    = "catalogue"
	SourceModel        = "model"
	SourceUser         = "user"
	SourceDefault      = "default"

	HealthcheckCommand = "CMD"
	HealthcheckShell   = "CMD-SHELL"
)

var (
	Roles        = []string{RoleApp, RoleDB, RoleCache, RoleWorker, RoleProxy, RoleInit}
	Protocols    = []string{ProtocolTCP, ProtocolUDP}
	Conditions   = []string{ConditionStarted, ConditionHealthy, ConditionCompleted}
	Capabilities = []string{NeedHostNetwork, NeedDevices, NeedPrivileged, NeedBindMount, NeedLowPort, NeedCapAdd}
	Generators   = []string{"", GeneratePassword24, GenerateHex32, GenerateBase64, GenerateUUID, GenerateLaravelKey}
	Confidences  = []string{ConfidenceHigh, ConfidenceMedium, ConfidenceLow}
	Sources      = []string{
		SourceCompose, SourceEnvSample, SourceDockerfile, SourceOCILabel, SourceSidecarRule, SourceHealthchecks, SourceReadme,
		SourceCode, SourceHelm, SourceCIWorkflow, SourceCatalogue, SourceModel, SourceUser, SourceDefault,
	}
)
