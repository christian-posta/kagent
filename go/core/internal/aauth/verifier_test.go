package aauth_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/kagent-dev/kagent/go/core/internal/aauth"
)

func TestParseVerifyMode(t *testing.T) {
	cases := []struct {
		in   string
		want aauth.VerifyMode
	}{
		{"", aauth.VerifyModeLog},
		{"log", aauth.VerifyModeLog},
		{"observe", aauth.VerifyModeLog},
		{"off", aauth.VerifyModeOff},
		{"disabled", aauth.VerifyModeOff},
		{"false", aauth.VerifyModeOff},
		{"enforce", aauth.VerifyModeEnforce},
		{"STRICT", aauth.VerifyModeEnforce},
		{"  enforce  ", aauth.VerifyModeEnforce},
		{"garbage", aauth.VerifyModeLog}, // unknown → log (safe default)
	}
	for _, tc := range cases {
		if got := aauth.ParseVerifyMode(tc.in); got != tc.want {
			t.Errorf("ParseVerifyMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLocalJWKSFetcher_OnlyServesOwnURL(t *testing.T) {
	issuer, err := aauth.NewIssuer("http://localhost:8083", time.Hour)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	f, err := aauth.NewLocalJWKSFetcher(issuer)
	if err != nil {
		t.Fatalf("NewLocalJWKSFetcher: %v", err)
	}

	// Happy path: own JWKS URL.
	set, err := f.Get(t.Context(), "http://localhost:8083/.well-known/jwks.json")
	if err != nil {
		t.Fatalf("Get(own jwks): %v", err)
	}
	if set == nil || set.Len() != 1 {
		t.Fatalf("expected exactly 1 key in JWKS, got %v", set)
	}

	// Negative: someone else's URL.
	if _, err := f.Get(t.Context(), "http://attacker.example/.well-known/jwks.json"); err == nil {
		t.Errorf("expected error for foreign jwks_uri")
	}

	// Metadata happy path.
	md, err := f.GetMetadata(t.Context(), "http://localhost:8083/.well-known/aauth-agent.json")
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}
	if md["issuer"] != "http://localhost:8083" {
		t.Errorf("expected issuer=http://localhost:8083, got %q", md["issuer"])
	}
}

// captureLogger records the most recent Info/Warn call so tests can assert
// the middleware logged what we expect.
type captureLogger struct {
	mu        sync.Mutex
	lastLevel string
	lastMsg   string
	lastKV    []any
}

func (c *captureLogger) Info(msg string, kv ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastLevel = "info"
	c.lastMsg = msg
	c.lastKV = kv
}
func (c *captureLogger) Warn(msg string, kv ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastLevel = "warn"
	c.lastMsg = msg
	c.lastKV = kv
}

func newTestVerifier(t *testing.T, mode aauth.VerifyMode) (*aauth.Verifier, *captureLogger) {
	t.Helper()
	issuer, err := aauth.NewIssuer("http://localhost:8083", time.Hour)
	if err != nil {
		t.Fatalf("NewIssuer: %v", err)
	}
	v, err := aauth.NewLocalVerifier(issuer, mode)
	if err != nil {
		t.Fatalf("NewLocalVerifier: %v", err)
	}
	logger := &captureLogger{}
	// Set logger via reflection-like assignment: Verifier.Logger is exported.
	v.Logger = logger
	return v, logger
}

func TestMiddleware_OffMode_Passthrough(t *testing.T) {
	v, _ := newTestVerifier(t, aauth.VerifyModeOff)

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "http://controller/api/sessions", nil)
	rec := httptest.NewRecorder()
	v.Middleware()(next).ServeHTTP(rec, req)

	if !called {
		t.Fatal("downstream handler should have been called in off mode")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}

func TestMiddleware_LogMode_UnsignedRequest_PassthroughAndWarns(t *testing.T) {
	v, logger := newTestVerifier(t, aauth.VerifyModeLog)

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		// No verified identity should be attached for an unsigned request.
		if _, ok := aauth.VerifiedIdentityFromContext(r.Context()); ok {
			t.Error("did not expect a verified identity for unsigned request")
		}
		w.WriteHeader(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodPost, "http://controller/api/sessions", nil)
	rec := httptest.NewRecorder()
	v.Middleware()(next).ServeHTTP(rec, req)

	if !called {
		t.Fatal("downstream handler should have been called in log mode (passthrough)")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 (log mode never rejects), got %d", rec.Code)
	}
	if logger.lastLevel != "warn" {
		t.Errorf("expected a warn log line, got %q msg=%q", logger.lastLevel, logger.lastMsg)
	}
	if logger.lastMsg != "aauth: unverified" {
		t.Errorf("expected unverified log, got %q", logger.lastMsg)
	}
}

func TestMiddleware_EnforceMode_UnsignedRequest_Rejects401(t *testing.T) {
	v, _ := newTestVerifier(t, aauth.VerifyModeEnforce)

	called := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	})

	req := httptest.NewRequest(http.MethodPost, "http://controller/api/sessions", nil)
	rec := httptest.NewRecorder()
	v.Middleware()(next).ServeHTTP(rec, req)

	if called {
		t.Fatal("downstream handler must not run in enforce mode when verification fails")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "AAuth" {
		t.Errorf("expected WWW-Authenticate=AAuth, got %q", got)
	}
}

func TestMiddleware_SkipsAAuthAndWellKnownPaths(t *testing.T) {
	v, logger := newTestVerifier(t, aauth.VerifyModeEnforce)

	for _, path := range []string{
		"/health",
		"/version",
		"/.well-known/jwks.json",
		"/.well-known/aauth-agent.json",
		"/aauth/agent-jwt",
		"/.well-known/anything-else",
	} {
		t.Run(path, func(t *testing.T) {
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				w.WriteHeader(http.StatusOK)
			})
			req := httptest.NewRequest(http.MethodGet, "http://controller"+path, nil)
			rec := httptest.NewRecorder()
			v.Middleware()(next).ServeHTTP(rec, req)
			if !called {
				t.Fatalf("path %s should bypass the verifier", path)
			}
			if rec.Code != http.StatusOK {
				t.Errorf("path %s should not be rejected; got %d", path, rec.Code)
			}
			if logger.lastMsg == "aauth: unverified" || logger.lastMsg == "aauth: verified" {
				t.Errorf("path %s should not be logged by the verifier; got %q", path, logger.lastMsg)
			}
			logger.lastMsg = ""
		})
	}
}
