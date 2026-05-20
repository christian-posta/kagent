package aauth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	libaauth "github.com/christian-posta/aauth-go-library/pkg/aauth"
	"github.com/go-logr/logr"
	"github.com/lestrrat-go/jwx/v2/jwk"
)

// VerifyMode controls what the verifier middleware does with results.
type VerifyMode string

const (
	// VerifyModeOff disables verification entirely. The middleware is a no-op.
	VerifyModeOff VerifyMode = "off"
	// VerifyModeLog runs verification on every protected request and logs the
	// outcome but never rejects. Useful for observability and rollout.
	VerifyModeLog VerifyMode = "log"
	// VerifyModeEnforce runs verification and returns 401 with an AAuth
	// challenge when verification fails. Reserved for future use; not the
	// default in this revision.
	VerifyModeEnforce VerifyMode = "enforce"
)

// ParseVerifyMode parses a case-insensitive mode string. Empty defaults to log.
func ParseVerifyMode(s string) VerifyMode {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "off", "disabled", "false":
		return VerifyModeOff
	case "enforce", "strict":
		return VerifyModeEnforce
	case "", "log", "observe":
		return VerifyModeLog
	default:
		return VerifyModeLog
	}
}

// VerifiedIdentity is what the middleware stashes on the request context when
// verification succeeds. Downstream handlers can pull it out with
// VerifiedIdentityFromContext.
type VerifiedIdentity = libaauth.Identity

type verifiedIdentityKey struct{}

// VerifiedIdentityFromContext returns the verified AAuth identity attached to
// ctx by the verifier middleware. The second return value is false when no
// verified identity is present (verification not run, off-mode, or failure
// in log-only mode).
func VerifiedIdentityFromContext(ctx context.Context) (VerifiedIdentity, bool) {
	id, ok := ctx.Value(verifiedIdentityKey{}).(VerifiedIdentity)
	return id, ok
}

// jwksLogger is the minimal interface the middleware uses for logging.
type jwksLogger interface {
	Info(msg string, kv ...any)
	Warn(msg string, kv ...any)
}

// noopLogger is the default when the caller doesn't pass one.
type noopLogger struct{}

func (noopLogger) Info(string, ...any) {}
func (noopLogger) Warn(string, ...any) {}

// LogrAdapter wraps a logr.Logger as a jwksLogger so the controller can route
// verifier output through its existing structured-logging pipeline.
type LogrAdapter struct {
	Logger logr.Logger
}

func (a LogrAdapter) Info(msg string, kv ...any) { a.Logger.Info(msg, kv...) }
func (a LogrAdapter) Warn(msg string, kv ...any) {
	a.Logger.V(0).Info(msg, append([]any{"severity", "warn"}, kv...)...)
}

// LocalJWKSFetcher is a JWKSFetcher that serves the controller's own JWKS
// from memory, with no recursive HTTP fetch. It satisfies the aauth library's
// JWKSFetcher interface for the special case where the verifier is running
// inside the issuer process.
type LocalJWKSFetcher struct {
	IssuerURL string
	Set       jwk.Set
}

// Get returns the in-memory JWKS when uri matches the well-known JWKS URL.
func (l *LocalJWKSFetcher) Get(_ context.Context, uri string) (jwk.Set, error) {
	expected := strings.TrimRight(l.IssuerURL, "/") + PathJWKS
	if uri != expected {
		return nil, fmt.Errorf("unknown jwks_uri %q (local fetcher only serves %q)", uri, expected)
	}
	return l.Set, nil
}

// GetMetadata returns the Agent Provider metadata document when uri matches
// the well-known discovery URL.
func (l *LocalJWKSFetcher) GetMetadata(_ context.Context, uri string) (map[string]interface{}, error) {
	expected := strings.TrimRight(l.IssuerURL, "/") + PathAgentMetadata
	if uri != expected {
		return nil, fmt.Errorf("unknown metadata uri %q (local fetcher only serves %q)", uri, expected)
	}
	return map[string]interface{}{
		"issuer":   l.IssuerURL,
		"jwks_uri": strings.TrimRight(l.IssuerURL, "/") + PathJWKS,
	}, nil
}

// Invalidate is a no-op for the local fetcher (the JWKS never changes for
// the life of the process — the Issuer holds the only copy).
func (l *LocalJWKSFetcher) Invalidate(string) {}

// NewLocalJWKSFetcher builds a fetcher that serves the given Issuer's public
// JWK set from memory. Used by Phase 3 verification middleware when the
// verifier runs inside the controller process (which is also the issuer).
func NewLocalJWKSFetcher(issuer *Issuer) (*LocalJWKSFetcher, error) {
	if issuer == nil {
		return nil, fmt.Errorf("aauth: issuer is required")
	}
	set := jwk.NewSet()
	if err := set.AddKey(issuer.pubJWK); err != nil {
		return nil, fmt.Errorf("aauth: build local JWKS: %w", err)
	}
	return &LocalJWKSFetcher{IssuerURL: issuer.IssuerURL(), Set: set}, nil
}

// Verifier wraps the aauth-go-library verifier with the policy options we
// expect to see in a kagent controller deployment. One Verifier serves all
// protected routes; the middleware factory closes over it.
type Verifier struct {
	Options libaauth.VerifyOptions
	Fetcher libaauth.JWKSFetcher
	Logger  jwksLogger
	Mode    VerifyMode
}

// NewLocalVerifier builds a Verifier that knows how to validate aa-agent+jwt
// tokens issued by the supplied Issuer. The verifier accepts only the jwt
// scheme by default (Phase 2 traffic); set Options.AllowPseudonymous=true to
// also accept hwk-scheme requests.
func NewLocalVerifier(issuer *Issuer, mode VerifyMode) (*Verifier, error) {
	fetcher, err := NewLocalJWKSFetcher(issuer)
	if err != nil {
		return nil, err
	}
	return &Verifier{
		Options: libaauth.VerifyOptions{
			Issuer:                     issuer.IssuerURL(),
			AgentServers:               []libaauth.AgentServer{{Issuer: issuer.IssuerURL(), JwksURI: issuer.IssuerURL() + PathJWKS}},
			AllowedSignatureKeySchemes: []string{"jwt"},
			AllowedJWTTypes:            []string{AgentTokenType},
			SignatureWindow:            60 * time.Second,
			AllowInsecureJWTIssuer:     true, // demo runs on http://localhost; production should be false.
		},
		Fetcher: fetcher,
		Logger:  noopLogger{},
		Mode:    mode,
	}, nil
}

// Verify runs the aauth-go-library Verify against an inbound HTTP request.
//
// The lib does direct map access on lowercase header keys (e.g.
// ``headers["signature-input"]``), but Go's net/http canonicalizes incoming
// headers to title case (``Signature-Input``). We rebuild the header map
// with lowercase keys before handing it to the lib.
func (v *Verifier) Verify(ctx context.Context, r *http.Request) libaauth.VerifyResult {
	authority := r.Host
	if authority == "" {
		authority = r.URL.Host
	}
	return libaauth.Verify(ctx, v.Options, r.Method, authority, r.URL.Path, lowercaseHeaderKeys(r.Header), v.Fetcher)
}

// lowercaseHeaderKeys returns a new http.Header where every key is lowercased.
// Required because the upstream aauth library does direct map access on
// lowercase header names while Go normalizes incoming request headers to
// canonical (title-case) keys.
func lowercaseHeaderKeys(in http.Header) http.Header {
	out := make(http.Header, len(in))
	for k, v := range in {
		out[strings.ToLower(k)] = v
	}
	return out
}

// shouldSkip returns true for paths the verifier should ignore: AAuth itself,
// public discovery documents, health/version probes. These paths either have
// no caller identity yet (the JWT mint endpoint) or are public on purpose.
func shouldSkip(path string) bool {
	switch path {
	case PathJWKS, PathAgentMetadata, PathAgentJWT:
		return true
	case "/health", "/version":
		return true
	}
	return strings.HasPrefix(path, "/.well-known/")
}

// Middleware returns an http middleware that runs verification on every
// non-skipped request. Behaviour depends on v.Mode:
//   - off: passthrough, no verification.
//   - log: verify, log outcome, never reject. Successful verification
//     attaches the Identity to the request context.
//   - enforce: verify, reject with 401 on failure (reserved for future use).
func (v *Verifier) Middleware() func(http.Handler) http.Handler {
	if v == nil || v.Mode == VerifyModeOff {
		return func(next http.Handler) http.Handler { return next }
	}
	logger := v.Logger
	if logger == nil {
		logger = noopLogger{}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if shouldSkip(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			result := v.Verify(r.Context(), r)
			if result.Err != nil {
				v.onFailure(w, r, next, result, logger)
				return
			}
			logger.Info("aauth: verified",
				"caller", result.Identity.Delegate,
				"level", string(result.Identity.Level),
				"method", r.Method,
				"path", r.URL.Path,
			)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), verifiedIdentityKey{}, result.Identity)))
		})
	}
}

func (v *Verifier) onFailure(w http.ResponseWriter, r *http.Request, next http.Handler, result libaauth.VerifyResult, logger jwksLogger) {
	stage, detail, scheme := "", "", ""
	if result.Diagnostics != nil {
		stage = result.Diagnostics.Stage
		detail = result.Diagnostics.Detail
		scheme = result.Diagnostics.Scheme
	}
	logger.Warn("aauth: unverified",
		"reason", result.Err.Error(),
		"stage", stage,
		"detail", detail,
		"scheme", scheme,
		"method", r.Method,
		"path", r.URL.Path,
	)
	if v.Mode == VerifyModeEnforce {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("WWW-Authenticate", "AAuth")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintf(w, `{"error":"aauth_verification_failed","reason":%q}`, result.Err.Error())
		return
	}
	// log mode: passthrough.
	next.ServeHTTP(w, r)
}

// Compile-time assertion: the fetcher satisfies the lib interface.
var _ libaauth.JWKSFetcher = (*LocalJWKSFetcher)(nil)

// keep sync.Once usable from tests that load this package in parallel.
var _ = sync.Once{}
