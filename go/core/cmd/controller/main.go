/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"log"

	"github.com/kagent-dev/kagent/go/core/internal/httpserver/auth"
	"github.com/kagent-dev/kagent/go/core/pkg/app"
	pkgauth "github.com/kagent-dev/kagent/go/core/pkg/auth"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/agentsxk8s"
	"github.com/kagent-dev/kagent/go/core/pkg/sandboxbackend/substrate"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"
)

//nolint:gocyclo
func main() {
	authorizer := &auth.NoopAuthorizer{}
	app.Start(func(bootstrap app.BootstrapConfig) (*app.ExtensionConfig, error) {
		authenticator := getAuthenticator(bootstrap.Config.Auth)
		backend := selectSandboxBackend(bootstrap.Config)
		idleSuspender := buildSubstrateIdleSuspender(bootstrap.Config, backend)
		workerPoolEnsurer := buildSubstrateWorkerPoolEnsurer(bootstrap.Config, backend)
		ext := &app.ExtensionConfig{
			Authenticator:  authenticator,
			Authorizer:     authorizer,
			AgentPlugins:   nil,
			SandboxBackend: backend,
			SandboxRouting: substrateSandboxRouting(bootstrap.Config, backend, idleSuspender),
		}
		if idleSuspender != nil {
			ext.ManagerRunnables = append(ext.ManagerRunnables, idleSuspender)
		}
		if workerPoolEnsurer != nil {
			ext.ManagerRunnables = append(ext.ManagerRunnables, workerPoolEnsurer)
		}
		return ext, nil
	}, nil)
}

// buildSubstrateWorkerPoolEnsurer constructs the WorkerPool auto-provisioner
// when the substrate backend is active AND --substrate-worker-pool-ateom-image
// is set. The ensurer creates the WorkerPool referenced by the rest of the
// substrate flags at startup, so operators don't have to apply
// 01-substrate.yaml by hand.
//
// Returns nil when (a) the active backend isn't substrate, or (b) no
// ateomImage is configured — in either case nil is the right "do nothing"
// signal for ManagerRunnables.
func buildSubstrateWorkerPoolEnsurer(cfg *app.Config, backend sandboxbackend.Backend) *substrate.WorkerPoolEnsurer {
	if cfg == nil {
		return nil
	}
	if _, ok := backend.(*substrate.Backend); !ok {
		return nil
	}
	return substrate.NewWorkerPoolEnsurer(
		cfg.Substrate.WorkerPoolNamespace,
		cfg.Substrate.WorkerPoolName,
		cfg.Substrate.WorkerPoolAteomImage,
		int32(cfg.Substrate.WorkerPoolReplicas),
	)
}

// substrateSandboxRouting returns the A2A routing override for the substrate
// backend (Host-header-via-atenet-router). For any other backend, returns nil.
// The idleSuspender (when non-nil) is wired into the routing transport's
// onRequest callback so each outbound request bumps the actor's idle timer.
func substrateSandboxRouting(cfg *app.Config, backend sandboxbackend.Backend, idleSuspender *substrate.IdleSuspender) sandboxbackend.SandboxRoutingFunc {
	if cfg == nil || backend == nil {
		return nil
	}
	if _, ok := backend.(*substrate.Backend); !ok {
		return nil
	}
	return substrate.NewSandboxRoutingFunc(cfg.Substrate.RouterURL, idleSuspender)
}

// buildSubstrateIdleSuspender constructs the IdleSuspender when the substrate
// backend is active AND --substrate-idle-timeout > 0. The component shares the
// backend's already-dialed ControlClient.
func buildSubstrateIdleSuspender(cfg *app.Config, backend sandboxbackend.Backend) *substrate.IdleSuspender {
	if cfg == nil || cfg.Substrate.IdleTimeout <= 0 {
		return nil
	}
	sb, ok := backend.(*substrate.Backend)
	if !ok {
		return nil
	}
	control := sb.ControlClient()
	if control == nil {
		log.Printf("substrate: --substrate-idle-timeout set but Control client is unavailable; auto-suspend disabled")
		return nil
	}
	return substrate.NewIdleSuspender(substrate.IdleSuspenderConfig{
		Control:       control,
		IdleTimeout:   cfg.Substrate.IdleTimeout,
		SweepInterval: cfg.Substrate.IdleSweepInterval,
	})
}

// selectSandboxBackend picks the substrate backend when its required flags are
// set, otherwise falls back to the default agent-sandbox (agentsxk8s) backend.
// "Setting --substrate-worker-pool-name" is the explicit opt-in.
//
// When the substrate backend is selected and a Control endpoint is configured,
// this also dials the ate-api-server gRPC service and hands the live client to
// the backend so it can drive actor lifecycle (CreateActor on Ready, etc.).
// A dial failure here surfaces immediately as a controller startup error
// rather than producing a half-wired backend.
func selectSandboxBackend(cfg *app.Config) sandboxbackend.Backend {
	if cfg == nil || cfg.Substrate.WorkerPoolName == "" {
		return agentsxk8s.New()
	}
	sc := substrate.Config{
		WorkerPoolNamespace: cfg.Substrate.WorkerPoolNamespace,
		WorkerPoolName:      cfg.Substrate.WorkerPoolName,
		SnapshotsLocation:   cfg.Substrate.SnapshotsLocation,
		PauseImage:          cfg.Substrate.PauseImage,
		RunscAMD64URL:       cfg.Substrate.RunscAMD64URL,
		RunscAMD64SHA256:    cfg.Substrate.RunscAMD64SHA256,
		RunscARM64URL:       cfg.Substrate.RunscARM64URL,
		RunscARM64SHA256:    cfg.Substrate.RunscARM64SHA256,
		RouterURL:           cfg.Substrate.RouterURL,
		AgentImage:          cfg.Substrate.AgentImage,
	}
	if cfg.Substrate.ControlEndpoint != "" {
		cli, err := substrate.NewControlClient(context.Background(), substrate.ControlClientConfig{
			Endpoint:      cfg.Substrate.ControlEndpoint,
			PlaintextOnly: cfg.Substrate.ControlPlaintext,
		})
		if err != nil {
			// Log via stdlib; ctrl logger isn't initialized at this point in
			// startup. The error is non-fatal — backend still emits
			// ActorTemplates; only actor lifecycle is degraded.
			log.Printf("substrate: failed to dial Control endpoint %q (continuing without actor lifecycle): %v",
				cfg.Substrate.ControlEndpoint, err)
		} else {
			sc.Control = cli
		}
	}
	return substrate.New(sc)
}

func getAuthenticator(authCfg struct{ Mode, UserIDClaim string }) pkgauth.AuthProvider {
	switch authCfg.Mode {
	case "trusted-proxy":
		return auth.NewProxyAuthenticator(authCfg.UserIDClaim)
	case "unsecure":
		return &auth.UnsecureAuthenticator{}
	default:
		panic("unknown auth mode: " + authCfg.Mode + " (valid modes: unsecure, trusted-proxy)")
	}
}
