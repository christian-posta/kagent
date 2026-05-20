package env

// Core kagent environment variables used by the controller and agent runtime.
var (
	KagentNamespace = RegisterStringVar(
		"KAGENT_NAMESPACE",
		"kagent",
		"Kubernetes namespace where kagent resources are deployed.",
		ComponentController,
	)

	KagentControllerName = RegisterStringVar(
		"KAGENT_CONTROLLER_NAME",
		"kagent-controller",
		"Name of the kagent controller service.",
		ComponentController,
	)

	KagentA2ADebugAddr = RegisterStringVar(
		"KAGENT_A2A_DEBUG_ADDR",
		"",
		"Debug address for the A2A server. When set, all A2A HTTP requests are dialed to this address.",
		ComponentController,
	)

	// Variables injected into agent pods (not read by the controller itself).

	KagentName = RegisterStringVar(
		"KAGENT_NAME",
		"",
		"Name of the agent. Injected into agent pods via the controller.",
		ComponentAgentRuntime,
	)

	KagentURL = RegisterStringVar(
		"KAGENT_URL",
		"",
		"Base URL for A2A communication with the kagent controller.",
		ComponentAgentRuntime,
	)

	KagentSkillsFolder = RegisterStringVar(
		"KAGENT_SKILLS_FOLDER",
		"/skills",
		"Directory path where agent skills are mounted.",
		ComponentAgentRuntime,
	)

	KagentSRTSettingsPath = RegisterStringVar(
		"KAGENT_SRT_SETTINGS_PATH",
		"/config/srt-settings.json",
		"Path to the mounted srt settings file used by sandboxed execution.",
		ComponentAgentRuntime,
	)

	KagentPropagateToken = RegisterStringVar(
		"KAGENT_PROPAGATE_TOKEN",
		"",
		"When set, propagates the authentication token to downstream services.",
		ComponentAgentRuntime,
	)

	StsWellKnownURI = RegisterStringVar(
		"STS_WELL_KNOWN_URI",
		"",
		"Well-known endpoint for the Security Token Service (STS) used for token exchange.",
		ComponentAgentRuntime,
	)

	AAuthEnabled = RegisterStringVar(
		"AAUTH_ENABLED",
		"false",
		"When true, enables AAuth (RFC 9421 HTTP Message Signatures) for all outbound agent requests.",
		ComponentAgentRuntime,
	)

	AAuthAgentID = RegisterStringVar(
		"AAUTH_AGENT_ID",
		"",
		"AAuth agent identifier URI, of the form aauth:<name>@<namespace>.kagent.local.",
		ComponentAgentRuntime,
	)

	AAuthControllerURL = RegisterStringVar(
		"AAUTH_CONTROLLER_URL",
		"",
		"URL the agent uses to reach the kagent controller's AAuth Agent Provider endpoints "+
			"(/aauth/agent-jwt, /.well-known/jwks.json). When unset, the agent falls back to "+
			"the hwk (pseudonymous) signature scheme. Phase 2.",
		ComponentAgentRuntime,
	)

	AAuthSATokenPath = RegisterStringVar(
		"AAUTH_SA_TOKEN_PATH",
		"",
		"Filesystem path of the audience-scoped ServiceAccount token the agent sends as a "+
			"Bearer credential when calling the controller's /aauth/agent-jwt mint endpoint. "+
			"When set, the AAuth signer reads this file; otherwise it falls back to the "+
			"default SA token mount (/var/run/secrets/kubernetes.io/serviceaccount/token).",
		ComponentAgentRuntime,
	)

	AAuthVerifyIssuerRewrite = RegisterStringVar(
		"AAUTH_VERIFY_ISSUER_REWRITE",
		"",
		"Comma-separated canonical-iss=in-cluster-URL pairs used by the agent's inbound "+
			"verifier to fetch JWKS for issuers whose canonical URL isn't reachable from "+
			"inside the cluster. Example: http://localhost:8083=http://kagent-controller.kagent:8083",
		ComponentAgentRuntime,
	)
)
