package aauth

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
type AgentJWTRequest struct {
	Sub          string                 `json:"sub,omitempty"`
	PublicKeyJWK map[string]interface{} `json:"public_key_jwk"`
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
// call). Workload identity is established here by validating the caller's
// Kubernetes ServiceAccount token via TokenReview. The canonical sub is
// derived from the SA identity; the body's sub field is only accepted if
// it matches.
func HandleIssueAgentJWT(issuer *Issuer, auth SubjectAuthenticator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()

		bearer := BearerTokenFromAuthorizationHeader(r.Header.Get("Authorization"))
		if bearer == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing or invalid Authorization header (expected Bearer <serviceaccount-token>)")
			return
		}

		derivedSub, err := auth.Authenticate(r.Context(), bearer)
		if err != nil {
			writeJSONError(w, http.StatusForbidden, fmt.Sprintf("token validation failed: %s", err))
			return
		}

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
		if req.Sub != "" && req.Sub != derivedSub {
			writeJSONError(w, http.StatusForbidden, fmt.Sprintf("body sub %q does not match ServiceAccount-derived sub %q", req.Sub, derivedSub))
			return
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
