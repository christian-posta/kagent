package sandboxbackend

import "net/http"

// SandboxRoutingFunc lets a sandbox backend customize how the A2A registrar
// dials sandbox-mode agents.
//
// The current shipping case is substrate: the dial target is the shared
// atenet-router Service and per-actor identity is carried by a Host header
// (parsed by atenet's ExtProc to extract the actor ID and trigger
// ResumeActor). Other backends (agent-sandbox, openshell) keep the default
// "dial a per-agent Service" semantics and don't need this hook — the
// app glue passes nil for them.
//
// Returning ("", nil) means "no override; use the default per-agent URL the
// translator put on the agent card."
type SandboxRoutingFunc func(namespace, name string) (dialURL string, httpClient *http.Client)
