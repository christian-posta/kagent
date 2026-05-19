package aauth

import (
	"context"
	"fmt"
	"strings"

	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SAUsernamePrefix is the prefix the kube-apiserver places on the username
// in a TokenReview status for a ServiceAccount-authenticated request.
const SAUsernamePrefix = "system:serviceaccount:"

// SubjectAuthenticator validates a caller's bearer token and returns the
// canonical AAuth agent identifier (sub claim) for that workload.
//
// In production the caller is always a Kubernetes pod presenting its
// projected ServiceAccount token. The kube-apiserver authenticates the
// token via TokenReview; this helper translates the resulting
// system:serviceaccount:<ns>:<name> username into the
// aauth:<name>@<ns>.kagent.local identifier embedded in the JWT's sub.
//
// Splitting the interface out from the production implementation lets the
// HTTP handler be unit-tested without standing up a fake clientset.
type SubjectAuthenticator interface {
	Authenticate(ctx context.Context, bearerToken string) (sub string, err error)
}

// K8sSubjectAuthenticator authenticates via the kube-apiserver TokenReview API.
type K8sSubjectAuthenticator struct {
	Client kubernetes.Interface
	// Audiences optionally restricts which audiences are acceptable.
	// Empty = accept any. For audience-bound tokens (recommended in prod),
	// set this to ["kagent-controller"] and configure the agent pod to
	// project an SA token with that audience.
	Audiences []string
}

// Authenticate runs a TokenReview against the kube-apiserver and derives
// the canonical AAuth sub from the authenticated ServiceAccount.
func (k *K8sSubjectAuthenticator) Authenticate(ctx context.Context, bearerToken string) (string, error) {
	if bearerToken == "" {
		return "", fmt.Errorf("empty bearer token")
	}
	tr := &authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token:     bearerToken,
			Audiences: k.Audiences,
		},
	}
	out, err := k.Client.AuthenticationV1().TokenReviews().Create(ctx, tr, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("TokenReview API error: %w", err)
	}
	if !out.Status.Authenticated {
		msg := out.Status.Error
		if msg == "" {
			msg = "token not authenticated"
		}
		return "", fmt.Errorf("%s", msg)
	}
	return SubjectFromUsername(out.Status.User.Username)
}

// SubjectFromUsername parses a Kubernetes ServiceAccount username
// (system:serviceaccount:<ns>:<sa-name>) and returns the canonical AAuth
// identifier aauth:<sa-name>@<ns>.kagent.local.
//
// Returns an error if the username is empty, malformed, or represents a
// user/group/anonymous principal rather than a ServiceAccount.
func SubjectFromUsername(username string) (string, error) {
	if !strings.HasPrefix(username, SAUsernamePrefix) {
		return "", fmt.Errorf("caller is not a ServiceAccount: %q", username)
	}
	rest := username[len(SAUsernamePrefix):]
	ns, name, ok := strings.Cut(rest, ":")
	if !ok || ns == "" || name == "" {
		return "", fmt.Errorf("malformed ServiceAccount username: %q", username)
	}
	return fmt.Sprintf("aauth:%s@%s.kagent.local", name, ns), nil
}

// BearerTokenFromAuthorizationHeader extracts the token from a standard
// "Authorization: Bearer <token>" header value. Returns an empty string
// if the header is missing or doesn't use the Bearer scheme.
func BearerTokenFromAuthorizationHeader(authHeader string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return ""
	}
	return strings.TrimSpace(authHeader[len(prefix):])
}
