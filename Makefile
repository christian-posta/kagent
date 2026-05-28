# Load local overrides (gitignored) — e.g. KAGENT_HELM_EXTRA_ARGS=-f helm/kagent/values.local.yaml
-include .env

# Image configuration
DOCKER_REGISTRY ?= localhost:5001
BASE_IMAGE_REGISTRY ?= cgr.dev
DOCKER_REPO ?= kagent-dev/kagent
HELM_REPO ?= oci://ghcr.io/kagent-dev
HELM_DIST_FOLDER ?= dist

BUILD_DATE := $(shell date -u '+%Y-%m-%d')
GIT_COMMIT := $(shell git rev-parse --short HEAD || echo "unknown")
VERSION ?= $(shell git describe --tags --always 2>/dev/null | grep v || echo "v0.0.0-$(GIT_COMMIT)")

# Local architecture detection to build for the current platform
LOCALARCH ?= $(shell uname -m | sed 's/x86_64/amd64/' | sed 's/aarch64/arm64/')

KUBECONFIG_PERM ?= $(shell \
  if [ "$$(uname -s | tr '[:upper:]' '[:lower:]')" = "darwin" ]; then \
    stat -f "%Lp" ~/.kube/config; \
  else \
    stat -c "%a" ~/.kube/config; \
  fi)


# Docker buildx configuration
BUILDKIT_VERSION = v0.23.0
BUILDX_NO_DEFAULT_ATTESTATIONS=1
BUILDX_BUILDER_NAME ?= kagent-builder-$(BUILDKIT_VERSION)

DOCKER_BUILDER ?= docker buildx
DOCKER_BUILD_ARGS ?= --push --platform linux/$(LOCALARCH)

KIND_CLUSTER_NAME ?= kagent
KIND_IMAGE_VERSION ?= 1.35.0

CONTROLLER_IMAGE_NAME ?= controller
UI_IMAGE_NAME ?= ui
APP_IMAGE_NAME ?= app
KAGENT_ADK_IMAGE_NAME ?= kagent-adk
GOLANG_ADK_IMAGE_NAME ?= golang-adk
SKILLS_INIT_IMAGE_NAME ?= skills-init

CONTROLLER_IMAGE_TAG ?= $(VERSION)
UI_IMAGE_TAG ?= $(VERSION)
APP_IMAGE_TAG ?= $(VERSION)
KAGENT_ADK_IMAGE_TAG ?= $(VERSION)
GOLANG_ADK_IMAGE_TAG ?= $(VERSION)
GOLANG_ADK_FULL_IMAGE_TAG ?= $(VERSION)-full
SKILLS_INIT_IMAGE_TAG ?= $(VERSION)
CONTROLLER_IMG ?= $(DOCKER_REGISTRY)/$(DOCKER_REPO)/$(CONTROLLER_IMAGE_NAME):$(CONTROLLER_IMAGE_TAG)
UI_IMG ?= $(DOCKER_REGISTRY)/$(DOCKER_REPO)/$(UI_IMAGE_NAME):$(UI_IMAGE_TAG)
APP_IMG ?= $(DOCKER_REGISTRY)/$(DOCKER_REPO)/$(APP_IMAGE_NAME):$(APP_IMAGE_TAG)
KAGENT_ADK_IMG ?= $(DOCKER_REGISTRY)/$(DOCKER_REPO)/$(KAGENT_ADK_IMAGE_NAME):$(KAGENT_ADK_IMAGE_TAG)
GOLANG_ADK_IMG ?= $(DOCKER_REGISTRY)/$(DOCKER_REPO)/$(GOLANG_ADK_IMAGE_NAME):$(GOLANG_ADK_IMAGE_TAG)
GOLANG_ADK_FULL_IMG ?= $(DOCKER_REGISTRY)/$(DOCKER_REPO)/$(GOLANG_ADK_IMAGE_NAME):$(GOLANG_ADK_FULL_IMAGE_TAG)
SKILLS_INIT_IMG ?= $(DOCKER_REGISTRY)/$(DOCKER_REPO)/$(SKILLS_INIT_IMAGE_NAME):$(SKILLS_INIT_IMAGE_TAG)

#take from go/go.mod
AWK ?= $(shell command -v gawk || command -v awk)
TOOLS_GO_VERSION ?= $(shell $(AWK) '/^go / { print $$2 }' go/go.mod)
export GOTOOLCHAIN=go$(TOOLS_GO_VERSION)

# Version information for the build
LDFLAGS := "-X github.com/$(DOCKER_REPO)/go/core/internal/version.Version=$(VERSION)      \
            -X github.com/$(DOCKER_REPO)/go/core/internal/version.GitCommit=$(GIT_COMMIT) \
            -X github.com/$(DOCKER_REPO)/go/core/internal/version.BuildDate=$(BUILD_DATE)"

#tools versions
TOOLS_UV_VERSION ?= 0.10.4
TOOLS_NODE_VERSION ?= 24.13.0
TOOLS_PYTHON_VERSION ?= 3.13

# build args
TOOLS_IMAGE_BUILD_ARGS =  --build-arg VERSION=$(VERSION)
TOOLS_IMAGE_BUILD_ARGS += --build-arg LDFLAGS=$(LDFLAGS)
TOOLS_IMAGE_BUILD_ARGS += --build-arg DOCKER_REPO=$(DOCKER_REPO)
TOOLS_IMAGE_BUILD_ARGS += --build-arg DOCKER_REGISTRY=$(DOCKER_REGISTRY)
TOOLS_IMAGE_BUILD_ARGS += --build-arg BASE_IMAGE_REGISTRY=$(BASE_IMAGE_REGISTRY)
TOOLS_IMAGE_BUILD_ARGS += --build-arg TOOLS_GO_VERSION=$(TOOLS_GO_VERSION)
TOOLS_IMAGE_BUILD_ARGS += --build-arg TOOLS_UV_VERSION=$(TOOLS_UV_VERSION)
TOOLS_IMAGE_BUILD_ARGS += --build-arg TOOLS_PYTHON_VERSION=$(TOOLS_PYTHON_VERSION)
TOOLS_IMAGE_BUILD_ARGS += --build-arg TOOLS_NODE_VERSION=$(TOOLS_NODE_VERSION)


##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Git

.PHONY: init-git-hooks
init-git-hooks:  ## Use the tracked version of Git hooks from this repo
	git config core.hooksPath .githooks
	echo "Git hooks initialized"

# KMCP
KMCP_ENABLED ?= true
KMCP_VERSION ?= $(shell $(AWK) '/github\.com\/kagent-dev\/kmcp/ { print substr($$2, 2) }' go/go.mod) # KMCP version defaults to what's referenced in go.mod

HELM_ACTION=upgrade --install

# Helm chart variables
KAGENT_DEFAULT_MODEL_PROVIDER ?= openAI

# Print tools versions
print-tools-versions:
	@echo "VERSION      : $(VERSION)"
	@echo "Tools Go     : $(TOOLS_GO_VERSION)"
	@echo "Tools UV     : $(TOOLS_UV_VERSION)"
	@echo "Tools Node   : $(TOOLS_NODE_VERSION)"
	@echo "Tools Istio  : $(TOOLS_ISTIO_VERSION)"
	@echo "Tools Argo CD: $(TOOLS_ARGO_CD_VERSION)"

# Check if the appropriate API key is set based on the model provider
check-api-key:
	@if [ "$(KAGENT_DEFAULT_MODEL_PROVIDER)" = "openAI" ]; then \
		if [ -z "$(OPENAI_API_KEY)" ]; then \
			echo "Error: OPENAI_API_KEY environment variable is not set for OpenAI provider"; \
			echo "Please set it with: export OPENAI_API_KEY=your-api-key"; \
			exit 1; \
		fi; \
	elif [ "$(KAGENT_DEFAULT_MODEL_PROVIDER)" = "anthropic" ]; then \
		if [ -z "$(ANTHROPIC_API_KEY)" ]; then \
			echo "Error: ANTHROPIC_API_KEY environment variable is not set for Anthropic provider"; \
			echo "Please set it with: export ANTHROPIC_API_KEY=your-api-key"; \
			exit 1; \
		fi; \
	elif [ "$(KAGENT_DEFAULT_MODEL_PROVIDER)" = "azureOpenAI" ]; then \
		if [ -z "$(AZUREOPENAI_API_KEY)" ]; then \
			echo "Error: AZUREOPENAI_API_KEY environment variable is not set for Azure OpenAI provider"; \
			echo "Please set it with: export AZUREOPENAI_API_KEY=your-api-key"; \
			exit 1; \
		fi; \
	elif [ "$(KAGENT_DEFAULT_MODEL_PROVIDER)" = "gemini" ]; then \
		if [ -z "$(GOOGLE_API_KEY)" ]; then \
			echo "Error: GOOGLE_API_KEY environment variable is not set for Gemini provider"; \
			echo "Please set it with: export GOOGLE_API_KEY=your-api-key"; \
			exit 1; \
		fi; \
	elif [ "$(KAGENT_DEFAULT_MODEL_PROVIDER)" = "ollama" ]; then \
		echo "Note: Ollama provider does not require an API key"; \
	else \
		echo "Warning: Unknown model provider '$(KAGENT_DEFAULT_MODEL_PROVIDER)'. Skipping API key check."; \
	fi

.PHONY: buildx-create
buildx-create:
	docker buildx inspect $(BUILDX_BUILDER_NAME) 2>&1 > /dev/null || \
	docker buildx create --name $(BUILDX_BUILDER_NAME) --platform linux/amd64,linux/arm64 --driver docker-container --use --driver-opt network=host || true
	docker buildx use $(BUILDX_BUILDER_NAME) || true

.PHONY: build-all  # for test purpose build all but output to /dev/null
build-all: BUILD_ARGS ?= --progress=plain --builder $(BUILDX_BUILDER_NAME) --platform linux/amd64,linux/arm64 --output type=tar,dest=/dev/null
build-all: buildx-create
	$(DOCKER_BUILDER) build $(BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -f go/Dockerfile     ./go
	$(DOCKER_BUILDER) build $(BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -f go/Dockerfile.full ./go
	$(DOCKER_BUILDER) build $(BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -f ui/Dockerfile     ./ui
	$(DOCKER_BUILDER) build $(BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -f python/Dockerfile ./python

.PHONY: push-test-agent
push-test-agent: buildx-create build-kagent-adk
	echo "Building FROM DOCKER_REGISTRY=$(DOCKER_REGISTRY)/$(DOCKER_REPO)/kagent-adk:$(VERSION)"
	$(DOCKER_BUILDER) build --push $(BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -t $(DOCKER_REGISTRY)/kebab:latest -f go/core/test/e2e/agents/kebab/Dockerfile ./go/core/test/e2e/agents/kebab
	kubectl apply --namespace kagent --context kind-$(KIND_CLUSTER_NAME) -f go/core/test/e2e/agents/kebab/agent.yaml
	$(DOCKER_BUILDER) build --push $(BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -t $(DOCKER_REGISTRY)/poem-flow:latest -f python/samples/crewai/poem_flow/Dockerfile ./python
	$(DOCKER_BUILDER) build --push $(BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -t $(DOCKER_REGISTRY)/basic-openai:latest -f python/samples/openai/basic_agent/Dockerfile ./python
	$(DOCKER_BUILDER) build --push $(BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -t $(DOCKER_REGISTRY)/langgraph-kebab:latest -f python/samples/langgraph/kebab/Dockerfile ./python

.PHONY: push-test-skill
push-test-skill: buildx-create
	echo "Building FROM DOCKER_REGISTRY=$(DOCKER_REGISTRY)/$(DOCKER_REPO)/kebab-maker:$(VERSION)"
	$(DOCKER_BUILDER) build --push $(BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -t $(DOCKER_REGISTRY)/kebab-maker:latest -f go/core/test/e2e/testdata/skills/kebab-maker/Dockerfile ./go/core/test/e2e/testdata/skills/kebab-maker

.PHONY: create-kind-cluster
create-kind-cluster:
	bash ./scripts/kind/setup-kind.sh
	bash ./scripts/kind/setup-metallb.sh

.PHONY: use-kind-cluster
use-kind-cluster:
	kind get kubeconfig --name $(KIND_CLUSTER_NAME) > /tmp/kind-config
	KUBECONFIG=~/.kube/config:/tmp/kind-config kubectl config view --merge --flatten > ~/.kube/config.tmp && mv ~/.kube/config.tmp ~/.kube/config && chmod $(KUBECONFIG_PERM) ~/.kube/config
	kubectl create namespace kagent || true
	kubectl config set-context --current --namespace kagent || true

.PHONY: delete-kind-cluster
delete-kind-cluster:
	kind delete cluster --name $(KIND_CLUSTER_NAME)

.PHONY: clean
clean: prune-kind-cluster
clean: prune-docker-images
	docker buildx rm $(BUILDX_BUILDER_NAME)  -f || true
	rm -rf ./go/core/bin

.PHONY: prune-kind-cluster
prune-kind-cluster:
	echo "Pruning dangling docker images from kind  ..."
	docker exec $(KIND_CLUSTER_NAME)-control-plane crictl images --no-trunc --quiet | \
	grep '<none>' | awk '{print $3}' | xargs -r -n1 docker exec $(KIND_CLUSTER_NAME)-control-plane crictl rmi || :

.PHONY: prune-docker-images
prune-docker-images:
	echo "Pruning dangling docker images ..."
	docker images --format '{{.Repository}}:{{.Tag}} {{.ID}}' | \
	grep -v ":$(VERSION) " | grep kagent | grep -v '<none>' | awk '{print $2}' | xargs -r docker rmi || :
	docker images --filter dangling=true -q | xargs -r docker rmi || :

.PHONY: build
build: buildx-create build-controller build-ui build-app build-golang-adk build-golang-adk-full build-skills-init
	@echo "Build completed successfully."
	@echo "Controller Image: $(CONTROLLER_IMG)"
	@echo "UI Image: $(UI_IMG)"
	@echo "App Image: $(APP_IMG)"
	@echo "Kagent ADK Image: $(KAGENT_ADK_IMG)"
	@echo "Golang ADK Image: $(GOLANG_ADK_IMG)"
	@echo "Golang ADK Full Image: $(GOLANG_ADK_FULL_IMG)"
	@echo "Skills Init Image: $(SKILLS_INIT_IMG)"

.PHONY: build-monitor
build-monitor: buildx-create
	watch docker exec -t  buildx_buildkit_$(BUILDX_BUILDER_NAME)0  ps

.PHONY: build-cli
build-cli:
	make -C go build

.PHONY: build-cli-local
build-cli-local:
	make -C go clean
	make -C go core/bin/kagent-local

.PHONY: build-img-versions
build-img-versions:
	@echo controller=$(CONTROLLER_IMG)
	@echo ui=$(UI_IMG)
	@echo app=$(APP_IMG)
	@echo kagent-adk=$(KAGENT_ADK_IMG)
	@echo golang-adk=$(GOLANG_ADK_IMG)
	@echo golang-adk-full=$(GOLANG_ADK_FULL_IMG)
	@echo skills-init=$(SKILLS_INIT_IMG)

.PHONY: lint
lint:
	make -C go lint
	make -C python lint

.PHONY: push
push: push-controller push-ui push-app push-kagent-adk push-golang-adk push-golang-adk-full


.PHONY: controller-manifests
controller-manifests:
	make -C go manifests
	cp go/api/config/crd/bases/* helm/kagent-crds/templates/

.PHONY: build-controller
build-controller: buildx-create controller-manifests
	$(DOCKER_BUILDER) build $(DOCKER_BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) --build-arg BUILD_PACKAGE=core/cmd/controller/main.go -t $(CONTROLLER_IMG) -f go/Dockerfile ./go

.PHONY: build-ui
build-ui: buildx-create
	$(DOCKER_BUILDER) build $(DOCKER_BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -t $(UI_IMG) -f ui/Dockerfile ./ui

.PHONY: build-kagent-adk
build-kagent-adk: buildx-create
		$(DOCKER_BUILDER) build $(DOCKER_BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) -t $(KAGENT_ADK_IMG) -f python/Dockerfile ./python

.PHONY: build-app
build-app: buildx-create build-kagent-adk
	$(DOCKER_BUILDER) build $(DOCKER_BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) --build-arg KAGENT_ADK_VERSION=$(KAGENT_ADK_IMAGE_TAG) --build-arg DOCKER_REGISTRY=$(DOCKER_REGISTRY) -t $(APP_IMG) -f python/Dockerfile.app ./python

# Substrate-shim variant of the kagent app image (see SUBSTRATE.md §19).
# Built on top of the standard app image with python/Dockerfile.substrate.
APP_SUBSTRATE_IMG ?= $(DOCKER_REGISTRY)/$(DOCKER_REPO)/app-substrate:dev

.PHONY: build-substrate-app
build-substrate-app: buildx-create build-app ## Build the substrate config-via-env shim layered on top of kagent/app.
	$(DOCKER_BUILDER) build $(DOCKER_BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) --build-arg KAGENT_ADK_VERSION=$(KAGENT_ADK_IMAGE_TAG) --build-arg DOCKER_REGISTRY=$(DOCKER_REGISTRY) --build-arg DOCKER_REPO=$(DOCKER_REPO) -t $(APP_SUBSTRATE_IMG) -f python/Dockerfile.substrate ./python
	@echo ">> pushed $(APP_SUBSTRATE_IMG)"

.PHONY: build-golang-adk
build-golang-adk: buildx-create
	$(DOCKER_BUILDER) build $(DOCKER_BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) --build-arg BUILD_PACKAGE=adk/cmd/main.go -t $(GOLANG_ADK_IMG) -f go/Dockerfile ./go

.PHONY: build-golang-adk-full
build-golang-adk-full: buildx-create
	$(DOCKER_BUILDER) build $(DOCKER_BUILD_ARGS) $(TOOLS_IMAGE_BUILD_ARGS) --build-arg BUILD_PACKAGE=adk/cmd/main.go -t $(GOLANG_ADK_FULL_IMG) -f go/Dockerfile.full ./go

.PHONY: build-skills-init
build-skills-init: buildx-create
	$(DOCKER_BUILDER) build $(DOCKER_BUILD_ARGS) -t $(SKILLS_INIT_IMG) -f docker/skills-init/Dockerfile docker/skills-init

.PHONY: helm-cleanup
helm-cleanup:
	rm -f ./$(HELM_DIST_FOLDER)/*.tgz

.PHONY: helm-test
helm-test: helm-version
	mkdir -p tmp
	echo $$(helm template kagent ./helm/kagent/ --namespace kagent --set providers.default=ollama																	| tee tmp/ollama.yaml 		| grep ^kind: | wc -l)
	echo $$(helm template kagent ./helm/kagent/ --namespace kagent --set providers.default=openAI       --set providers.openAI.apiKey=your-openai-api-key 			| tee tmp/openAI.yaml 		| grep ^kind: | wc -l)
	echo $$(helm template kagent ./helm/kagent/ --namespace kagent --set providers.default=anthropic    --set providers.anthropic.apiKey=your-anthropic-api-key 	| tee tmp/anthropic.yaml 	| grep ^kind: | wc -l)
	echo $$(helm template kagent ./helm/kagent/ --namespace kagent --set providers.default=azureOpenAI  --set providers.azureOpenAI.apiKey=your-openai-api-key		| tee tmp/azureOpenAI.yaml	| grep ^kind: | wc -l)
	echo $$(helm template kagent ./helm/kagent/ --namespace kagent --set providers.default=gemini       --set providers.gemini.apiKey=your-gemini-api-key 			| tee tmp/gemini.yaml 		| grep ^kind: | wc -l)
	helm plugin ls | grep unittest || helm plugin install https://github.com/helm-unittest/helm-unittest.git
	helm unittest helm/kagent

.PHONY: helm-agents
helm-agents:
	VERSION=$(VERSION) envsubst < helm/agents/k8s/Chart-template.yaml > helm/agents/k8s/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/agents/k8s
	VERSION=$(VERSION) envsubst < helm/agents/kgateway/Chart-template.yaml > helm/agents/kgateway/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/agents/kgateway
	VERSION=$(VERSION) envsubst < helm/agents/istio/Chart-template.yaml > helm/agents/istio/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/agents/istio
	VERSION=$(VERSION) envsubst < helm/agents/promql/Chart-template.yaml > helm/agents/promql/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/agents/promql
	VERSION=$(VERSION) envsubst < helm/agents/observability/Chart-template.yaml > helm/agents/observability/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/agents/observability
	VERSION=$(VERSION) envsubst < helm/agents/helm/Chart-template.yaml > helm/agents/helm/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/agents/helm
	VERSION=$(VERSION) envsubst < helm/agents/argo-rollouts/Chart-template.yaml > helm/agents/argo-rollouts/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/agents/argo-rollouts
	VERSION=$(VERSION) envsubst < helm/agents/cilium-policy/Chart-template.yaml > helm/agents/cilium-policy/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/agents/cilium-policy
	VERSION=$(VERSION) envsubst < helm/agents/cilium-debug/Chart-template.yaml > helm/agents/cilium-debug/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/agents/cilium-debug
	VERSION=$(VERSION) envsubst < helm/agents/cilium-manager/Chart-template.yaml > helm/agents/cilium-manager/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/agents/cilium-manager

.PHONY: helm-tools
helm-tools:
	VERSION=$(VERSION) envsubst < helm/tools/grafana-mcp/Chart-template.yaml > helm/tools/grafana-mcp/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/tools/grafana-mcp
	VERSION=$(VERSION) envsubst < helm/tools/querydoc/Chart-template.yaml > helm/tools/querydoc/Chart.yaml
	helm package -d $(HELM_DIST_FOLDER) helm/tools/querydoc

.PHONY: helm-version
helm-version: helm-cleanup helm-agents helm-tools
	VERSION=$(VERSION) KMCP_VERSION=$(KMCP_VERSION) envsubst < helm/kagent-crds/Chart-template.yaml > helm/kagent-crds/Chart.yaml
	VERSION=$(VERSION) KMCP_VERSION=$(KMCP_VERSION) envsubst < helm/kagent/Chart-template.yaml > helm/kagent/Chart.yaml
	helm dependency update helm/kagent
	helm dependency update helm/kagent-crds
	helm package -d $(HELM_DIST_FOLDER) helm/kagent-crds
	helm package -d $(HELM_DIST_FOLDER) helm/kagent

.PHONY: helm-install-provider
helm-install-provider: helm-version check-api-key
	helm $(HELM_ACTION) kagent-crds helm/kagent-crds \
		--namespace kagent \
		--create-namespace \
		--history-max 2    \
		--timeout 5m 			\
		--kube-context kind-$(KIND_CLUSTER_NAME) \
		--wait \
		--set kmcp.enabled=$(KMCP_ENABLED)
	helm $(HELM_ACTION) kagent helm/kagent \
		--namespace kagent \
		--create-namespace \
		--history-max 2    \
		--timeout 5m       \
		--kube-context kind-$(KIND_CLUSTER_NAME) \
		--wait \
		--set ui.service.type=LoadBalancer \
		--set registry=$(DOCKER_REGISTRY) \
		--set imagePullPolicy=Always \
		--set tag=$(VERSION) \
		--set controller.loglevel=debug \
		--set controller.image.pullPolicy=Always \
		--set ui.image.pullPolicy=Always \
		--set controller.service.type=LoadBalancer \
		--set providers.openAI.apiKey=$(OPENAI_API_KEY) \
		--set providers.azureOpenAI.apiKey=$(AZUREOPENAI_API_KEY) \
		--set providers.anthropic.apiKey=$(ANTHROPIC_API_KEY) \
		--set providers.gemini.apiKey=$(GOOGLE_API_KEY) \
		--set providers.default=$(KAGENT_DEFAULT_MODEL_PROVIDER) \
		--set kmcp.enabled=$(KMCP_ENABLED) \
		--set kmcp.image.tag=$(KMCP_VERSION) \
		--set querydoc.openai.apiKey=$(OPENAI_API_KEY) \
		--set database.postgres.bundled.image.repository=pgvector \
		--set database.postgres.bundled.image.name=pgvector \
		--set database.postgres.bundled.image.tag=pg18-trixie \
		--set database.postgres.vectorEnabled=true \
		$(KAGENT_HELM_EXTRA_ARGS)

.PHONY: helm-install
helm-install: build
helm-install: helm-install-provider

.PHONY: helm-test-install
helm-test-install: HELM_ACTION+="--dry-run"
helm-test-install: helm-install-provider
# Test install with dry-run
# Example: `make helm-test-install | tee helm-test-install.log`

.PHONY: helm-uninstall
helm-uninstall:
	helm uninstall kagent --namespace kagent --kube-context kind-$(KIND_CLUSTER_NAME) --wait
	helm uninstall kagent-crds --namespace kagent --kube-context kind-$(KIND_CLUSTER_NAME) --wait

.PHONY: helm-publish
helm-publish: helm-version
	helm push ./$(HELM_DIST_FOLDER)/kagent-crds-$(VERSION).tgz $(HELM_REPO)/kagent/helm
	helm push ./$(HELM_DIST_FOLDER)/kagent-$(VERSION).tgz $(HELM_REPO)/kagent/helm
	helm push ./$(HELM_DIST_FOLDER)/helm-agent-$(VERSION).tgz $(HELM_REPO)/kagent/agents
	helm push ./$(HELM_DIST_FOLDER)/istio-agent-$(VERSION).tgz $(HELM_REPO)/kagent/agents
	helm push ./$(HELM_DIST_FOLDER)/promql-agent-$(VERSION).tgz $(HELM_REPO)/kagent/agents
	helm push ./$(HELM_DIST_FOLDER)/observability-agent-$(VERSION).tgz $(HELM_REPO)/kagent/agents
	helm push ./$(HELM_DIST_FOLDER)/argo-rollouts-agent-$(VERSION).tgz $(HELM_REPO)/kagent/agents
	helm push ./$(HELM_DIST_FOLDER)/cilium-policy-agent-$(VERSION).tgz $(HELM_REPO)/kagent/agents
	helm push ./$(HELM_DIST_FOLDER)/cilium-manager-agent-$(VERSION).tgz $(HELM_REPO)/kagent/agents
	helm push ./$(HELM_DIST_FOLDER)/cilium-debug-agent-$(VERSION).tgz $(HELM_REPO)/kagent/agents
	helm push ./$(HELM_DIST_FOLDER)/kgateway-agent-$(VERSION).tgz $(HELM_REPO)/kagent/agents

.PHONY: kagent-cli-install
kagent-cli-install: use-kind-cluster build-cli-local helm-version helm-install-provider
	KAGENT_HELM_REPO=./helm/ ./go/core/bin/kagent-local dashboard

.PHONY: kagent-cli-port-forward
kagent-cli-port-forward: use-kind-cluster
	@echo "Port forwarding to kagent CLI..."
	kubectl port-forward -n kagent service/kagent-controller 8083:8083

.PHONY: kagent-ui-port-forward
kagent-ui-port-forward: use-kind-cluster
	open http://localhost:8082/
	kubectl port-forward -n kagent service/kagent-ui 8082:8080

.PHONY: kagent-addon-install
kagent-addon-install: use-kind-cluster
	# to test the kagent addons - installing istio, grafana, prometheus, metrics-server
	istioctl install --set profile=demo -y
	kubectl apply --context kind-$(KIND_CLUSTER_NAME) -f contrib/addons/grafana.yaml
	kubectl apply --context kind-$(KIND_CLUSTER_NAME) -f contrib/addons/prometheus.yaml
	kubectl apply --context kind-$(KIND_CLUSTER_NAME) -f contrib/addons/metrics-server.yaml
	# wait for pods to be ready
	kubectl wait --context kind-$(KIND_CLUSTER_NAME) --for=condition=Ready pod -l app.kubernetes.io/name=grafana    -n kagent --timeout=60s
	kubectl wait --context kind-$(KIND_CLUSTER_NAME) --for=condition=Ready pod -l app.kubernetes.io/name=prometheus -n kagent --timeout=60s

.PHONY: open-dev-container
open-dev-container:
	@echo "Building and starting dev container..."
	devcontainer up --workspace-folder .

.PHONY: otel-local
otel-local:
	docker rm -f jaeger-desktop || true
	docker run -d --name jaeger-desktop --restart=always -p 16686:16686 -p 4317:4317 -p 4318:4318 jaegertracing/jaeger:2.7.0
	@echo "Jaeger UI available at http://localhost:16686/"

.PHONY: kind-debug
kind-debug:
	@echo "Debugging the kind cluster..."
	@echo "Enter the kind cluster control plane container..."
	docker exec -it $(KIND_CLUSTER_NAME)-control-plane bash -c 'apt-get update && apt-get install -y btop htop'
	docker exec -it $(KIND_CLUSTER_NAME)-control-plane bash -c 'btop --utf-force'

.PHONY: audit
audit:
	echo "Running CVE audit GO"
	make -C go govulncheck
	echo "Running CVE audit UI"
	make -C ui audit
	echo "Running CVE audit PYTHON"
	make -C python audit

.PHONY: report/image-cve
report/image-cve: audit build
	echo "Running CVE scan :: CVE -> CSV ... reports/$(SEMVER)/"
	grype docker:$(CONTROLLER_IMG) -o template -t reports/cve-report.tmpl --file reports/$(SEMVER)/controller-cve.csv
	grype docker:$(APP_IMG)        -o template -t reports/cve-report.tmpl --file reports/$(SEMVER)/app-cve.csv
	grype docker:$(UI_IMG)         -o template -t reports/cve-report.tmpl --file reports/$(SEMVER)/ui-cve.csv
	grype docker:$(SKILLS_INIT_IMG) -o template -t reports/cve-report.tmpl --file reports/$(SEMVER)/skills-init-cve.csv

##@ Substrate Demo (kagent backed by agent-substrate; see SUBSTRATE.md, demo/substrate-poc/DEMO.md)
#
# The demo runs the controller OUT-OF-CLUSTER (binary on this host) against
# whatever cluster `kubectl config current-context` points at. Substrate
# itself must already be installed in `ate-system` — these targets do not
# bring up substrate.

SUBSTRATE_DEMO_NS       ?= kagent-substrate-poc
SUBSTRATE_DEMO_POOL     ?= poc-pool
SUBSTRATE_DEMO_IMG      ?= localhost:5001/substrate-poc-agent:p3
SUBSTRATE_DEMO_RUNSC_SHA ?= a397be1abc2420d26bce6c70e6e2ff96c73aaaab929756c56f5e2089ea842b63
SUBSTRATE_DEMO_RUNSC_URL ?= https://storage.googleapis.com/gvisor/releases/nightly/2026-05-19/x86_64/runsc
SUBSTRATE_DEMO_KIND_CTR ?= kind-control-plane
SUBSTRATE_DEMO_PG_NAME  ?= kagent-substrate-poc-pg
SUBSTRATE_DEMO_PG_IMG   ?= pgvector/pgvector:pg16
# Use uncommon host ports to avoid colliding with other dev databases /
# controllers / port-forwards the user may already have running.
SUBSTRATE_DEMO_PG_PORT     ?= 15432
SUBSTRATE_DEMO_CTRL_BIN ?= /tmp/kagent-controller-substrate-demo
SUBSTRATE_DEMO_CTRL_LOG ?= /tmp/kagent-controller-substrate-demo.log
SUBSTRATE_DEMO_PF_LOG   ?= /tmp/kagent-substrate-demo-pf.log
SUBSTRATE_DEMO_ATENET_PORT ?= 18000
SUBSTRATE_DEMO_API_PORT    ?= 14443
SUBSTRATE_DEMO_HEALTH_PORT ?= 18092
SUBSTRATE_DEMO_HTTP_PORT   ?= 18093
SUBSTRATE_DEMO_IDLE       ?= 30s
SUBSTRATE_DEMO_SWEEP      ?= 10s
# When set, the controller is started with --substrate-agent-image=<this>,
# which makes the substrate backend swap in the substrate-shim kagent ADK
# image (built via `make build-substrate-app`) for every SandboxAgent it
# emits. Leave empty for the Phase-0 stand-in / BYO demo path; set for A2's
# real-ADK path. See SUBSTRATE.md §19.
SUBSTRATE_DEMO_AGENT_IMAGE ?=
# Ateom-gvisor image for the auto-provisioned WorkerPool (B2). When set, the
# controller's WorkerPoolEnsurer creates the WorkerPool at startup, so
# 01-substrate.yaml no longer carries it. Defaults to the same image substrate's
# counter demo uses (proved to work on this kind cluster).
SUBSTRATE_DEMO_ATEOM_IMAGE ?= localhost:5001/ateom-gvisor-34ef0400225d9f0582e4c906399c2137@sha256:6e7b187826fa2195e2b961d57f24bd74dc998e1e1852c949885dbc81aa376159
SUBSTRATE_DEMO_POOL_REPLICAS ?= 2

.PHONY: demo-substrate-preflight
demo-substrate-preflight: ## Verify substrate is installed and the kind-registry is reachable.
	@echo ">> verifying kubectl context: $$(kubectl config current-context)"
	@kubectl get ns ate-system >/dev/null 2>&1 || { echo "FAIL: ate-system namespace missing — install substrate first"; exit 1; }
	@kubectl get crd actortemplates.ate.dev workerpools.ate.dev >/dev/null 2>&1 || { echo "FAIL: substrate CRDs missing"; exit 1; }
	@docker ps --format '{{.Names}}' | grep -q '^kind-registry$$' || { echo "FAIL: kind-registry container not running"; exit 1; }
	@docker ps --format '{{.Names}}' | grep -q '^$(SUBSTRATE_DEMO_KIND_CTR)$$' || { echo "FAIL: kind node container '$(SUBSTRATE_DEMO_KIND_CTR)' not running"; exit 1; }
	@which kubectl-ate >/dev/null 2>&1 || { echo "FAIL: kubectl-ate plugin not on PATH (install from agent-substrate/substrate)"; exit 1; }
	@echo ">> OK: substrate installed, kind-registry up, kubectl-ate present"

.PHONY: demo-substrate-runsc
demo-substrate-runsc: ## Pre-stage the gVisor runsc binary on the kind node so atelet skips the slow GCS download.
	@if docker exec $(SUBSTRATE_DEMO_KIND_CTR) test -x /run/ateom-gvisor/static-files/runsc-$(SUBSTRATE_DEMO_RUNSC_SHA); then \
		echo ">> runsc already staged on $(SUBSTRATE_DEMO_KIND_CTR)"; \
	else \
		echo ">> staging runsc on $(SUBSTRATE_DEMO_KIND_CTR) (may take ~2 min on slow networks)..."; \
		docker exec $(SUBSTRATE_DEMO_KIND_CTR) sh -c 'mkdir -p /run/ateom-gvisor/static-files && curl -fsSL -o /run/ateom-gvisor/static-files/runsc-$(SUBSTRATE_DEMO_RUNSC_SHA) $(SUBSTRATE_DEMO_RUNSC_URL) && chmod 0755 /run/ateom-gvisor/static-files/runsc-$(SUBSTRATE_DEMO_RUNSC_SHA)'; \
		echo ">> staged: $$(docker exec $(SUBSTRATE_DEMO_KIND_CTR) sha256sum /run/ateom-gvisor/static-files/runsc-$(SUBSTRATE_DEMO_RUNSC_SHA))"; \
	fi

.PHONY: demo-substrate-image
demo-substrate-image: ## Build the Phase-0 stand-in agent image and push to kind-registry.
	@cd demo/substrate-poc && docker build -t $(SUBSTRATE_DEMO_IMG) . >/dev/null
	@docker push $(SUBSTRATE_DEMO_IMG) >/dev/null
	@echo ">> pushed $(SUBSTRATE_DEMO_IMG)"

.PHONY: demo-substrate-crds
demo-substrate-crds: ## Install kagent CRDs (large Agent/SandboxAgent need --server-side).
	@kubectl apply -f helm/kagent-crds/templates/kagent.dev_modelconfigs.yaml >/dev/null
	@kubectl apply -f helm/kagent-crds/templates/kagent.dev_modelproviderconfigs.yaml >/dev/null
	@kubectl apply -f helm/kagent-crds/templates/kagent.dev_memories.yaml >/dev/null
	@kubectl apply -f helm/kagent-crds/templates/kagent.dev_remotemcpservers.yaml >/dev/null
	@kubectl apply -f helm/kagent-crds/templates/kagent.dev_toolservers.yaml >/dev/null
	@kubectl apply -f helm/kagent-crds/templates/kagent.dev_agentharnesses.yaml >/dev/null
	@kubectl apply --server-side -f helm/kagent-crds/templates/kagent.dev_agents.yaml >/dev/null
	@kubectl apply --server-side -f helm/kagent-crds/templates/kagent.dev_sandboxagents.yaml >/dev/null
	@echo ">> kagent CRDs installed"

.PHONY: demo-substrate-pg
demo-substrate-pg: ## Start a local Postgres+pgvector docker for the controller.
	@if docker ps --format '{{.Names}}' | grep -q '^$(SUBSTRATE_DEMO_PG_NAME)$$'; then \
		echo ">> $(SUBSTRATE_DEMO_PG_NAME) already running"; \
	else \
		docker rm -f $(SUBSTRATE_DEMO_PG_NAME) >/dev/null 2>&1 || true; \
		docker run -d --name $(SUBSTRATE_DEMO_PG_NAME) -p $(SUBSTRATE_DEMO_PG_PORT):5432 \
			-e POSTGRES_PASSWORD=kagent -e POSTGRES_USER=postgres -e POSTGRES_DB=kagent \
			$(SUBSTRATE_DEMO_PG_IMG) >/dev/null || { \
				echo "FAIL: docker run for $(SUBSTRATE_DEMO_PG_NAME) failed (port $(SUBSTRATE_DEMO_PG_PORT) busy?)"; \
				docker logs $(SUBSTRATE_DEMO_PG_NAME) 2>&1 | tail -5; \
				exit 1; \
			}; \
		i=0; while ! docker exec $(SUBSTRATE_DEMO_PG_NAME) pg_isready -U postgres >/dev/null 2>&1; do \
			i=$$((i+1)); \
			if [ $$i -gt 30 ]; then \
				echo "FAIL: $(SUBSTRATE_DEMO_PG_NAME) never became ready"; \
				docker logs $(SUBSTRATE_DEMO_PG_NAME) 2>&1 | tail -10; \
				exit 1; \
			fi; \
			sleep 1; \
		done; \
		echo ">> $(SUBSTRATE_DEMO_PG_NAME) ready on :$(SUBSTRATE_DEMO_PG_PORT)"; \
	fi

.PHONY: demo-substrate-pf
demo-substrate-pf: ## Start background port-forwards for atenet-router and substrate's Control API.
	@pkill -f "port-forward.*svc/atenet-router" 2>/dev/null || true
	@pkill -f "port-forward.*svc/api"           2>/dev/null || true
	@nohup kubectl port-forward -n ate-system svc/atenet-router $(SUBSTRATE_DEMO_ATENET_PORT):80 >>$(SUBSTRATE_DEMO_PF_LOG) 2>&1 &
	@nohup kubectl port-forward -n ate-system svc/api $(SUBSTRATE_DEMO_API_PORT):443        >>$(SUBSTRATE_DEMO_PF_LOG) 2>&1 &
	@until nc -z localhost $(SUBSTRATE_DEMO_ATENET_PORT) >/dev/null 2>&1; do sleep 1; done
	@until nc -z localhost $(SUBSTRATE_DEMO_API_PORT)    >/dev/null 2>&1; do sleep 1; done
	@echo ">> port-forwards live on :$(SUBSTRATE_DEMO_ATENET_PORT) (atenet) and :$(SUBSTRATE_DEMO_API_PORT) (api)"

.PHONY: demo-substrate-pool
demo-substrate-pool: ## Apply the demo Namespace (the WorkerPool is now auto-provisioned by the controller; see B2).
	@kubectl apply -f demo/substrate-poc/01-substrate.yaml
	@echo ">> applied 01-substrate.yaml (Namespace only; WorkerPool managed by controller via --substrate-worker-pool-ateom-image)"

.PHONY: demo-substrate-controller
demo-substrate-controller: ## Build the controller binary and start it in the background with --substrate-* flags.
	@pkill -f "$(SUBSTRATE_DEMO_CTRL_BIN)" 2>/dev/null || true
	@sleep 1
	@cd go && go build -o $(SUBSTRATE_DEMO_CTRL_BIN) ./core/cmd/controller
	@nohup $(SUBSTRATE_DEMO_CTRL_BIN) \
		--postgres-database-url="postgres://postgres:kagent@127.0.0.1:$(SUBSTRATE_DEMO_PG_PORT)/kagent?sslmode=disable" \
		--database-vector-enabled=true \
		--watch-namespaces="$(SUBSTRATE_DEMO_NS)" \
		--metrics-bind-address=0 \
		--health-probe-bind-address=:$(SUBSTRATE_DEMO_HEALTH_PORT) \
		--http-server-address=:$(SUBSTRATE_DEMO_HTTP_PORT) \
		--a2a-base-url="http://127.0.0.1:$(SUBSTRATE_DEMO_HTTP_PORT)" \
		--leader-elect=false \
		--substrate-worker-pool-name=$(SUBSTRATE_DEMO_POOL) \
		--substrate-worker-pool-namespace=$(SUBSTRATE_DEMO_NS) \
		--substrate-snapshots-location=gs://ate-snapshots \
		--substrate-pause-image="gcr.io/gke-release/pause@sha256:bcbd57ba5653580ec647b16d8163cdd1112df3609129b01f912a8032e48265da" \
		--substrate-control-endpoint=localhost:$(SUBSTRATE_DEMO_API_PORT) \
		--substrate-router-url=http://localhost:$(SUBSTRATE_DEMO_ATENET_PORT) \
		--substrate-idle-timeout=$(SUBSTRATE_DEMO_IDLE) \
		--substrate-idle-sweep-interval=$(SUBSTRATE_DEMO_SWEEP) \
		--substrate-worker-pool-ateom-image=$(SUBSTRATE_DEMO_ATEOM_IMAGE) \
		--substrate-worker-pool-replicas=$(SUBSTRATE_DEMO_POOL_REPLICAS) \
		$(if $(SUBSTRATE_DEMO_AGENT_IMAGE),--substrate-agent-image=$(SUBSTRATE_DEMO_AGENT_IMAGE),) \
		>$(SUBSTRATE_DEMO_CTRL_LOG) 2>&1 &
	@i=0; while ! grep -qE "Starting workers|substrate-idle-suspender.*starting" $(SUBSTRATE_DEMO_CTRL_LOG) 2>/dev/null; do \
		i=$$((i+1)); \
		if [ $$i -gt 60 ]; then \
			echo "FAIL: controller never reached Starting workers (last 20 log lines):"; \
			tail -20 $(SUBSTRATE_DEMO_CTRL_LOG); \
			exit 1; \
		fi; \
		if grep -qE "address already in use|FATAL|unable to (create manager|connect)" $(SUBSTRATE_DEMO_CTRL_LOG) 2>/dev/null; then \
			echo "FAIL: controller startup error (last 10 log lines):"; \
			tail -10 $(SUBSTRATE_DEMO_CTRL_LOG); \
			exit 1; \
		fi; \
		sleep 1; \
	done
	@echo ">> controller running (log: $(SUBSTRATE_DEMO_CTRL_LOG))"

.PHONY: demo-substrate-agents
demo-substrate-agents: ## Apply the four demo SandboxAgents (BYO type, no model creds required).
	@kubectl apply -f demo/substrate-poc/03-agents.yaml
	@echo ">> waiting for all SandboxAgents to be Ready..."
	@kubectl wait sandboxagent -n $(SUBSTRATE_DEMO_NS) --all --for=condition=Ready --timeout=300s
	@echo ">> all SandboxAgents Ready"

.PHONY: demo-substrate-up
demo-substrate-up: demo-substrate-preflight demo-substrate-runsc demo-substrate-image demo-substrate-crds demo-substrate-pg demo-substrate-pf demo-substrate-pool demo-substrate-controller demo-substrate-agents demo-substrate-status ## Full setup. Idempotent: re-run any time to converge.

.PHONY: demo-substrate-status
demo-substrate-status: ## Print the dashboard view (SandboxAgents, workers, actors, snapshot size).
	@SUBSTRATE_DEMO_NS=$(SUBSTRATE_DEMO_NS) SUBSTRATE_DEMO_POOL=$(SUBSTRATE_DEMO_POOL) ./demo/substrate-poc/status.sh

.PHONY: demo-substrate-traffic
demo-substrate-traffic: ## Send one request to each SandboxAgent via the kagent A2A mux (drives resume).
	@for a in demo-alpha demo-bravo demo-charlie demo-delta; do \
		printf "  → %-15s " "$$a"; \
		curl -sS --max-time 30 -X POST -H "Content-Type: application/json" \
			-d '{"jsonrpc":"2.0","id":"1","method":"message/send","params":{"message":{"role":"user","parts":[{"kind":"text","text":"ping"}]}}}' \
			http://localhost:$(SUBSTRATE_DEMO_HTTP_PORT)/api/a2a-sandboxes/$(SUBSTRATE_DEMO_NS)/$$a/ \
			>/dev/null && echo "200" || echo "ERR"; \
	done

.PHONY: demo-substrate-logs
demo-substrate-logs: ## Tail the controller log (Ctrl-C to stop).
	@tail -f $(SUBSTRATE_DEMO_CTRL_LOG)

.PHONY: demo-substrate-down
demo-substrate-down: ## Tear down demo (controller, port-forwards, pg, agents, pool, ns). Leaves substrate and CRDs alone.
	@# Delete SandboxAgents FIRST while the controller is still alive — the
	@# kagent.dev/substrate-actor finalizer requires OnDelete to run before
	@# K8s can remove the CR. If we killed the controller first, every
	@# SandboxAgent would be stuck in Terminating forever.
	@-kubectl delete -f demo/substrate-poc/03-agents.yaml --ignore-not-found --wait=true --timeout=60s 2>/dev/null
	@-kubectl delete -f demo/substrate-poc/04-real-agent.yaml --ignore-not-found --wait=true --timeout=60s 2>/dev/null
	@# Force-clear any straggler finalizers — happens when an old controller
	@# died mid-cleanup or substrate was unreachable. Safe to run; no-op when
	@# nothing's left.
	@-for sa in $$(kubectl get sandboxagent -n $(SUBSTRATE_DEMO_NS) -o name 2>/dev/null); do \
		kubectl patch $$sa -n $(SUBSTRATE_DEMO_NS) --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null; \
	done
	@-pkill -f "$(SUBSTRATE_DEMO_CTRL_BIN)"            2>/dev/null
	@-pkill -f "port-forward.*svc/atenet-router"       2>/dev/null
	@-pkill -f "port-forward.*svc/api"                 2>/dev/null
	@-docker rm -f $(SUBSTRATE_DEMO_PG_NAME)           2>/dev/null
	@-kubectl delete -f demo/substrate-poc/01-substrate.yaml --ignore-not-found 2>/dev/null
	@-kubectl delete ns $(SUBSTRATE_DEMO_NS) --ignore-not-found --timeout=60s 2>/dev/null
	@echo ">> demo torn down (substrate + CRDs left in place)"
