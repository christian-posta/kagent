/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package v1alpha2

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// AgentHarnessBackendType selects which sandbox control plane provisions the
// environment. Additional backends may be added in the future.
// +kubebuilder:validation:Enum=openshell;openclaw;nemoclaw
type AgentHarnessBackendType string

const (
	AgentHarnessBackendOpenshell AgentHarnessBackendType = "openshell"
	AgentHarnessBackendOpenClaw  AgentHarnessBackendType = "openclaw"
	AgentHarnessBackendNemoClaw  AgentHarnessBackendType = "nemoclaw"
)

// AgentHarnessChannelType selects a messenger integration for OpenClaw harness VMs.
// +kubebuilder:validation:Enum=telegram;slack
type AgentHarnessChannelType string

const (
	AgentHarnessChannelTypeTelegram AgentHarnessChannelType = "telegram"
	AgentHarnessChannelTypeSlack    AgentHarnessChannelType = "slack"
)

// AgentHarnessChannelAccess controls whether the bot listens broadly or only on an allowlist.
// +kubebuilder:validation:Enum=allowlist;open;disabled
type AgentHarnessChannelAccess string

const (
	AgentHarnessChannelAccessAllowlist AgentHarnessChannelAccess = "allowlist"
	AgentHarnessChannelAccessOpen      AgentHarnessChannelAccess = "open"
	AgentHarnessChannelAccessDisabled  AgentHarnessChannelAccess = "disabled"
)

// AgentHarnessChannelCredential supplies a token from an inline value or a Secret/ConfigMap key.
//
// +kubebuilder:validation:XValidation:rule="(has(self.value) && !has(self.valueFrom)) || (!has(self.value) && has(self.valueFrom))",message="Exactly one of value or valueFrom must be specified"
type AgentHarnessChannelCredential struct {
	// +kubebuilder:validation:MaxLength=8192
	Value string `json:"value,omitempty"`
	// +optional
	ValueFrom *ValueSource `json:"valueFrom,omitempty"`
}

// AgentHarnessTelegramChannelSpec configures Telegram when AgentHarnessChannel.type is Telegram.
//
// +kubebuilder:validation:XValidation:rule="!(size(self.allowedUserIDs) > 0 && has(self.allowedUserIDsFrom))",message="allowedUserIDs and allowedUserIDsFrom are mutually exclusive"
type AgentHarnessTelegramChannelSpec struct {
	BotToken AgentHarnessChannelCredential `json:"botToken"`
	// +optional
	AllowedUserIDs []string `json:"allowedUserIDs,omitempty"`
	// +optional
	AllowedUserIDsFrom *ValueSource `json:"allowedUserIDsFrom,omitempty"`
}

// AgentHarnessSlackChannelSpec configures Slack when AgentHarnessChannel.type is Slack.
//
// +kubebuilder:validation:XValidation:rule="self.channelAccess != 'allowlist' || (has(self.allowlistChannels) && size(self.allowlistChannels) > 0)",message="allowlistChannels is required when channelAccess is allowlist"
type AgentHarnessSlackChannelSpec struct {
	BotToken AgentHarnessChannelCredential `json:"botToken"`
	AppToken AgentHarnessChannelCredential `json:"appToken"`
	// +kubebuilder:validation:Required
	ChannelAccess AgentHarnessChannelAccess `json:"channelAccess"`
	// +optional
	AllowlistChannels []string `json:"allowlistChannels,omitempty"`
	// +optional
	// +kubebuilder:default=true
	InteractiveReplies *bool `json:"interactiveReplies,omitempty"`
}

// AgentHarnessChannel declares one messenger binding inside an OpenClaw/NemoClaw harness VM.
//
// +kubebuilder:validation:XValidation:rule="(self.type == 'telegram' && has(self.telegram) && !has(self.slack)) || (self.type == 'slack' && has(self.slack) && !has(self.telegram))",message="exactly one of telegram or slack must be set and must match type"
type AgentHarnessChannel struct {
	// Name is a stable id for this binding (OpenClaw channels.*.accounts key).
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// +kubebuilder:validation:Required
	Type AgentHarnessChannelType `json:"type"`
	// +optional
	Telegram *AgentHarnessTelegramChannelSpec `json:"telegram,omitempty"`
	// +optional
	Slack *AgentHarnessSlackChannelSpec `json:"slack,omitempty"`
}

// AgentHarnessRuntime selects which control plane provisions the harness VM.
// +kubebuilder:validation:Enum=openshell;substrate
type AgentHarnessRuntime string

const (
	AgentHarnessRuntimeOpenshell AgentHarnessRuntime = "openshell"
	AgentHarnessRuntimeSubstrate AgentHarnessRuntime = "substrate"
)

// AgentHarnessSubstrateSnapshotsConfig points at a GCS prefix for actor memory snapshots.
// Substrate currently expects a gs:// location (see Agent Substrate SnapshotsConfig).
type AgentHarnessSubstrateSnapshotsConfig struct {
	// Location is the GCS URI prefix for golden and incremental snapshots.
	// Example: gs://ate-snapshots/kagent/my-namespace/my-harness/
	// +required
	// +kubebuilder:validation:Pattern=`^gs://`
	Location string `json:"location"`
}

// AgentHarnessSubstrateWorkerPoolSpec creates a dedicated WorkerPool for this harness.
// Mutually exclusive with workerPoolRef.
type AgentHarnessSubstrateWorkerPoolSpec struct {
	// Replicas is the number of ateom worker pods. Defaults to 1 when unset or zero.
	// +optional
	// +kubebuilder:default=1
	Replicas int32 `json:"replicas,omitempty"`

	// AteomImage is the ateom herder image (pullable registry ref, not ko://).
	// Overrides the controller-wide substrate ateom image default for this WorkerPool.
	// +optional
	AteomImage string `json:"ateomImage,omitempty"`
}

// AgentHarnessSubstrateSpec configures Agent Substrate (WorkerPool + ActorTemplate + Actor).
//
// By default kagent provisions a per-harness ActorTemplate (and optionally a WorkerPool).
// Set actorTemplateRef only to adopt an existing template (advanced / legacy).
// +kubebuilder:validation:XValidation:rule="(has(self.gatewayToken) && !has(self.gatewayTokenSecretRef)) || (!has(self.gatewayToken) && has(self.gatewayTokenSecretRef))",message="Exactly one of gatewayToken or gatewayTokenSecretRef must be specified"
// +kubebuilder:validation:XValidation:rule="!(has(self.workerPoolRef) && has(self.workerPool))",message="workerPoolRef and workerPool are mutually exclusive"
type AgentHarnessSubstrateSpec struct {
	// WorkerPoolRef references an existing ate.dev WorkerPool (namespace/name).
	// Mutually exclusive with workerPool.
	// +optional
	WorkerPoolRef *TypedLocalReference `json:"workerPoolRef,omitempty"`

	// WorkerPool creates a dedicated WorkerPool in the harness namespace when workerPoolRef is unset.
	// +optional
	WorkerPool *AgentHarnessSubstrateWorkerPoolSpec `json:"workerPool,omitempty"`

	// SnapshotsConfig configures actor memory snapshots. Defaults to
	// gs://ate-snapshots/<namespace>/<agentharnessname> when unset.
	// +optional
	SnapshotsConfig *AgentHarnessSubstrateSnapshotsConfig `json:"snapshotsConfig,omitempty"`

	// WorkloadImage overrides the default nemoclaw/openclaw sandbox image in the ActorTemplate.
	// +optional
	WorkloadImage string `json:"workloadImage,omitempty"`

	// ActorTemplateRef adopts an existing ate.dev ActorTemplate instead of auto-provisioning.
	// When set, workerPoolRef/workerPool/snapshotsConfig are ignored for template creation.
	// +optional
	ActorTemplateRef *TypedLocalReference `json:"actorTemplateRef,omitempty"`

	// GatewayPort is the port OpenClaw listens on inside the actor (Substrate routes to :80 today).
	// +optional
	// +kubebuilder:default=80
	GatewayPort int32 `json:"gatewayPort,omitempty"`

	// GatewayToken is the OpenClaw gateway Bearer token for this harness.
	// Prefer gatewayTokenSecretRef for production secrets.
	// +optional
	// +kubebuilder:validation:MinLength=1
	GatewayToken string `json:"gatewayToken,omitempty"`

	// GatewayTokenSecretRef references a Secret key holding the OpenClaw gateway Bearer token.
	// The Secret must contain a "token" key.
	// +optional
	GatewayTokenSecretRef *TypedLocalReference `json:"gatewayTokenSecretRef,omitempty"`
}

// AgentHarnessSpec describes a generic remote execution environment that agents
// (or human operators) can attach to via exec or SSH.
//
// An AgentHarness is distinct from a SandboxAgent: it has no agent runtime baked
// in. The backend is responsible for provisioning an environment that stays
// ready to accept incoming commands.
//
// +kubebuilder:validation:XValidation:rule="!has(self.channels) || size(self.channels) == 0 || self.backend == 'openclaw' || self.backend == 'nemoclaw'",message="channels may only be set when backend is openclaw or nemoclaw"
// +kubebuilder:validation:XValidation:rule="!has(self.substrate) || self.runtime == 'substrate'",message="spec.substrate may only be set when runtime is substrate"
// +kubebuilder:validation:XValidation:rule="self.runtime != 'substrate' || has(self.substrate)",message="spec.substrate is required when runtime is substrate"
type AgentHarnessSpec struct {
	// Runtime selects the harness provisioning stack. Defaults to openshell when unset.
	// +optional
	// +kubebuilder:default=openshell
	Runtime AgentHarnessRuntime `json:"runtime,omitempty"`

	// Substrate configures Agent Substrate when runtime is substrate.
	// +optional
	Substrate *AgentHarnessSubstrateSpec `json:"substrate,omitempty"`

	// Backend selects the control plane to use. Required.
	// +kubebuilder:validation:Required
	Backend AgentHarnessBackendType `json:"backend"`

	// Description is a short human-readable summary shown in the UI (e.g. agents list).
	// +optional
	Description string `json:"description,omitempty"`

	// Image is the container image to run in the harness VM, if the backend
	// supports per-resource images. Backends openclaw and nemoclaw pin the image
	// to the NemoClaw sandbox base; openshell uses spec.image when set.
	// +optional
	Image string `json:"image,omitempty"`

	// Env is a list of environment variables injected into the harness workload.
	// Values use the Kubernetes EnvVar shape; ValueFrom references are
	// resolved server-side where supported.
	// +optional
	Env []corev1.EnvVar `json:"env,omitempty"`

	// Network controls outbound access from the harness. When unset,
	// backend defaults apply.
	// +optional
	Network *AgentHarnessNetwork `json:"network,omitempty"`

	// ModelConfigRef is the reference to the ModelConfig used to configure the harness.
	// When set with backend openclaw or nemoclaw, the controller registers the gateway provider and,
	// after the harness is Ready, writes OpenClaw config inside the VM (~/.openclaw/openclaw.json) and starts the gateway.
	// It is ignored for backend openshell.
	// +optional
	ModelConfigRef string `json:"modelConfigRef,omitempty"`

	// Channels configures Telegram and Slack integrations for OpenClaw inside the harness VM.
	// Only supported when backend is openclaw or nemoclaw.
	// +optional
	Channels []AgentHarnessChannel `json:"channels,omitempty"`
}

// AgentHarnessNetwork captures the minimal network-policy knobs exposed to users.
type AgentHarnessNetwork struct {
	// AllowedDomains is a list of DNS names the harness may reach.
	// +optional
	AllowedDomains []string `json:"allowedDomains,omitempty"`
}

// AgentHarnessConnection describes how clients reach the provisioned harness VM.
type AgentHarnessConnection struct {
	// Endpoint is the backend-specific address (gRPC target, SSH host:port,
	// ...) clients should use to reach the harness.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
}

// AgentHarnessStatusRef identifies a harness instance on an external control plane.
type AgentHarnessStatusRef struct {
	Backend AgentHarnessBackendType `json:"backend"`
	ID      string                  `json:"id"`
}

// AgentHarnessStatus is the observed state of an AgentHarness.
type AgentHarnessStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`

	// BackendRef points at the harness instance on the backend control
	// plane, once Ensure has succeeded at least once.
	// +optional
	BackendRef *AgentHarnessStatusRef `json:"backendRef,omitempty"`

	// Connection is populated by the controller when the harness is ready.
	// +optional
	Connection *AgentHarnessConnection `json:"connection,omitempty"`

	// Substrate records observed Substrate provisioning state.
	// +optional
	Substrate *AgentHarnessSubstrateStatus `json:"substrate,omitempty"`
}

// AgentHarnessSubstrateStatus is observed Substrate control-plane state for this harness.
type AgentHarnessSubstrateStatus struct {
	// Conditions describe substrate provisioning progress (e.g. ActorTemplate golden snapshot).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// AgentHarnessSubstrateConditionType enumerates substrate-specific condition types.
const (
	AgentHarnessSubstrateConditionTypeActorTemplateReady = "ActorTemplateReady"
	AgentHarnessSubstrateConditionTypeResourcesCleaned   = "ResourcesCleaned"
)

// AgentHarnessConditionType enumerates the condition types an AgentHarness may report.
const (
	AgentHarnessConditionTypeReady    = "Ready"
	AgentHarnessConditionTypeAccepted = "Accepted"
)

// +kubebuilder:object:root=true
// +kubebuilder:resource:path=agentharnesses,singular=agentharness,shortName=ahr,categories=kagent
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Backend",type="string",JSONPath=".spec.backend"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="ID",type="string",JSONPath=".status.backendRef.id"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentHarness is a generic remote execution environment provisioned by a backend
// (e.g. OpenShell) and addressable by exec/SSH.
type AgentHarness struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentHarnessSpec   `json:"spec,omitempty"`
	Status AgentHarnessStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentHarnessList is a list of AgentHarness resources.
type AgentHarnessList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentHarness `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(GroupVersion, &AgentHarness{}, &AgentHarnessList{})
		return nil
	})
}
