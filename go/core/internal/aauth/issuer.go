// Package aauth implements the controller-side AAuth Agent Provider:
// it generates an Ed25519 issuer key on startup, publishes a JWKS, and
// issues aa-agent+jwt tokens that bind an agent's identity to its
// ephemeral signing public key (cnf.jwk).
//
// This is Phase 2 of the kagent AAuth integration. Phase 1 used the hwk
// (bare-key/pseudonymous) scheme; Phase 2 adds the jwt scheme so that
// agents present a controller-issued JWT instead of just a raw public key.
//
// The issuer key is loaded from (or written to, on first start) a Secret
// in the controller's namespace — see keystore.go. This keeps the JWKS
// stable across controller restarts so outstanding agent JWTs remain
// verifiable.
package aauth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jws"
)

// DefaultKid is the key id embedded in the issuer's JWKS and in every JWT header.
const DefaultKid = "kagent-issuer-1"

// AgentTokenDWK is the value the AAuth library expects in the dwk claim of
// aa-agent+jwt tokens. See aauth-go-library/pkg/aauth/tokens_jwt.go.
const AgentTokenDWK = "aauth-agent.json"

// AgentTokenType is the required JWT typ header for agent tokens.
const AgentTokenType = "aa-agent+jwt"

// Issuer signs aa-agent+jwt tokens with a process-local Ed25519 key.
type Issuer struct {
	priv      ed25519.PrivateKey
	pub       ed25519.PublicKey
	pubJWK    jwk.Key
	kid       string
	issuerURL string
	tokenTTL  time.Duration
}

// NewIssuer generates a fresh Ed25519 keypair and returns an Issuer
// configured to embed issuerURL in every minted token's iss claim.
//
// This is the ephemeral path — useful for tests and for environments where
// JWKS rotation on every restart is acceptable. Production should use
// NewIssuerFromKey with a key loaded from a persistent Secret.
func NewIssuer(issuerURL string, tokenTTL time.Duration) (*Issuer, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("aauth: generate ed25519 key: %w", err)
	}
	return NewIssuerFromKey(priv, pub, DefaultKid, issuerURL, tokenTTL)
}

// NewIssuerFromKey constructs an Issuer from an existing Ed25519 keypair and kid.
// Used when the keypair is loaded from a persistent store (Secret) so that the
// JWKS stays stable across controller restarts.
func NewIssuerFromKey(
	priv ed25519.PrivateKey,
	pub ed25519.PublicKey,
	kid string,
	issuerURL string,
	tokenTTL time.Duration,
) (*Issuer, error) {
	if issuerURL == "" {
		return nil, fmt.Errorf("aauth: issuerURL is required")
	}
	if len(priv) == 0 || len(pub) == 0 {
		return nil, fmt.Errorf("aauth: priv and pub keys are required")
	}
	if kid == "" {
		kid = DefaultKid
	}

	pubJWK, err := jwk.FromRaw(pub)
	if err != nil {
		return nil, fmt.Errorf("aauth: convert public key to JWK: %w", err)
	}
	_ = pubJWK.Set(jwk.KeyIDKey, kid)
	_ = pubJWK.Set(jwk.AlgorithmKey, jwa.EdDSA)
	_ = pubJWK.Set(jwk.KeyUsageKey, "sig")

	return &Issuer{
		priv:      priv,
		pub:       pub,
		pubJWK:    pubJWK,
		kid:       kid,
		issuerURL: issuerURL,
		tokenTTL:  tokenTTL,
	}, nil
}

// IssuerURL returns the canonical iss URL embedded in every minted token.
func (i *Issuer) IssuerURL() string { return i.issuerURL }

// JWKS returns the issuer's public key encoded as a JWKS document.
// Suitable to serve at /.well-known/jwks.json.
func (i *Issuer) JWKS() ([]byte, error) {
	set := jwk.NewSet()
	if err := set.AddKey(i.pubJWK); err != nil {
		return nil, fmt.Errorf("aauth: build JWKS: %w", err)
	}
	return json.Marshal(set)
}

// AgentMetadata returns the Agent Provider metadata document.
// Served at /.well-known/aauth-agent.json so resources can discover the
// issuer's JWKS URI.
func (i *Issuer) AgentMetadata() map[string]interface{} {
	return map[string]interface{}{
		"issuer":   i.issuerURL,
		"jwks_uri": i.issuerURL + "/.well-known/jwks.json",
	}
}

// MintAgentJWT issues an aa-agent+jwt that asserts the given sub (agent id)
// is bound to the supplied public-key JWK (cnf.jwk).
//
// The cnfJWK argument must be the agent's PUBLIC key serialized as a JWK
// JSON object (typically Ed25519: {"kty":"OKP","crv":"Ed25519","x":"..."}).
func (i *Issuer) MintAgentJWT(sub string, cnfJWK map[string]interface{}) (string, error) {
	if sub == "" {
		return "", fmt.Errorf("aauth: sub is required")
	}
	if cnfJWK == nil {
		return "", fmt.Errorf("aauth: cnf.jwk is required")
	}

	now := time.Now()
	claims := map[string]interface{}{
		"iss": i.issuerURL,
		"dwk": AgentTokenDWK,
		"sub": sub,
		"iat": now.Unix(),
		"exp": now.Add(i.tokenTTL).Unix(),
		"cnf": map[string]interface{}{
			"jwk": cnfJWK,
		},
	}

	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("aauth: marshal claims: %w", err)
	}

	hdrs := jws.NewHeaders()
	_ = hdrs.Set(jws.TypeKey, AgentTokenType)
	_ = hdrs.Set(jws.AlgorithmKey, jwa.EdDSA)
	_ = hdrs.Set(jws.KeyIDKey, i.kid)

	signed, err := jws.Sign(payload,
		jws.WithKey(jwa.EdDSA, i.priv, jws.WithProtectedHeaders(hdrs)),
	)
	if err != nil {
		return "", fmt.Errorf("aauth: sign JWT: %w", err)
	}
	return string(signed), nil
}
