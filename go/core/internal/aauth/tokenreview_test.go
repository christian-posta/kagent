package aauth_test

import (
	"context"
	"testing"

	"github.com/kagent-dev/kagent/go/core/internal/aauth"
	authv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestSubjectFromUsername(t *testing.T) {
	cases := []struct {
		name     string
		username string
		want     string
		wantErr  bool
	}{
		{
			name:     "valid SA",
			username: "system:serviceaccount:kagent:aauth-test-agent",
			want:     "aauth:aauth-test-agent@kagent.kagent.local",
		},
		{
			name:     "SA in different namespace",
			username: "system:serviceaccount:team-a:my-agent",
			want:     "aauth:my-agent@team-a.kagent.local",
		},
		{
			name:     "human user rejected",
			username: "alice@example.com",
			wantErr:  true,
		},
		{
			name:     "anonymous rejected",
			username: "system:anonymous",
			wantErr:  true,
		},
		{
			name:     "malformed SA missing name",
			username: "system:serviceaccount:kagent:",
			wantErr:  true,
		},
		{
			name:     "malformed SA missing namespace",
			username: "system:serviceaccount::name",
			wantErr:  true,
		},
		{
			name:     "empty",
			username: "",
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := aauth.SubjectFromUsername(tc.username)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBearerTokenFromAuthorizationHeader(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Bearer abc.def.ghi", "abc.def.ghi"},
		{"Bearer  padded ", "padded"},
		{"bearer lowercase", ""}, // scheme is case-sensitive in our impl
		{"abc.def.ghi", ""},
		{"", ""},
	}
	for _, tc := range cases {
		if got := aauth.BearerTokenFromAuthorizationHeader(tc.in); got != tc.want {
			t.Errorf("BearerTokenFromAuthorizationHeader(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestK8sSubjectAuthenticator_Authenticate(t *testing.T) {
	cases := []struct {
		name        string
		status      authv1.TokenReviewStatus
		bearerToken string
		want        string
		wantErr     bool
	}{
		{
			name:        "happy path",
			bearerToken: "valid-sa-token",
			status: authv1.TokenReviewStatus{
				Authenticated: true,
				User:          authv1.UserInfo{Username: "system:serviceaccount:kagent:aauth-test-agent"},
			},
			want: "aauth:aauth-test-agent@kagent.kagent.local",
		},
		{
			name:        "unauthenticated",
			bearerToken: "bogus",
			status: authv1.TokenReviewStatus{
				Authenticated: false,
				Error:         "invalid token",
			},
			wantErr: true,
		},
		{
			name:        "authenticated but not a ServiceAccount",
			bearerToken: "human-user-token",
			status: authv1.TokenReviewStatus{
				Authenticated: true,
				User:          authv1.UserInfo{Username: "alice@example.com"},
			},
			wantErr: true,
		},
		{
			name:        "empty bearer rejected without calling API",
			bearerToken: "",
			wantErr:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			client.PrependReactor("create", "tokenreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
				tr := action.(k8stesting.CreateAction).GetObject().(*authv1.TokenReview)
				tr.Status = tc.status
				return true, tr, nil
			})
			a := &aauth.K8sSubjectAuthenticator{Client: client}

			got, err := a.Authenticate(context.Background(), tc.bearerToken)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
