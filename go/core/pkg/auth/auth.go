package auth

import (
	"context"
	"net/http"
	"net/url"
)

type Verb string

const (
	VerbGet    Verb = "get"
	VerbCreate Verb = "create"
	VerbUpdate Verb = "update"
	VerbDelete Verb = "delete"
)

type Resource struct {
	Name string
	Type string
}

type User struct {
	ID string
}

type Agent struct {
	ID string
}

// Authn
type Principal struct {
	User   User
	Agent  Agent
	Claims map[string]any // Raw JWT claims (nil for non-JWT auth)
}

type Session interface {
	Principal() Principal
}

// Responsibilities:
// - Authenticate:
//   - a2a requests from ui/cli (human users)
//   - api requests from users/agents
//
// - Forward auth credentials to upstream agents
type AuthProvider interface {
	Authenticate(ctx context.Context, reqHeaders http.Header, query url.Values) (Session, error)
	// add auth to upstream requests of a session for upstream service account.
	UpstreamAuth(r *http.Request, session Session, upstreamPrincipal Principal) error
}

// Authz
type Authorizer interface {
	Check(ctx context.Context, principal Principal, verb Verb, resource Resource) error
}

// context utils

type sessionKeyType struct{}

var (
	sessionKey = sessionKeyType{}
)

func AuthSessionFrom(ctx context.Context) (Session, bool) {
	v, ok := ctx.Value(sessionKey).(Session)
	return v, ok && v != nil
}

func AuthSessionTo(ctx context.Context, session Session) context.Context {
	return context.WithValue(ctx, sessionKey, session)
}

func AuthnMiddleware(authn AuthProvider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip authentication for health and version endpoints (used by probes)
			// and for AAuth Agent Provider endpoints (public discovery + JWT issuance;
			// the JWT issue endpoint is the agent's bootstrap credential and is reached
			// before the agent has any other auth material — Phase 3 will TokenReview it).
			if r.URL.Path == "/health" || r.URL.Path == "/version" ||
				r.URL.Path == "/.well-known/jwks.json" ||
				r.URL.Path == "/.well-known/aauth-agent.json" ||
				r.URL.Path == "/aauth/agent-jwt" {
				next.ServeHTTP(w, r)
				return
			}
			session, err := authn.Authenticate(r.Context(), r.Header, r.URL.Query())
			if err != nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			if session != nil {
				r = r.WithContext(AuthSessionTo(r.Context(), session))
			}
			next.ServeHTTP(w, r)
		})
	}
}
