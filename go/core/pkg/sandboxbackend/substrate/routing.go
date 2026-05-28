package substrate

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// DNSSuffix is the substrate atenet-router DNS suffix that the ExtProc parses
// to extract the actor ID. Stable: `actors.resources.substrate.ate.dev` —
// declared in agent-substrate/internal/resources/actor.go.
const DNSSuffix = "actors.resources.substrate.ate.dev"

// actorIDRegex is the substrate-mandated shape (DNS-1123 label).
var actorIDRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ActorIDFor returns a deterministic, substrate-legal actor ID for the
// (namespace, name) pair of a kagent SandboxAgent. Substrate requires actor
// IDs to match `[a-z0-9]([-a-z0-9]*[a-z0-9])?` and be ≤63 chars (DNS-1123).
//
// Strategy:
//   - Combine as `<ns>--<name>` (double dash to make the boundary unambiguous).
//   - If the result is ≤63 chars and matches the regex, use it.
//   - Otherwise truncate to 53 chars + a 10-char sha256 prefix, joined with `-`.
//     This preserves a recognizable prefix in the actor ID (good for logs)
//     while guaranteeing uniqueness.
//
// The same SandboxAgent must always produce the same actor ID — both the
// routing code and the lifecycle code (CreateActor / SuspendActor) rely on
// this determinism.
func ActorIDFor(namespace, name string) string {
	raw := namespace + "--" + name
	if len(raw) <= 63 && actorIDRegex.MatchString(raw) {
		return raw
	}
	// Hash the original combined identifier for uniqueness; truncate the
	// readable prefix so the total is ≤63 chars.
	h := sha256.Sum256([]byte(raw))
	hashSuffix := hex.EncodeToString(h[:])[:10]
	// Allowed prefix length = 63 - len("-") - 10 = 52.
	prefix := raw
	if len(prefix) > 52 {
		prefix = prefix[:52]
	}
	// Replace any chars that violate the DNS-1123 label rules.
	prefix = sanitizeForDNS1123(prefix)
	// Ensure trailing char isn't `-` (DNS-1123 rule).
	prefix = strings.TrimRight(prefix, "-")
	if prefix == "" {
		prefix = "agent"
	}
	return prefix + "-" + hashSuffix
}

// HostHeaderFor returns the HTTP Host header value atenet-router's ExtProc
// will parse to extract the actor ID. Substrate's ExtProc strips DNSSuffix
// from the Host and uses the remainder as the actor identifier.
func HostHeaderFor(actorID string) string {
	return actorID + "." + DNSSuffix
}

// HostRewritingTransport returns an http.RoundTripper that overrides the
// outgoing request's Host header to the substrate actor-DNS form, while
// still dialing the underlying base transport's network target (typically
// atenet-router.ate-system.svc or a port-forward).
//
// This pairs with an A2A client whose URL points at the atenet-router
// Service; the URL determines the DIAL TARGET, while the rewritten Host
// header is what atenet's ExtProc parses for actor identity.
//
// The optional onRequest hook fires before the request is forwarded.
// Substrate uses it to feed the IdleSuspender's Touch — each outbound A2A
// request to a sandbox-mode agent counts as activity for that actor.
func HostRewritingTransport(actorID string, base http.RoundTripper, onRequest func(actorID string)) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &hostRewriter{actorID: actorID, base: base, onRequest: onRequest}
}

type hostRewriter struct {
	actorID   string
	base      http.RoundTripper
	onRequest func(actorID string)
}

func (h *hostRewriter) RoundTrip(req *http.Request) (*http.Response, error) {
	if h.onRequest != nil {
		h.onRequest(h.actorID)
	}
	// http.Request.Host wins over URL.Host for the wire-level Host header
	// (see net/http docs); setting it here ensures atenet sees the actor
	// identity even though we dial a different address.
	host := HostHeaderFor(h.actorID)
	r := req.Clone(req.Context())
	r.Host = host
	return h.base.RoundTrip(r)
}

// sanitizeForDNS1123 lowercases and replaces disallowed chars with '-'.
// Used only inside ActorIDFor's overflow branch.
func sanitizeForDNS1123(s string) string {
	s = strings.ToLower(s)
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
			b = append(b, c)
		default:
			b = append(b, '-')
		}
	}
	// Strip leading non-alphanumerics (rule: first char must be [a-z0-9]).
	for len(b) > 0 && (b[0] < 'a' || b[0] > 'z') && (b[0] < '0' || b[0] > '9') {
		b = b[1:]
	}
	return string(b)
}

// ValidateActorID returns nil if the given ID would be accepted by substrate.
// Exposed for callers that build IDs by other means (e.g. tests).
func ValidateActorID(id string) error {
	if len(id) == 0 {
		return fmt.Errorf("actor id is empty")
	}
	if len(id) > 63 {
		return fmt.Errorf("actor id %q is %d chars; substrate requires ≤63", id, len(id))
	}
	if !actorIDRegex.MatchString(id) {
		return fmt.Errorf("actor id %q does not match [a-z0-9]([-a-z0-9]*[a-z0-9])?", id)
	}
	return nil
}
