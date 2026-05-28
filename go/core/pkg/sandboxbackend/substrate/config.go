package substrate

// Config configures the substrate sandbox backend. Values are typically wired
// from controller flags (see go/core/cmd/controller/main.go).
type Config struct {
	// WorkerPoolNamespace + WorkerPoolName identify the shared substrate
	// WorkerPool that every emitted ActorTemplate will reference. For the PoC
	// there is one pool per controller; per-agent pool selection is future work.
	WorkerPoolNamespace string
	WorkerPoolName      string

	// SnapshotsLocation is the gs://... or s3://... prefix under which substrate
	// stores actor snapshots. The emitted ActorTemplate appends the agent's
	// name+namespace as a subdirectory.
	SnapshotsLocation string

	// PauseImage is substrate's sandbox-root container image.
	// Example on-prem/kind: registry.k8s.io/pause:3.10.2@sha256:...
	// Example on GCP:       gcr.io/gke-release/pause@sha256:...
	PauseImage string

	// Runsc download config — substrate fetches the gVisor `runsc` binary from
	// these URLs and verifies the SHA256 before running. atelet caches the
	// result at /run/ateom-gvisor/static-files/runsc-<sha256> on the node.
	RunscAMD64URL    string
	RunscAMD64SHA256 string
	RunscARM64URL    string
	RunscARM64SHA256 string

	// Control is the gRPC client used to drive substrate actor lifecycle
	// (CreateActor on Ready, SuspendActor on idle). Optional: when nil, the
	// backend still emits ActorTemplate objects but does NOT create actors
	// automatically — operators would have to run `kubectl ate create actor`
	// themselves. Phase 3 onward wires this in from controller flags.
	Control *ControlClient

	// RouterURL is the in-cluster (or port-forwarded) base URL of substrate's
	// atenet-router, used by the A2A registrar to construct dial targets for
	// sandbox-mode agents. The actor identity travels via the Host header
	// (see HostRewritingTransport), not via the URL host.
	RouterURL string

	// AgentImage, when non-empty, overrides the container.image emitted on
	// the ActorTemplate and forces a known command line that a substrate
	// config-via-env shim (python/Dockerfile.substrate) understands. The
	// shim materializes /tmp/config/{config.json,agent-card.json,
	// srt-settings.json} from KAGENT_CONFIG_JSON / KAGENT_AGENT_CARD_JSON
	// / KAGENT_SRT_SETTINGS_JSON env vars before exec'ing
	// `kagent-adk static --local`.
	//
	// Why required: substrate's ActorTemplate.spec.containers schema has no
	// volumes / volumeMounts, so the kagent translator's /config Secret
	// can't be carried across. The shim works around that constraint.
	//
	// When empty, the substrate backend passes the PodTemplate's image
	// through unchanged — useful for stand-in agents that don't need any
	// kagent config (the Phase 0 demo path) or for non-ADK BYO images.
	AgentImage string

	// WorkerPoolReplicas + WorkerPoolAteomImage drive the optional
	// WorkerPoolEnsurer (see workerpool_ensurer.go). When AteomImage is set,
	// the controller adopts ownership of the shared WorkerPool: if it
	// doesn't exist, the ensurer creates it; if it exists with a different
	// ateomImage, the ensurer patches it. Replicas is only used at create
	// time so operators can manually scale the pool without us reverting.
	//
	// When AteomImage is empty, no ensurer runs — the WorkerPool must be
	// provisioned by the operator (the original behavior).
	WorkerPoolReplicas    int32
	WorkerPoolAteomImage  string
}
