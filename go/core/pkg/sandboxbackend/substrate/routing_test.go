package substrate

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestActorIDFor(t *testing.T) {
	cases := []struct {
		ns, name string
		want     string
		check    func(t *testing.T, got string)
	}{
		{
			ns: "kagent-substrate-poc", name: "phase2-demo",
			want: "kagent-substrate-poc--phase2-demo",
		},
		{
			ns: "a", name: "b",
			want: "a--b",
		},
		// Boundary at exactly 63 chars — verify the short path still triggers.
		{
			ns: strings.Repeat("n", 30), name: strings.Repeat("a", 31),
			check: func(t *testing.T, got string) {
				require.LessOrEqual(t, len(got), 63)
				require.NoError(t, ValidateActorID(got))
				// Total raw = 30+2+31 = 63 — fits exactly without truncation.
				require.Equal(t, strings.Repeat("n", 30)+"--"+strings.Repeat("a", 31), got)
			},
		},
		// Over-budget input → truncated prefix + hash suffix.
		{
			ns: strings.Repeat("n", 40), name: strings.Repeat("a", 40),
			check: func(t *testing.T, got string) {
				require.LessOrEqual(t, len(got), 63)
				require.NoError(t, ValidateActorID(got))
				// Suffix is 10 chars of sha256 hex, prefixed by "-".
				require.Equal(t, byte('-'), got[len(got)-11])
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.ns+"/"+tc.name, func(t *testing.T) {
			got := ActorIDFor(tc.ns, tc.name)
			if tc.check != nil {
				tc.check(t, got)
			}
			if tc.want != "" {
				require.Equal(t, tc.want, got)
			}
			// Determinism: same inputs → same output.
			require.Equal(t, got, ActorIDFor(tc.ns, tc.name))
		})
	}
}

func TestHostHeaderFor(t *testing.T) {
	require.Equal(t,
		"my-agent.actors.resources.substrate.ate.dev",
		HostHeaderFor("my-agent"),
	)
}

func TestHostRewritingTransport(t *testing.T) {
	var seen string
	stub := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		seen = req.Host
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
		}, nil
	})
	rt := HostRewritingTransport("demo-actor", stub, nil)
	req, err := http.NewRequest("GET", "http://atenet-router.ate-system.svc/path", nil)
	require.NoError(t, err)
	originalHost := req.Host
	resp, err := rt.RoundTrip(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, "demo-actor.actors.resources.substrate.ate.dev", seen,
		"transport must rewrite Host to the actor-DNS form")
	// Caller's request must be untouched (we clone before mutating).
	require.Equal(t, originalHost, req.Host, "RoundTrip must not mutate the caller's request")
}

func TestValidateActorID(t *testing.T) {
	require.Error(t, ValidateActorID(""))
	require.Error(t, ValidateActorID(strings.Repeat("a", 64)))
	require.Error(t, ValidateActorID("-leading-dash"))
	require.Error(t, ValidateActorID("trailing-dash-"))
	require.Error(t, ValidateActorID("UPPER"))
	require.NoError(t, ValidateActorID("valid-id"))
	require.NoError(t, ValidateActorID("a"))
	require.NoError(t, ValidateActorID(strings.Repeat("a", 63)))
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
