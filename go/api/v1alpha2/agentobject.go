package v1alpha2

import "sigs.k8s.io/controller-runtime/pkg/client"

type WorkloadMode string

const (
	WorkloadModeDeployment WorkloadMode = "deployment"
	WorkloadModeSandbox    WorkloadMode = "sandbox"
)

// defaultWorkloadMode is the fallback used when an Agent's spec.workloadMode
// is unset. The controller binary calls SetDefaultWorkloadMode at startup
// from its --default-workload-mode flag (helm value `defaultWorkloadMode`).
var defaultWorkloadMode = WorkloadModeDeployment

// SetDefaultWorkloadMode overrides the package-level default. Intended to be
// called once at controller startup; never goroutine-safe.
func SetDefaultWorkloadMode(mode WorkloadMode) {
	switch mode {
	case WorkloadModeDeployment, WorkloadModeSandbox:
		defaultWorkloadMode = mode
	}
}

// DefaultWorkloadMode returns the current package-level default. Mostly
// useful for tests and for surfacing the active default in status messages.
func DefaultWorkloadMode() WorkloadMode {
	return defaultWorkloadMode
}

// defaultAAuthEnabled is the install-wide fallback applied to every
// declarative Agent that doesn't explicitly set `spec.declarative.aauth`.
// false matches the legacy opt-in behavior; helm chart value
// `controller.defaultAAuthEnabled: true` flips it so every declarative
// Agent gets AAuth signing turned on by default.
var defaultAAuthEnabled = false

// SetDefaultAAuthEnabled overrides the package-level default. Called once
// at controller startup from the --default-aauth-enabled flag.
func SetDefaultAAuthEnabled(enabled bool) {
	defaultAAuthEnabled = enabled
}

// DefaultAAuthEnabled returns the current package-level default.
func DefaultAAuthEnabled() bool {
	return defaultAAuthEnabled
}

// AgentObject is the shared shape implemented by Agent. It remains an
// interface because earlier versions of kagent shipped both `Agent` and
// `SandboxAgent` CRDs; collapsing them is in progress, and downstream code
// (translator, A2A registrar, substrate backend) still consumes the
// interface even though there's only one implementor now.
// +kubebuilder:object:generate=false
type AgentObject interface {
	client.Object
	GetAgentSpec() *AgentSpec
	GetAgentStatus() *AgentStatus
	GetWorkloadMode() WorkloadMode
}

func (a *Agent) GetAgentSpec() *AgentSpec {
	if a == nil {
		return nil
	}
	return &a.Spec
}

func (a *Agent) GetAgentStatus() *AgentStatus {
	if a == nil {
		return nil
	}
	return &a.Status
}

// GetWorkloadMode returns this Agent's effective workload mode: the
// explicit spec.workloadMode field if set, otherwise the package-level
// default (controlled by --default-workload-mode at controller startup).
func (a *Agent) GetWorkloadMode() WorkloadMode {
	if a == nil {
		return defaultWorkloadMode
	}
	if a.Spec.WorkloadMode != nil && *a.Spec.WorkloadMode != "" {
		return *a.Spec.WorkloadMode
	}
	return defaultWorkloadMode
}
