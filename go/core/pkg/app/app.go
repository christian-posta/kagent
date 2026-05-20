/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package app

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"net/http"
	"net/http/pprof"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/hashicorp/go-multierror"
	"github.com/kagent-dev/kagent/go/core/internal/version"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"

	"github.com/kagent-dev/kagent/go/core/internal/a2a"
	"github.com/kagent-dev/kagent/go/core/internal/aauth"
	"github.com/kagent-dev/kagent/go/core/internal/database"
	"github.com/kagent-dev/kagent/go/core/internal/mcp"
	versionmetrics "github.com/kagent-dev/kagent/go/core/internal/metrics"
	"github.com/kagent-dev/kagent/go/core/internal/telemetry"

	"github.com/kagent-dev/kagent/go/core/internal/controller/reconciler"
	reconcilerutils "github.com/kagent-dev/kagent/go/core/internal/controller/reconciler/utils"
	agent_translator "github.com/kagent-dev/kagent/go/core/internal/controller/translator/agent"
	"github.com/kagent-dev/kagent/go/core/internal/httpserver"
	"github.com/kagent-dev/kagent/go/core/internal/httpserver/handlers"
	common "github.com/kagent-dev/kagent/go/core/internal/utils"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	dbpkg "github.com/kagent-dev/kagent/go/api/database"
	"github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/kagent-dev/kagent/go/core/pkg/migrations"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/openclaw"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/openshell"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/substrate/harness"
	"github.com/kagent-dev/kagent/go/core/pkg/translator"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/kagent-dev/kagent/go/api/v1alpha2"
	"github.com/kagent-dev/kagent/go/core/internal/controller"
	"github.com/kagent-dev/kagent/go/core/internal/goruntime"
	"github.com/kagent-dev/kmcp/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	agentsandboxv1 "sigs.k8s.io/agent-sandbox/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	substratev1 "github.com/agent-substrate/substrate/pkg/api/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme          = runtime.NewScheme()
	setupLog        = ctrl.Log.WithName("setup")
	kagentNamespace = common.GetResourceNamespace()

	// These variables should be set during build time using -ldflags
	Version   = version.Version
	GitCommit = version.GitCommit
	BuildDate = version.BuildDate
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(v1alpha1.AddToScheme(scheme))
	utilruntime.Must(v1alpha2.AddToScheme(scheme))
	utilruntime.Must(agentsandboxv1.AddToScheme(scheme))
	utilruntime.Must(substratev1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

type Config struct {
	Metrics struct {
		Addr     string
		CertPath string
		CertName string
		CertKey  string
	}
	Webhook struct {
		CertPath string
		CertName string
		CertKey  string
	}
	Streaming struct {
		MaxBufSize     resource.QuantityValue `default:"1Mi"`
		InitialBufSize resource.QuantityValue `default:"4Ki"`
		Timeout        time.Duration          `default:"60s"`
	}
	Proxy struct {
		URL string
	}
	Auth struct {
		Mode        string
		UserIDClaim string
	}
	LeaderElection     bool
	ProbeAddr          string
	SecureMetrics      bool
	EnableHTTP2        bool
	DefaultModelConfig types.NamespacedName
	// DefaultWorkloadMode is applied to every Agent CR that doesn't set
	// `spec.workloadMode` explicitly. Valid values: "deployment", "sandbox".
	// Defaults to "deployment". When the substrate backend is configured
	// (Substrate.WorkerPoolName set), helm typically flips this to "sandbox"
	// so the whole install opts into substrate by default.
	DefaultWorkloadMode string
	HttpServerAddr     string
	WatchNamespaces    string
	A2ABaseUrl         string
	Database           struct {
		Url           string
		UrlFile       string
		VectorEnabled bool
	}
	Openshell struct {
		GatewayURL  string
		Token       string
		TokenFile   string
		CAFile      string
		Insecure    bool
		DialTimeout time.Duration
		CallTimeout time.Duration
	}
	// Substrate selects + configures the agent-substrate (ate.dev) sandbox
	// backend. When WorkerPoolName is non-empty the controller uses this backend
	// instead of the default agentsxk8s. See SUBSTRATE.md for the design.
	Substrate struct {
		WorkerPoolNamespace string
		WorkerPoolName      string
		SnapshotsLocation   string
		PauseImage          string
		RunscAMD64URL       string
		RunscAMD64SHA256    string
		RunscARM64URL       string
		RunscARM64SHA256    string

		// ControlEndpoint is the gRPC target for substrate's ate-api-server.
		// In-cluster default: "api.ate-system.svc.cluster.local:443".
		ControlEndpoint string
		// ControlPlaintext disables TLS for the control connection. Only safe
		// for local/dev (e.g. when port-forwarding without certs).
		ControlPlaintext bool
		// RouterURL is the dial target for atenet-router that the A2A
		// registrar uses when proxying sandbox-mode agent traffic. The actor
		// identity travels via Host header, not the URL host.
		// In-cluster default: "http://atenet-router.ate-system.svc".
		RouterURL string

		// IdleTimeout is how long a substrate actor must be untouched before
		// the IdleSuspender requests a SuspendActor. Zero (or negative)
		// disables auto-suspend.
		IdleTimeout time.Duration
		// IdleSweepInterval controls how often the IdleSuspender checks for
		// expired actors. Should be smaller than IdleTimeout.
		IdleSweepInterval time.Duration
		// AgentImage, when non-empty, overrides the container.image emitted
		// on every ActorTemplate with a substrate-aware kagent app image
		// (built from python/Dockerfile.substrate). Required when running
		// real kagent ADK agents in substrate; leave empty for the Phase 0
		// stand-in / BYO demo path.
		AgentImage string

		// WorkerPoolAteomImage + WorkerPoolReplicas opt into the optional
		// WorkerPoolEnsurer (see go/core/pkg/sandboxbackend/substrate/
		// workerpool_ensurer.go). When AteomImage is set the controller
		// auto-provisions the shared WorkerPool at startup; when empty the
		// operator must apply it themselves (the original behavior).
		WorkerPoolAteomImage string
		WorkerPoolReplicas   int
	}
}

func (cfg *Config) SetFlags(commandLine *flag.FlagSet) {
	commandLine.StringVar(&cfg.Metrics.Addr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	commandLine.StringVar(&cfg.ProbeAddr, "health-probe-bind-address", ":8082", "The address the probe endpoint binds to.")
	commandLine.BoolVar(&cfg.LeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	commandLine.BoolVar(&cfg.SecureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	commandLine.StringVar(&cfg.Metrics.CertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	commandLine.StringVar(&cfg.Metrics.CertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	commandLine.StringVar(&cfg.Metrics.CertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	commandLine.StringVar(&cfg.Webhook.CertPath, "webhook-cert-path", "",
		"The directory that contains the webhook server certificate.")
	commandLine.StringVar(&cfg.Webhook.CertName, "webhook-cert-name", "tls.crt", "The name of the wehbook server certificate file.")
	commandLine.StringVar(&cfg.Webhook.CertKey, "webhook-cert-key", "tls.key", "The name of the webhook server key file.")
	commandLine.BoolVar(&cfg.EnableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	commandLine.StringVar(&cfg.DefaultModelConfig.Name, "default-model-config-name", "default-model-config", "The name of the default model config.")
	commandLine.StringVar(&cfg.DefaultModelConfig.Namespace, "default-model-config-namespace", kagentNamespace, "The namespace of the default model config.")
	commandLine.StringVar(&cfg.HttpServerAddr, "http-server-address", ":8083", "The address the HTTP server binds to.")
	commandLine.StringVar(&cfg.A2ABaseUrl, "a2a-base-url", "http://127.0.0.1:8083", "The base URL of the A2A Server endpoint, as advertised to clients.")
	commandLine.StringVar(&cfg.Database.Url, "postgres-database-url", "postgres://postgres:kagent@kagent-postgresql.kagent.svc.cluster.local:5432/postgres", "The URL of the PostgreSQL database.")
	commandLine.StringVar(&cfg.Database.UrlFile, "postgres-database-url-file", "", "Path to a file containing the PostgreSQL database URL. Takes precedence over --postgres-database-url.")
	commandLine.BoolVar(&cfg.Database.VectorEnabled, "database-vector-enabled", true, "Enable pgvector extension and memory table. Requires pgvector to be installed on the PostgreSQL server.")

	commandLine.StringVar(&cfg.WatchNamespaces, "watch-namespaces", "", "The namespaces to watch for .")

	commandLine.Var(&cfg.Streaming.MaxBufSize, "streaming-max-buf-size", "The maximum size of the streaming buffer.")
	commandLine.Var(&cfg.Streaming.InitialBufSize, "streaming-initial-buf-size", "The initial size of the streaming buffer.")
	commandLine.DurationVar(&cfg.Streaming.Timeout, "streaming-timeout", 60*time.Second, "The timeout for the streaming connection.")

	commandLine.StringVar(&cfg.Proxy.URL, "proxy-url", "", "Proxy URL for internally-built k8s URLs (e.g., http://proxy.kagent.svc.cluster.local:8080)")

	commandLine.StringVar(&cfg.Auth.Mode, "auth-mode", "unsecure", "Authentication mode: unsecure or trusted-proxy")
	commandLine.StringVar(&cfg.Auth.UserIDClaim, "auth-user-id-claim", "sub", "JWT claim name for user identity")

	commandLine.StringVar(&cfg.DefaultWorkloadMode, "default-workload-mode", "deployment", `Default workload mode for Agent CRs that don't set spec.workloadMode. One of "deployment" (per-agent Deployment, the default) or "sandbox" (route through the configured sandbox backend, e.g. substrate).`)

	commandLine.StringVar(&agent_translator.DefaultImageConfig.Registry, "image-registry", agent_translator.DefaultImageConfig.Registry, "The registry to use for the image.")
	commandLine.StringVar(&agent_translator.DefaultImageConfig.Tag, "image-tag", agent_translator.DefaultImageConfig.Tag, "The tag to use for the image.")
	commandLine.StringVar(&agent_translator.DefaultImageConfig.PullPolicy, "image-pull-policy", agent_translator.DefaultImageConfig.PullPolicy, "The pull policy to use for the image.")
	commandLine.StringVar(&agent_translator.DefaultImageConfig.PullSecret, "image-pull-secret", "", "The pull secret name for the agent image.")
	commandLine.StringVar(&agent_translator.DefaultImageConfig.Repository, "image-repository", agent_translator.DefaultImageConfig.Repository, "The repository to use for the agent image.")
	commandLine.StringVar(&agent_translator.DefaultSkillsInitImageConfig.Registry, "skills-init-image-registry", agent_translator.DefaultSkillsInitImageConfig.Registry, "The registry to use for the skills init image.")
	commandLine.StringVar(&agent_translator.DefaultSkillsInitImageConfig.Tag, "skills-init-image-tag", agent_translator.DefaultSkillsInitImageConfig.Tag, "The tag to use for the skills init image.")
	commandLine.StringVar(&agent_translator.DefaultSkillsInitImageConfig.PullPolicy, "skills-init-image-pull-policy", agent_translator.DefaultSkillsInitImageConfig.PullPolicy, "The pull policy to use for the skills init image.")
	commandLine.StringVar(&agent_translator.DefaultSkillsInitImageConfig.Repository, "skills-init-image-repository", agent_translator.DefaultSkillsInitImageConfig.Repository, "The repository to use for the skills init image.")

	commandLine.StringVar(&cfg.Openshell.GatewayURL, "openshell-gateway-url", "", "gRPC target for the OpenShell sandbox gateway (e.g. dns:///openshell.openshell.svc:443). When empty, the Sandbox controller is disabled.")
	commandLine.StringVar(&cfg.Openshell.Token, "openshell-token", "", "Static bearer token for the OpenShell gateway. Prefer --openshell-token-file for secrets.")
	commandLine.StringVar(&cfg.Openshell.TokenFile, "openshell-token-file", "", "Path to a file containing the OpenShell gateway bearer token. Takes precedence over --openshell-token.")
	commandLine.StringVar(&cfg.Openshell.CAFile, "openshell-tls-ca-file", "", "Path to a PEM file containing CA bundle for verifying the OpenShell gateway TLS certificate. Optional.")
	commandLine.BoolVar(&cfg.Openshell.Insecure, "openshell-insecure", false, "Dial the OpenShell gateway without TLS. Use only for local development.")
	commandLine.DurationVar(&cfg.Openshell.DialTimeout, "openshell-dial-timeout", 10*time.Second, "Timeout for the initial dial to the OpenShell gateway.")
	commandLine.DurationVar(&cfg.Openshell.CallTimeout, "openshell-call-timeout", 30*time.Second, "Per-RPC timeout for OpenShell gateway calls.")

	// agent-substrate (ate.dev) sandbox backend flags. When --substrate-worker-pool-name
	// is set, the controller emits ate.dev/v1alpha1 ActorTemplate objects for
	// SandboxAgents instead of the default agent-sandbox Sandbox CRs.
	commandLine.StringVar(&cfg.Substrate.WorkerPoolName, "substrate-worker-pool-name", "", "Name of the shared substrate WorkerPool. Setting this enables the substrate backend.")
	commandLine.StringVar(&cfg.Substrate.WorkerPoolNamespace, "substrate-worker-pool-namespace", "", "Namespace of the shared substrate WorkerPool.")
	commandLine.StringVar(&cfg.Substrate.SnapshotsLocation, "substrate-snapshots-location", "gs://ate-snapshots", "URI prefix for substrate actor snapshots (e.g. gs://my-bucket or s3://my-bucket). The per-agent path is appended automatically.")
	commandLine.StringVar(&cfg.Substrate.PauseImage, "substrate-pause-image", "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4", "Pause container image substrate uses as the sandbox root.")
	commandLine.StringVar(&cfg.Substrate.RunscAMD64URL, "substrate-runsc-amd64-url", "gs://gvisor/releases/nightly/2026-05-19/x86_64/runsc", "gs:// URL of the amd64 runsc binary substrate downloads.")
	commandLine.StringVar(&cfg.Substrate.RunscAMD64SHA256, "substrate-runsc-amd64-sha256", "a397be1abc2420d26bce6c70e6e2ff96c73aaaab929756c56f5e2089ea842b63", "SHA256 of the amd64 runsc binary.")
	commandLine.StringVar(&cfg.Substrate.RunscARM64URL, "substrate-runsc-arm64-url", "gs://gvisor/releases/nightly/2026-05-19/aarch64/runsc", "gs:// URL of the arm64 runsc binary substrate downloads.")
	commandLine.StringVar(&cfg.Substrate.RunscARM64SHA256, "substrate-runsc-arm64-sha256", "1ba2366ae2efceba166046f51a4104f9261c9cb72c6db8f5b3fe2dc57dea86b9", "SHA256 of the arm64 runsc binary.")
	commandLine.StringVar(&cfg.Substrate.ControlEndpoint, "substrate-control-endpoint", "api.ate-system.svc.cluster.local:443", "gRPC target for substrate's ate-api-server Control API.")
	commandLine.BoolVar(&cfg.Substrate.ControlPlaintext, "substrate-control-plaintext", false, "Disable TLS for the substrate Control gRPC connection. Local/dev only.")
	commandLine.StringVar(&cfg.Substrate.RouterURL, "substrate-router-url", "http://atenet-router.ate-system.svc", "Dial target for substrate's atenet-router. The A2A registrar uses this URL for sandbox-mode agents; actor identity is carried in the Host header.")
	commandLine.DurationVar(&cfg.Substrate.IdleTimeout, "substrate-idle-timeout", 0, "Auto-suspend substrate actors that haven't seen a request for this long. Zero disables auto-suspend (operators must call `kubectl ate suspend actor` manually).")
	commandLine.DurationVar(&cfg.Substrate.IdleSweepInterval, "substrate-idle-sweep-interval", 30*time.Second, "How often the IdleSuspender checks for expired substrate actors.")
	commandLine.StringVar(&cfg.Substrate.AgentImage, "substrate-agent-image", "", "Container image for substrate-mode agents that contains the substrate config-via-env shim (built via python/Dockerfile.substrate). When empty, the translator's default image is used unchanged.")
	commandLine.StringVar(&cfg.Substrate.WorkerPoolAteomImage, "substrate-worker-pool-ateom-image", "", "ateom-gvisor container image to use when auto-provisioning the shared WorkerPool. When set, the controller creates+updates the WorkerPool referenced by --substrate-worker-pool-name. When empty, no auto-provisioning happens and the operator must apply the WorkerPool themselves.")
	commandLine.IntVar(&cfg.Substrate.WorkerPoolReplicas, "substrate-worker-pool-replicas", 2, "Initial replica count for the auto-provisioned WorkerPool. Only used at create time — once the WorkerPool exists, operators can scale it manually without the ensurer reverting.")

	commandLine.StringVar(&agent_translator.DefaultServiceAccountName, "default-service-account-name", "", "Global default ServiceAccount name for agent pods. When set, agents without an explicit serviceAccountName will use this instead of creating a per-agent ServiceAccount.")

	commandLine.Var(&MapValue{Target: &agent_translator.DefaultAgentPodLabels}, "default-agent-pod-labels", "Comma-separated key=value pairs of labels to apply to all agent pod templates (e.g. 'team=platform,env=prod'). Per-agent labels take precedence.")

	commandLine.StringVar(&agent_translator.DefaultAgentBindHost, "default-agent-bind-host", agent_translator.DefaultAgentBindHost, "Default host address for agent pods to bind to. Use '0.0.0.0' for IPv4 only or '::' for dual-stack (IPv4+IPv6).")
}

// LoadFromEnv loads configuration values from environment variables.
// Flag names are converted to uppercase with underscores (e.g., metrics-bind-address -> METRICS_BIND_ADDRESS).
func LoadFromEnv(fs *flag.FlagSet) error {
	var loadErr error

	fs.VisitAll(func(f *flag.Flag) {
		envName := strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))

		if envVal := os.Getenv(envName); envVal != "" {
			if err := f.Value.Set(envVal); err != nil {
				loadErr = multierror.Append(loadErr, fmt.Errorf("failed to set flag %s from env %s=%s: %w", f.Name, envName, envVal, err))
			}
		}
	})

	return loadErr
}

// MapValue implements flag.Value for a map[string]string.
// It parses comma-separated key=value pairs (e.g. "team=platform,env=prod").
type MapValue struct {
	Target *map[string]string
}

func (m *MapValue) String() string {
	if m.Target == nil || *m.Target == nil {
		return ""
	}
	keys := make([]string, 0, len(*m.Target))
	for k := range *m.Target {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+(*m.Target)[k])
	}
	return strings.Join(pairs, ",")
}

func (m *MapValue) Set(raw string) error {
	result := make(map[string]string)
	for pair := range strings.SplitSeq(raw, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return fmt.Errorf("invalid format %q: expected key=value", pair)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			return fmt.Errorf("invalid entry: empty key in %q", pair)
		}
		result[k] = v
	}
	*m.Target = result
	return nil
}

type BootstrapConfig struct {
	Ctx      context.Context
	Manager  manager.Manager
	Router   *mux.Router
	DbClient dbpkg.Client
	Config   *Config
}

type CtrlManagerConfigFunc func(manager.Manager) error

type ExtensionConfig struct {
	Authenticator    auth.AuthProvider
	Authorizer       auth.Authorizer
	AgentPlugins     []agent_translator.TranslatorPlugin
	MCPServerPlugins []translator.MCPTranslatorPlugin
	SandboxBackend   sandboxbackend.Backend
	// SandboxRouting is an optional override that lets the active sandbox
	// backend customize how the A2A registrar dials sandbox-mode agents.
	// nil = use the default per-agent Service URL. Substrate supplies a
	// non-nil func that routes through atenet-router with a Host-header-
	// encoded actor identity.
	SandboxRouting sandboxbackend.SandboxRoutingFunc
	// ManagerRunnables are backend-specific background loops that should join
	// the manager lifecycle (Start/Stop with the manager). Substrate uses
	// this for its IdleSuspender sweep loop. Each runnable's Start runs in
	// its own goroutine; nil entries are ignored.
	ManagerRunnables []manager.Runnable
}

type GetExtensionConfig func(bootstrap BootstrapConfig) (*ExtensionConfig, error)

// MigrationRunner applies database migrations given the resolved connection URL.
// vectorEnabled mirrors the --database-vector-enabled flag; custom runners can use it
// to conditionally apply vector-specific migrations.
// Returning a non-nil error causes the app to exit.
//
// Pass nil to Start to use the default migration runner (migrations.RunUp with migrations.FS).
// Provide a custom runner to take over the migration process entirely — for example,
// to run additional enterprise migrations alongside or instead of the built-in ones.
// Custom runners that want to include the built-in migrations can call migrations.RunUp directly.
type MigrationRunner func(ctx context.Context, url string, vectorEnabled bool) error

func Start(getExtensionConfig GetExtensionConfig, migrationRunner MigrationRunner) {
	var tlsOpts []func(*tls.Config)
	var cfg Config

	// Reused below for mgr.Start; SetupSignalHandler must be called once per process.
	ctx := ctrl.SetupSignalHandler()

	cfg.SetFlags(flag.CommandLine)

	opts := zap.Options{}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	// Load configuration from environment variables (overrides flags)
	if err := LoadFromEnv(flag.CommandLine); err != nil {
		setupLog.Error(err, "failed to load configuration from environment variables")
		os.Exit(1)
	}

	// Wire DefaultWorkloadMode into v1alpha2 so AgentObject.GetWorkloadMode()
	// falls back to it when the per-Agent spec.workloadMode is unset.
	// Empty string is fine — SetDefaultWorkloadMode ignores unknown values
	// and the package default ("deployment") stays in place.
	v1alpha2.SetDefaultWorkloadMode(v1alpha2.WorkloadMode(cfg.DefaultWorkloadMode))

	logger := zap.New(zap.UseFlagOptions(&opts))
	ctrl.SetLogger(logger)

	shutdownTracing, err := telemetry.InitTracerProvider(ctx, Version)
	if err != nil {
		setupLog.Error(err, "failed to initialize tracing")
		os.Exit(1)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(shutdownCtx); err != nil {
			setupLog.Error(err, "failed to shutdown tracing")
		}
	}()

	setupLog.Info("Starting KAgent Controller", "version", Version, "git_commit", GitCommit, "build_date", BuildDate, "config", cfg)

	goruntime.SetMaxProcs(logger)

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("disabling http/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !cfg.EnableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Create watchers for metrics and webhooks certificates
	var metricsCertWatcher, webhookCertWatcher *certwatcher.CertWatcher

	ctrlmetrics.Registry.MustRegister(versionmetrics.NewBuildInfoCollector())

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   cfg.Metrics.Addr,
		SecureServing: cfg.SecureMetrics,
		TLSOpts:       tlsOpts,
	}

	if cfg.SecureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(cfg.Metrics.CertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", cfg.Metrics.CertPath, "metrics-cert-name", cfg.Metrics.CertName, "metrics-cert-key", cfg.Metrics.CertKey)

		var err error
		metricsCertWatcher, err = certwatcher.New(
			filepath.Join(cfg.Metrics.CertPath, cfg.Metrics.CertName),
			filepath.Join(cfg.Metrics.CertPath, cfg.Metrics.CertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize metrics certificate watcher", "error", err)
			os.Exit(1)
		}

		metricsServerOptions.TLSOpts = append(metricsServerOptions.TLSOpts, func(config *tls.Config) {
			config.GetCertificate = metricsCertWatcher.GetCertificate
		})
	}

	if len(cfg.Webhook.CertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", cfg.Webhook.CertPath, "webhook-cert-name", cfg.Webhook.CertName, "webhook-cert-key", cfg.Webhook.CertKey)

		var err error
		webhookCertWatcher, err = certwatcher.New(
			filepath.Join(cfg.Webhook.CertPath, cfg.Webhook.CertName),
			filepath.Join(cfg.Webhook.CertPath, cfg.Webhook.CertKey),
		)
		if err != nil {
			setupLog.Error(err, "to initialize webhook certificate watcher", "error", err)
			os.Exit(1)
		}
	}

	// filter out invalid namespaces from the watchNamespaces flag (comma separated list)
	watchNamespacesList := filterValidNamespaces(strings.Split(cfg.WatchNamespaces, ","))

	clientOpts := client.Options{}
	if len(watchNamespacesList) > 0 {
		// In namespaced RBAC mode a Role cannot grant access to cluster-scoped
		// resources, so prevent the cached client from starting a cluster-scoped
		// Namespace informer whose list/watch would keep crashing.
		clientOpts.Cache = &client.CacheOptions{
			DisableFor: []client.Object{&corev1.Namespace{}},
		}
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		HealthProbeBindAddress: cfg.ProbeAddr,
		LeaderElection:         cfg.LeaderElection,
		LeaderElectionID:       "0e9f6799.kagent.dev",
		Client:                 clientOpts,
		Cache: cache.Options{
			DefaultNamespaces: configureNamespaceWatching(watchNamespacesList),
		},
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Resolve the database URL once so both the migration runner and the pool
	// connection use exactly the same value.
	dbURL, err := database.ResolveURL(cfg.Database.Url, cfg.Database.UrlFile)
	if err != nil {
		setupLog.Error(err, "unable to resolve database URL")
		os.Exit(1)
	}

	// Use the built-in migration runner when none is provided.
	if migrationRunner == nil {
		migrationRunner = func(_ context.Context, url string, vectorEnabled bool) error {
			return migrations.RunUp(url, migrations.FS, vectorEnabled)
		}
	}

	// Run migrations before connecting; schema must exist before queries.
	setupLog.Info("running database migrations")
	if err := migrationRunner(ctx, dbURL, cfg.Database.VectorEnabled); err != nil {
		setupLog.Error(err, "database migration failed")
		os.Exit(1)
	}
	setupLog.Info("database migrations complete")

	// Connect to database
	db, err := database.Connect(ctx, &database.PostgresConfig{
		URL:           dbURL,
		VectorEnabled: cfg.Database.VectorEnabled,
	})
	if err != nil {
		setupLog.Error(err, "unable to connect to database")
		os.Exit(1)
	}

	dbClient := database.NewClient(db)
	router := mux.NewRouter()
	extensionCfg, err := getExtensionConfig(BootstrapConfig{
		Ctx:      ctx,
		Manager:  mgr,
		Router:   router,
		DbClient: dbClient,
		Config:   &cfg,
	})
	if err != nil {
		setupLog.Error(err, "unable to get start config")
		os.Exit(1)
	}

	apiTranslator := agent_translator.NewAdkApiTranslatorWithWatchedNamespaces(
		mgr.GetClient(),
		watchNamespacesList,
		cfg.DefaultModelConfig,
		extensionCfg.AgentPlugins,
		cfg.Proxy.URL,
		extensionCfg.SandboxBackend,
	)

	rcnclr := reconciler.NewKagentReconciler(
		apiTranslator,
		mgr.GetClient(),
		dbClient,
		cfg.DefaultModelConfig,
		watchNamespacesList,
		extensionCfg.SandboxBackend,
	)

	if err := (&controller.ServiceController{
		Scheme:     mgr.GetScheme(),
		Reconciler: rcnclr,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "MCPServerToolDiscovery")
		os.Exit(1)
	}

	if err := (&controller.MCPServerToolController{
		Scheme:     mgr.GetScheme(),
		Reconciler: rcnclr,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Service")
		os.Exit(1)
	}

	// Single controller after the SandboxAgent unification (SUBSTRATE.md §21).
	// AgentController's reconciler dispatches on agent.GetWorkloadMode() for
	// the sandbox-vs-deployment branch.
	if err = (&controller.AgentController{
		Scheme:        mgr.GetScheme(),
		Reconciler:    rcnclr,
		AdkTranslator: apiTranslator,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Agent")
		os.Exit(1)
	}

	// Build openshell + substrate harness backends. The AgentHarness controller
	// runs when at least one is configured.
	openshellHarnessEnabled := cfg.Openshell.GatewayURL != ""
	substrateHarnessEnabled := cfg.Substrate.ControlEndpoint != "" && cfg.Substrate.WorkerPoolAteomImage != ""
	// substrateHarnessClient is captured at function scope so the HTTPServer
	// (built below) can reuse it for the /api/substrate observability
	// endpoints. Nil when substrate isn't enabled.
	var substrateHarnessClient *harness.Client
	if openshellHarnessEnabled || substrateHarnessEnabled {
		kubeClient := mgr.GetClient()
		var openshellBackends map[v1alpha2.AgentHarnessBackendType]sandboxbackend.AsyncBackend
		var substrateBackends map[v1alpha2.AgentHarnessBackendType]sandboxbackend.AsyncBackend
		var substrateProvisioner *harness.Provisioner

		if openshellHarnessEnabled {
			var err error
			openshellBackends, err = buildOpenshellSandboxBackends(ctx, &cfg, kubeClient)
			if err != nil {
				setupLog.Error(err, "unable to build openshell sandbox backends")
				os.Exit(1)
			}
		}

		if substrateHarnessEnabled {
			harnessCfg := harness.Config{
				AteAPIEndpoint: cfg.Substrate.ControlEndpoint,
				Insecure:       cfg.Substrate.ControlPlaintext,
				DialTimeout:    10 * time.Second,
				// CallTimeout bumped from the original 30s pj-kagent default —
				// ResumeActor for an openclaw harness goes through
				// atelet.RestoreWorkload, which on kind/macOS lands at
				// 14–20s for the OCI rootfs unpack on cold cache and can
				// occasionally cross 30s when atelet is busy. 120s gives
				// plenty of headroom for slow first restores without
				// stuck-resume retry loops. See SUBSTRATE.md §22 + the
				// atenet 60s bgCtx companion patch.
				CallTimeout:                   120 * time.Second,
				DefaultActorTemplateNamespace: cfg.Substrate.WorkerPoolNamespace,
			}
			harnessClient, err := harness.Dial(ctx, harnessCfg)
			if err != nil {
				setupLog.Error(err, "unable to dial substrate Control API for harness backend")
				os.Exit(1)
			}
			substrateHarnessClient = harnessClient
			openClawBackend := harness.NewOpenClawBackend(harnessClient, harnessCfg, v1alpha2.AgentHarnessBackendOpenClaw, mgr.GetEventRecorderFor("agentharness-openclaw-substrate"))
			substrateBackends = map[v1alpha2.AgentHarnessBackendType]sandboxbackend.AsyncBackend{
				v1alpha2.AgentHarnessBackendOpenClaw: openClawBackend,
			}
			substrateProvisioner = &harness.Provisioner{
				Client: kubeClient,
				Ate:    harnessClient,
				Defaults: harness.ProvisionDefaults{
					PauseImage:           cfg.Substrate.PauseImage,
					RunscAMD64URL:        cfg.Substrate.RunscAMD64URL,
					RunscAMD64SHA256:     cfg.Substrate.RunscAMD64SHA256,
					RunscARM64URL:        cfg.Substrate.RunscARM64URL,
					RunscARM64SHA256:     cfg.Substrate.RunscARM64SHA256,
					DefaultAteomImage:    cfg.Substrate.WorkerPoolAteomImage,
					DefaultWorkloadImage: openclaw.NemoclawSandboxBaseImage,
				},
			}
		}

		if err := (&controller.AgentHarnessController{
			Client:               mgr.GetClient(),
			Recorder:             mgr.GetEventRecorder("agentharness-controller"),
			OpenshellBackends:    openshellBackends,
			SubstrateBackends:    substrateBackends,
			SubstrateProvisioner: substrateProvisioner,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "AgentHarness")
			os.Exit(1)
		}
	} else {
		setupLog.Info("AgentHarness controller disabled: set --openshell-gateway-url and/or substrate.WorkerPoolAteomImage")
	}

	if err = (&controller.ModelConfigController{
		Scheme:     mgr.GetScheme(),
		Reconciler: rcnclr,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelConfig")
		os.Exit(1)
	}

	if err = (&controller.ModelProviderConfigController{
		Scheme:     mgr.GetScheme(),
		Reconciler: rcnclr,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "ModelProviderConfig")
		os.Exit(1)
	}

	if err = (&controller.RemoteMCPServerController{
		Scheme:     mgr.GetScheme(),
		Reconciler: rcnclr,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "RemoteMCPServer")
		os.Exit(1)
	}

	if err := reconcilerutils.SetupOwnerIndexes(mgr, rcnclr.GetOwnedResourceTypes()); err != nil {
		setupLog.Error(err, "failed to setup indexes for owned resources")
		os.Exit(1)
	}

	// Register A2A handlers on all replicas. After the Agent+SandboxAgent
	// unification (SUBSTRATE.md §21) all agent traffic flows through the
	// single APIPathA2A path; sandbox-mode agents are distinguished by
	// workloadMode at the per-agent transport, not by a separate route.
	a2aHandler := a2a.NewA2AHttpMux(httpserver.APIPathA2A, extensionCfg.Authenticator)

	if err := mgr.Add(a2a.NewA2ARegistrar(
		mgr.GetCache(),
		a2aHandler,
		cfg.A2ABaseUrl+httpserver.APIPathA2A,
		extensionCfg.Authenticator,
		int(cfg.Streaming.MaxBufSize.Value()),
		int(cfg.Streaming.InitialBufSize.Value()),
		cfg.Streaming.Timeout,
		extensionCfg.SandboxRouting,
	)); err != nil {
		setupLog.Error(err, "unable to set up a2a registrar")
		os.Exit(1)
	}

	// Add any backend-supplied manager runnables (e.g. substrate's idle
	// suspender sweep loop). nil entries are ignored to keep the slot
	// trivially safe.
	for _, r := range extensionCfg.ManagerRunnables {
		if r == nil {
			continue
		}
		if err := mgr.Add(r); err != nil {
			setupLog.Error(err, "unable to add backend-supplied runnable")
			os.Exit(1)
		}
	}

	// Create MCP handler that bridges to A2A
	mcpHandler, err := mcp.NewMCPHandler(
		mgr.GetClient(),
		cfg.A2ABaseUrl+httpserver.APIPathA2A,
		extensionCfg.Authenticator,
		cfg.Streaming.Timeout,
	)
	if err != nil {
		setupLog.Error(err, "unable to create MCP handler")
		os.Exit(1)
	}

	// +kubebuilder:scaffold:builder
	if metricsCertWatcher != nil {
		setupLog.Info("Adding metrics certificate watcher to manager")
		if err := mgr.Add(metricsCertWatcher); err != nil {
			setupLog.Error(err, "unable to add metrics certificate watcher to manager")
			os.Exit(1)
		}
	}

	if webhookCertWatcher != nil {
		setupLog.Info("Adding webhook certificate watcher to manager")
		if err := mgr.Add(webhookCertWatcher); err != nil {
			setupLog.Error(err, "unable to add webhook certificate watcher to manager")
			os.Exit(1)
		}
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	if err := mgr.Add(&adminServer{port: ":6060"}); err != nil {
		setupLog.Error(err, "unable to set up admin server")
		os.Exit(1)
	}

	// AgentHarness gateway proxy: only enable when the substrate harness
	// backend is configured (same gate as the AgentHarness controller's
	// substrate dispatch). Without this, /api/agentharnesses/<ns>/<name>/gateway/
	// returns 503 ("substrate gateway proxy is not configured").
	var agentHarnessGatewayCfg *handlers.AgentHarnessGatewayConfig
	if substrateHarnessEnabled {
		agentHarnessGatewayCfg = &handlers.AgentHarnessGatewayConfig{
			AteAPIEndpoint: cfg.Substrate.ControlEndpoint,
			AteAPIInsecure: cfg.Substrate.ControlPlaintext,
			DialTimeout:    10 * time.Second,
			CallTimeout:    30 * time.Second,
		}
	}

	// AAuth Phase 2: optionally bring up the controller-side Agent Provider.
	// Disabled when AAUTH_ISSUER_URL is unset — agents with spec.aauth.enabled=true
	// will then fall back to Phase 1 (hwk) behavior.
	//
	// The issuer keypair is persisted in a Secret so JWKS stays stable across
	// controller restarts (otherwise every restart would invalidate every
	// outstanding aa-agent+jwt).
	var (
		aauthIssuer   *aauth.Issuer
		aauthSubject  aauth.SubjectAuthenticator
		aauthVerifier *aauth.Verifier
	)
	if issURL := os.Getenv("AAUTH_ISSUER_URL"); issURL != "" {
		priv, pub, kid, err := aauth.LoadOrGenerateIssuerKey(ctx, mgr.GetConfig(), kagentNamespace, aauth.IssuerSecretName)
		if err != nil {
			setupLog.Error(err, "unable to load or generate AAuth issuer key")
			os.Exit(1)
		}
		aauthIssuer, err = aauth.NewIssuerFromKey(priv, pub, kid, issURL, 24*time.Hour)
		if err != nil {
			setupLog.Error(err, "unable to create AAuth issuer")
			os.Exit(1)
		}
		// SubjectAuthenticator for POST /aauth/agent-jwt — validates the
		// caller's K8s ServiceAccount token via TokenReview and derives
		// the canonical sub from system:serviceaccount:<ns>:<name>.
		k8sClient, err := kubernetes.NewForConfig(mgr.GetConfig())
		if err != nil {
			setupLog.Error(err, "unable to build kubernetes clientset for AAuth TokenReview")
			os.Exit(1)
		}
		// Require the audience the agent's projected SA token is minted with.
		// Tokens with the default audience (e.g. leaked from /var/run/secrets)
		// will fail TokenReview and the mint request will be rejected with 403.
		aauthSubject = &aauth.K8sSubjectAuthenticator{
			Client:    k8sClient,
			Audiences: []string{"kagent-controller"},
		}
		setupLog.Info("AAuth issuer enabled", "issuerURL", issURL, "kid", kid, "secret", kagentNamespace+"/"+aauth.IssuerSecretName)

		// Phase 3: install the AAuth verification middleware on the controller's
		// HTTP server. Default mode is log-only so a misconfiguration won't 401
		// existing traffic; set AAUTH_VERIFY_MODE=enforce to gate.
		verifyMode := aauth.ParseVerifyMode(os.Getenv("AAUTH_VERIFY_MODE"))
		aauthVerifier, err = aauth.NewLocalVerifier(aauthIssuer, verifyMode)
		if err != nil {
			setupLog.Error(err, "unable to create AAuth verifier")
			os.Exit(1)
		}
		aauthVerifier.Logger = aauth.LogrAdapter{Logger: ctrl.Log.WithName("aauth")}
		setupLog.Info("AAuth verifier enabled", "mode", string(verifyMode))
	}

	httpServer, err := httpserver.NewHTTPServer(httpserver.ServerConfig{
		Router:                    router,
		BindAddr:                  cfg.HttpServerAddr,
		KubeClient:                mgr.GetClient(),
		A2AHandler:                a2aHandler,
		MCPHandler:                mcpHandler,
		WatchedNamespaces:         watchNamespacesList,
		DbClient:                  dbClient,
		Authorizer:                extensionCfg.Authorizer,
		Authenticator:             extensionCfg.Authenticator,
		ProxyURL:                  cfg.Proxy.URL,
		Reconciler:                rcnclr,
		SandboxBackend:            extensionCfg.SandboxBackend,
		AgentHarnessGateway:       agentHarnessGatewayCfg,
		SubstrateHarnessClient:    substrateHarnessClient,
		AAuthIssuer:               aauthIssuer,
		AAuthSubjectAuthenticator: aauthSubject,
		AAuthVerifier:             aauthVerifier,
	})
	if err != nil {
		setupLog.Error(err, "unable to create HTTP server")
		os.Exit(1)
	}
	if err := mgr.Add(httpServer); err != nil {
		setupLog.Error(err, "unable to set up HTTP server")
		os.Exit(1)
	}

	// Memory TTL cleanup runs only on the leader to avoid duplicate deletes.
	if err := mgr.Add(httpserver.NewMemoryCleanupRunnable(dbClient, 0)); err != nil {
		setupLog.Error(err, "unable to set up memory cleanup runnable")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

// buildOpenshellSandboxBackends constructs AsyncBackend values for openshell, openclaw,
// and nemoclaw from flag config. It dials the gateway once; OpenShell and Inference RPCs
// share that connection (see openshell.OpenShellClients). The connection is not explicitly
// closed today — same lifetime as the process.
func buildOpenshellSandboxBackends(ctx context.Context, cfg *Config, kubeClient client.Client) (map[v1alpha2.AgentHarnessBackendType]sandboxbackend.AsyncBackend, error) {
	oc := openshell.Config{
		GatewayURL:  cfg.Openshell.GatewayURL,
		Token:       cfg.Openshell.Token,
		Insecure:    cfg.Openshell.Insecure,
		DialTimeout: cfg.Openshell.DialTimeout,
		CallTimeout: cfg.Openshell.CallTimeout,
	}
	if cfg.Openshell.TokenFile != "" {
		data, err := os.ReadFile(cfg.Openshell.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("read openshell token file: %w", err)
		}
		oc.Token = strings.TrimSpace(string(data))
	}
	if cfg.Openshell.CAFile != "" {
		data, err := os.ReadFile(cfg.Openshell.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read openshell CA file: %w", err)
		}
		oc.TLSCAPEM = data
	}
	clients, err := openshell.Dial(ctx, oc)
	if err != nil {
		return nil, err
	}

	osh := openshell.NewOpenshellBackend(kubeClient, clients, oc, nil)
	ocl, err := openshell.NewOpenClawBackend(kubeClient, clients, oc, nil)
	if err != nil {
		return nil, err
	}
	return map[v1alpha2.AgentHarnessBackendType]sandboxbackend.AsyncBackend{
		v1alpha2.AgentHarnessBackendOpenshell: osh,
		v1alpha2.AgentHarnessBackendOpenClaw:  ocl,
	}, nil
}

// configureNamespaceWatching sets up the controller manager to watch specific namespaces
// based on the provided configuration. It returns the list of namespaces being watched,
// or nil if watching all namespaces.
func configureNamespaceWatching(watchNamespacesList []string) map[string]cache.Config {
	if len(watchNamespacesList) == 0 {
		setupLog.Info("Watching all namespaces (no valid namespaces specified)")
		return map[string]cache.Config{"": {}}
	}
	setupLog.Info("Watching specific namespaces at cache level", "namespaces", watchNamespacesList)

	namespacesMap := make(map[string]cache.Config)
	for _, ns := range watchNamespacesList {
		namespacesMap[ns] = cache.Config{}
	}

	return namespacesMap
}

// filterValidNamespaces removes invalid namespace names from the provided list.
// A valid namespace must be a valid DNS-1123 label.
func filterValidNamespaces(namespaces []string) []string {
	var validNamespaces []string

	for _, ns := range namespaces {
		if strings.TrimSpace(ns) == "" {
			continue
		}

		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			setupLog.Info("Ignoring invalid namespace name",
				"namespace", ns,
				"validation_errors", strings.Join(errs, ", "))
		} else {
			validNamespaces = append(validNamespaces, ns)
		}
	}

	return validNamespaces
}

var _ manager.Runnable = &adminServer{}

type adminServer struct {
	port string
}

func (a *adminServer) Start(ctx context.Context) error {
	setupLog.Info("starting pprof server")
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	mux.HandleFunc("/debug/pprof/goroutine", pprof.Handler("goroutine").ServeHTTP)
	mux.HandleFunc("/debug/pprof/heap", pprof.Handler("heap").ServeHTTP)
	mux.HandleFunc("/debug/pprof/block", pprof.Handler("block").ServeHTTP)
	mux.HandleFunc("/debug/pprof/threadcreate", pprof.Handler("threadcreate").ServeHTTP)
	mux.HandleFunc("/debug/pprof/mutex", pprof.Handler("mutex").ServeHTTP)
	mux.HandleFunc("/debug/pprof/allocs", pprof.Handler("allocs").ServeHTTP)
	setupLog.Info("pprof server started", "address", a.port)
	return http.ListenAndServe(a.port, mux)
}
