package openclaw

import (
	"fmt"
	"strings"

	"github.com/kagent-dev/kagent/go/api/v1alpha2"
)

// DefaultAPIKeyEnvVar is the environment variable name used for the model provider API key in the sandbox.
func DefaultAPIKeyEnvVar(provider v1alpha2.ModelProvider) string {
	return fmt.Sprintf("%s_API_KEY", strings.ToUpper(string(provider)))
}

// openshellResolveEnv matches OpenClaw onboard placeholders for OpenShell L7 credential rewrite.
//
// kagent fork: the upstream version routed through openshell/channels.ResolveEnvPlaceholder
// to share placeholder syntax with the openshell channel layer; we don't carry that
// package in this fork so we inline the format directly. The format
// `{{openshell:resolve:env:<VAR>}}` matches what OpenClaw's gateway recognizes.
func openshellResolveEnv(envVar string) string {
	return fmt.Sprintf("{{openshell:resolve:env:%s}}", envVar)
}
