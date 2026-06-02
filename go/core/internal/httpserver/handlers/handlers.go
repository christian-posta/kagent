package handlers

import (
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/kagent-dev/kagent/go/api/database"
	"github.com/kagent-dev/kagent/go/core/internal/controller/reconciler"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/substrate/harness"
)

// Handlers holds all the HTTP handler components
type Handlers struct {
	// KubeClient + AgentHarnessGateway support the substrate-backed
	// AgentHarness gateway proxy (HandleAgentHarnessGateway at
	// /api/agentharnesses/<ns>/<name>/gateway/). KubeClient is also held in
	// Base; the gateway handler is a method on *Handlers directly so it
	// needs them at this level too.
	KubeClient          client.Client
	AgentHarnessGateway *AgentHarnessGatewayConfig
	// SubstrateHarnessClient is the same pre-dialed harness.Client that
	// powers the /api/substrate observability endpoints. The AgentHarness
	// gateway proxy reuses it so it doesn't open a fresh gRPC connection
	// to ate-api on every request — under page-load parallelism (~10
	// parallel asset fetches) the per-request dial was storming ate-api
	// and getting intermittent INTERNAL_SERVER_ERROR responses, which the
	// gateway then turned into 503 text/plain replies and broke the
	// browser's MIME-typed asset loads.
	SubstrateHarnessClient *harness.Client

	Health              *HealthHandler
	ModelConfig         *ModelConfigHandler
	Model               *ModelHandler
	ModelProviderConfig *ModelProviderConfigHandler
	Sessions            *SessionsHandler
	Agents              *AgentsHandler
	Tools               *ToolsHandler
	ToolServers         *ToolServersHandler
	ToolServerTypes     *ToolServerTypesHandler
	Memory              *MemoryHandler
	Feedback            *FeedbackHandler
	Namespaces          *NamespacesHandler
	PromptTemplates     *PromptTemplatesHandler
	Tasks               *TasksHandler
	Checkpoints         *CheckpointsHandler
	CrewAI              *CrewAIHandler
	CurrentUser         *CurrentUserHandler
	// Substrate exposes read-only observability for the substrate workers +
	// actors. Set in server.go after construction so handlers.go has no
	// build-time dep on the harness package.
	Substrate *SubstrateHandler
}

// Base holds common dependencies for all handlers
type Base struct {
	KubeClient         client.Client
	DefaultModelConfig types.NamespacedName
	DatabaseService    database.Client
	Authorizer         auth.Authorizer // Interface for authorization checks
	ProxyURL           string
	WatchedNamespaces  []string
	SandboxBackend     sandboxbackend.Backend
}

// NewHandlers creates a new Handlers instance with all handler components.
func NewHandlers(kubeClient client.Client, defaultModelConfig types.NamespacedName, dbService database.Client, watchedNamespaces []string, authorizer auth.Authorizer, proxyURL string, rcnclr reconciler.KagentReconciler, sandboxBackend sandboxbackend.Backend) *Handlers {
	base := &Base{
		KubeClient:         kubeClient,
		DefaultModelConfig: defaultModelConfig,
		DatabaseService:    dbService,
		Authorizer:         authorizer,
		ProxyURL:           proxyURL,
		WatchedNamespaces:  watchedNamespaces,
		SandboxBackend:     sandboxBackend,
	}

	return &Handlers{
		KubeClient:          kubeClient,
		Health:              NewHealthHandler(),
		ModelConfig:         NewModelConfigHandler(base),
		Model:               NewModelHandler(base),
		ModelProviderConfig: NewModelProviderConfigHandler(base, rcnclr),
		Sessions:            NewSessionsHandler(base),
		Agents:              NewAgentsHandler(base),
		Tools:               NewToolsHandler(base),
		ToolServers:         NewToolServersHandler(base),
		ToolServerTypes:     NewToolServerTypesHandler(base),
		Memory:              NewMemoryHandler(base),
		Feedback:            NewFeedbackHandler(base),
		Namespaces:          NewNamespacesHandler(base),
		PromptTemplates:     NewPromptTemplatesHandler(base),
		Tasks:               NewTasksHandler(base),
		Checkpoints:         NewCheckpointsHandler(base),
		CrewAI:              NewCrewAIHandler(base),
		CurrentUser:         NewCurrentUserHandler(),
	}
}
