# metallb-iad — MetalLB IP Allocation Driver, the reference per-class driver
# for the address-controller core.

# Image coordinates. TAG defaults to something unique per commit; override for
# releases (e.g. make docker-push TAG=v0.1.0) or floating tags (TAG=main).
IMG_REPO ?= ghcr.io/cozystack/metallb-iad
TAG ?= $(shell git describe --tags --always --dirty)
IMG ?= $(IMG_REPO):$(TAG)
PLATFORMS ?= linux/amd64,linux/arm64

CHART := chart/metallb-iad
# Chart.yaml carries a 0.0.0 placeholder; the real version is stamped here.
CHART_VERSION ?= 0.1.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate the ClusterRole straight into the Helm chart (the CRDs belong to address-controller).
	$(CONTROLLER_GEN) rbac:roleName=metallb-iad paths="./..." \
		output:rbac:artifacts:config=$(CHART)/templates

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests fmt vet ## Run tests.
	go test ./... -coverprofile cover.out

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

##@ Build

.PHONY: build
build: manifests fmt vet ## Build the driver binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests fmt vet ## Run the driver from your host.
	go run ./cmd/main.go

.PHONY: docker-build
docker-build: ## Build the driver image for the host platform.
	$(CONTAINER_TOOL) build -t $(IMG) .

.PHONY: docker-push
docker-push: ## Push the driver image.
	$(CONTAINER_TOOL) push $(IMG)

.PHONY: docker-buildx
docker-buildx: ## Build and push a multi-platform driver image (requires buildx).
	$(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) -t $(IMG) .

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint the chart.
	helm lint $(CHART)

.PHONY: helm-package
helm-package: manifests helm-lint ## Package the chart into dist/ with the version stamped.
	mkdir -p dist
	helm package $(CHART) --version $(CHART_VERSION) --app-version $(TAG) -d dist

.PHONY: deploy
deploy: manifests ## Install/upgrade the chart into the cluster in ~/.kube/config.
	helm upgrade --install metallb-iad $(CHART) --namespace metallb-iad-system --create-namespace

.PHONY: undeploy
undeploy: ## Uninstall the chart.
	helm uninstall metallb-iad --namespace metallb-iad-system

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint

## Tool Versions
CONTROLLER_TOOLS_VERSION ?= v0.17.1
GOLANGCI_LINT_VERSION ?= v2.14.0

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VERSION))

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f "$(1)-$(3)" ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
rm -f $(1) || true ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv $(1) $(1)-$(3) ;\
} ;\
ln -sf $(1)-$(3) $(1)
endef
