package aauth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
)

// HTTP path constants exposed by the Agent Provider.
const (
	PathJWKS          = "/.well-known/jwks.json"
	PathAgentMetadata = "/.well-known/aauth-agent.json"
	PathAgentJWT      = "/aauth/agent-jwt"
)

// AgentJWTRequest is the body of POST /aauth/agent-jwt.
//
// The agent's identity is derived server-side from the bearer token in the
// Authorization header (verified via Kubernetes TokenReview). The Sub field
// in the body is optional; if present it must match the derived identity,
// otherwise the mint is rejected. PublicKeyJWK is the agent's ephemeral
// signing public key, bound into the minted JWT via the cnf.jwk claim.
//
// SubstrateActorID, when non-empty, selects the substrate-aware mint path
// (used when the agent runs as a substrate actor instead of a regular pod).
// In that path the bearer token is the worker pod's SA token and identity
// is resolved by chaining TokenReview → substrate Control.GetActor → kagent
// labels on the ActorTemplate. See SubstrateSubjectAuthenticator.
type AgentJWTRequest struct {
	Sub              string                 `json:"sub,omitempty"`
	PublicKeyJWK     map[string]interface{} `json:"public_key_jwk"`
	SubstrateActorID string                 `json:"substrate_actor_id,omitempty"`
}

// AgentJWTResponse is the body of a successful POST /aauth/agent-jwt.
type AgentJWTResponse struct {
	Token     string `json:"token"`
	ExpiresIn int64  `json:"expires_in"` // seconds
}

// HandleJWKS serves the issuer's public JWKS at /.well-known/jwks.json.
func HandleJWKS(issuer *Issuer) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		body, err := issuer.JWKS()
		if err != nil {
			http.Error(w, "failed to build JWKS", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/jwk-set+json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write(body)
	}
}

// HandleAgentMetadata serves the Agent Provider discovery document.
func HandleAgentMetadata(issuer *Issuer) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_ = json.NewEncoder(w).Encode(issuer.AgentMetadata())
	}
}

// HandleIssueAgentJWT mints an aa-agent+jwt for the requesting agent.
//
// The endpoint is bypassed by the regular AAuth-aware authn middleware
// (the caller has no AAuth identity yet — that's the whole point of this
// call). Workload identity is established by one of two paths, autodetected
// from the request body:
//
//   - Default (deployment-mode agents): validate the caller's K8s SA token
//     via TokenReview, derive sub from system:serviceaccount:<ns>:<name>.
//   - Substrate (when body.substrate_actor_id is set): chain TokenReview →
//     substrate Control.GetActor placement attestation → kagent labels on
//     the ActorTemplate, see SubstrateSubjectAuthenticator.
//
// The body's sub field is only accepted if it matches the derived identity.
//
// substrateAuth may be nil — substrate-aware mint is then unavailable and a
// substrate-flavored request returns 400 instead of attempting the default
// path (which would always fail since the worker SA isn't a per-agent SA).
func HandleIssueAgentJWT(issuer *Issuer, auth SubjectAuthenticator, substrateAuth *SubstrateSubjectAuthenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()

		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "read body")
			return
		}
		var req AgentJWTRequest
		if err := json.Unmarshal(body, &req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		if req.PublicKeyJWK == nil {
			writeJSONError(w, http.StatusBadRequest, "public_key_jwk is required")
			return
		}

		var derivedSub string
		if req.SubstrateActorID != "" {
			// Substrate path: source-IP + actor placement attestation. No
			// bearer is required (substrate's restricted container schema
			// can't mount an SA token into the actor's gVisor sandbox).
			if substrateAuth == nil {
				writeJSONError(w, http.StatusBadRequest, "substrate_actor_id was sent but the controller has no substrate authenticator configured")
				return
			}
			derivedSub, err = substrateAuth.AuthenticateActor(r.Context(), r.RemoteAddr, req.SubstrateActorID, req.Sub)
			if err != nil {
				ctrllog.FromContext(r.Context()).Info("aauth: substrate mint rejected",
					"remoteAddr", r.RemoteAddr,
					"substrate_actor_id", req.SubstrateActorID,
					"claimed_sub", req.Sub,
					"reason", err.Error(),
				)
				writeJSONError(w, http.StatusForbidden, fmt.Sprintf("substrate attestation failed: %s", err))
				return
			}
			ctrllog.FromContext(r.Context()).Info("aauth: substrate mint allowed",
				"remoteAddr", r.RemoteAddr,
				"substrate_actor_id", req.SubstrateActorID,
				"sub", derivedSub,
			)
		} else {
			// Deployment-mode path: K8s SA token via TokenReview.
			bearer := BearerTokenFromAuthorizationHeader(r.Header.Get("Authorization"))
			if bearer == "" {
				writeJSONError(w, http.StatusUnauthorized, "missing or invalid Authorization header (expected Bearer <serviceaccount-token>)")
				return
			}
			derivedSub, err = auth.Authenticate(r.Context(), bearer)
			if err != nil {
				writeJSONError(w, http.StatusForbidden, fmt.Sprintf("token validation failed: %s", err))
				return
			}
			if req.Sub != "" && req.Sub != derivedSub {
				writeJSONError(w, http.StatusForbidden, fmt.Sprintf("body sub %q does not match ServiceAccount-derived sub %q", req.Sub, derivedSub))
				return
			}
		}

		token, err := issuer.MintAgentJWT(derivedSub, req.PublicKeyJWK)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "mint failed: "+err.Error())
			return
		}
		resp := AgentJWTResponse{
			Token:     token,
			ExpiresIn: int64(issuer.tokenTTL.Seconds()),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
