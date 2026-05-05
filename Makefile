
# Image URLs for building/pushing
IMG             ?= ghcr.io/henres/leilfs-operator/saunafs-operator:latest
NFS_GANESHA_IMG ?= ghcr.io/henres/leilfs-operator/nfs-ganesha:latest
# ENVTEST_K8S_VERSION refers to the version of kubebuilder assets to be downloaded by envtest binary.
ENVTEST_K8S_VERSION = 1.29.0

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

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

##@ Development

.PHONY: manifests
manifests: controller-gen ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: generate
generate: controller-gen ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="scripts/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

# Utilize Kind or modify the e2e tests to load the image locally, enabling compatibility with other vendors.
.PHONY: test-e2e  # Run the e2e tests against a Kind k8s instance that is spun up.
test-e2e:
	go test ./test/e2e/ -v -ginkgo.v

# PLUGIN_BIN     - path to the plugin binary (default: bin/kubectl-saunafs)
# PLUGIN_CLUSTER - LeilFSCluster name to test against (default: leilfscluster-sample)
# PLUGIN_NS      - namespace (default: default)
# Requires a running Kind cluster: make kind-reset
PLUGIN_BIN     ?= bin/kubectl-saunafs
PLUGIN_CLUSTER ?= leilfscluster-sample
PLUGIN_NS      ?= default

.PHONY: test-plugin
test-plugin: build-plugin ## Run kubectl-saunafs plugin smoke tests against a live cluster.
	PLUGIN_BIN=$(PLUGIN_BIN) PLUGIN_CLUSTER=$(PLUGIN_CLUSTER) PLUGIN_NS=$(PLUGIN_NS) \
	  go test ./test/e2e/ -v --ginkgo.v --ginkgo.focus "kubectl-saunafs" --ginkgo.timeout 10m

.PHONY: lint
lint: golangci-lint ## Run golangci-lint linter & yamllint
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: golangci-lint ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: build-plugin
build-plugin: fmt vet ## Build the kubectl-saunafs plugin binary.
	go build -o bin/kubectl-saunafs ./cmd/kubectl-saunafs/

.PHONY: install-plugin
install-plugin: build-plugin ## Install the kubectl-saunafs plugin into GOBIN (or ~/go/bin).
	cp bin/kubectl-saunafs $(GOBIN)/kubectl-saunafs
	@echo "Plugin installed to $(GOBIN)/kubectl-saunafs"
	@echo "Make sure $(GOBIN) is in your PATH, then use: kubectl saunafs --help"

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build the operator image.
	$(CONTAINER_TOOL) build -t ${IMG} -f docker/operator.Dockerfile .

.PHONY: docker-push
docker-push: ## Push the operator image.
	$(CONTAINER_TOOL) push ${IMG}

.PHONY: docker-build-nfs-ganesha
docker-build-nfs-ganesha: ## Build the NFS-Ganesha image via docker compose.
	OPERATOR_IMAGE_TAG=$(shell echo ${NFS_GANESHA_IMG} | sed 's/.*://') \
	  docker compose -f docker/docker-compose.yml build nfs-ganesha

.PHONY: docker-push-nfs-ganesha
docker-push-nfs-ganesha: ## Push the NFS-Ganesha image via docker compose.
	docker compose -f docker/docker-compose.yml push nfs-ganesha

.PHONY: docker-build-all
docker-build-all: ## Build both operator and NFS-Ganesha images via docker compose.
	OPERATOR_IMAGE_TAG=$(shell echo ${IMG} | sed 's/.*://') \
	  docker compose -f docker/docker-compose.yml build

.PHONY: docker-push-all
docker-push-all: docker-build-all ## Build and push both images via docker compose.
	OPERATOR_IMAGE_TAG=$(shell echo ${IMG} | sed 's/.*://') \
	  docker compose -f docker/docker-compose.yml push

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' docker/operator.Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name project-v3-builder
	$(CONTAINER_TOOL) buildx use project-v3-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm project-v3-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate kustomize ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	@if [ -d "config/crd" ]; then \
		$(KUSTOMIZE) build config/crd > dist/install.yaml; \
	fi
	echo "---" >> dist/install.yaml  # Add a document separator before appending
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default >> dist/install.yaml

##@ Monitoring

.PHONY: monitoring-up
monitoring-up: ## Install kube-prometheus-stack + ServiceMonitor + dashboards in the current cluster.
	./hack/monitoring/install.sh

.PHONY: monitoring-down
monitoring-down: ## Uninstall the monitoring stack and delete the namespace.
	./hack/monitoring/uninstall.sh

.PHONY: monitoring-dashboards
monitoring-dashboards: ## Re-sync hack/monitoring/dashboards/*.json into the Grafana ConfigMap.
	./hack/monitoring/sync-dashboards.sh

.PHONY: monitoring-prometheus
monitoring-prometheus: ## Port-forward Prometheus on http://localhost:9090.
	kubectl --context kind-saunafs-operator -n monitoring port-forward svc/kube-prom-stack-kube-prome-prometheus 9090:9090

.PHONY: monitoring-grafana
monitoring-grafana: ## Port-forward Grafana on http://localhost:3000 (admin/admin).
	kubectl --context kind-saunafs-operator -n monitoring port-forward svc/kube-prom-stack-grafana 3000:80

##@ Kind

.PHONY: kind-prepare-dirs
kind-prepare-dirs: ## Create host directories bind-mounted as /mnt/hdd001 and /mnt/hdd002 in each Kind worker node.
	sudo mkdir -p \
	  $(KIND_DATA_DIR)/worker-master/hdd001 \
	  $(KIND_DATA_DIR)/worker-master/hdd002 \
	  $(KIND_DATA_DIR)/worker-chunk-1/hdd001 \
	  $(KIND_DATA_DIR)/worker-chunk-1/hdd002 \
	  $(KIND_DATA_DIR)/worker-chunk-2/hdd001 \
	  $(KIND_DATA_DIR)/worker-chunk-2/hdd002

.PHONY: kind-create
kind-create: kind-prepare-dirs ## Create a Kind cluster using scripts/kind-config.yaml.
	$(KIND) create cluster --name $(KIND_CLUSTER_NAME) --config $(KIND_CONFIG)

.PHONY: kind-delete
kind-delete: ## Delete the Kind cluster.
	$(KIND) delete cluster --name $(KIND_CLUSTER_NAME)

.PHONY: kind-load
kind-load: docker-build ## Build the manager image and load it into the Kind cluster.
	$(KIND) load docker-image $(KIND_IMG) --name $(KIND_CLUSTER_NAME)

.PHONY: kind-install
kind-install: manifests kustomize ## Install CRDs into the Kind cluster.
	$(KUSTOMIZE) build config/crd | $(KIND_KUBECTL) apply -f -

.PHONY: kind-deploy
kind-deploy: kind-load kind-install ## Load image and deploy the operator to the Kind cluster.
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(KIND_IMG)
	$(KUSTOMIZE) build config/default | $(KIND_KUBECTL) apply -f -

.PHONY: kind-undeploy
kind-undeploy: kustomize ## Remove the operator from the Kind cluster.
	$(KUSTOMIZE) build config/default | $(KIND_KUBECTL) delete --ignore-not-found=true -f -

.PHONY: kind-create-pull-secret
kind-create-pull-secret: ## Create ghcr.io pull secret and patch default SA in all workload namespaces. Requires GHCR_PAT=<token>.
	@if [ -z "$(GHCR_PAT)" ]; then \
	  echo "ERROR: GHCR_PAT is not set. Usage: make kind-create-pull-secret GHCR_PAT=ghp_xxxx"; \
	  exit 1; \
	fi
	@for ns in $(PULL_SECRET_NAMESPACES); do \
	  echo "==> Namespace: $${ns}"; \
	  $(KIND_KUBECTL) create namespace "$${ns}" --dry-run=client -o yaml | $(KIND_KUBECTL) apply -f -; \
	  $(KIND_KUBECTL) create secret docker-registry $(PULL_SECRET_NAME) \
	    --namespace="$${ns}" \
	    --docker-server=ghcr.io \
	    --docker-username=$(GHCR_USER) \
	    --docker-password=$(GHCR_PAT) \
	    --dry-run=client -o yaml | $(KIND_KUBECTL) apply -f -; \
	  $(KIND_KUBECTL) patch serviceaccount default \
	    --namespace="$${ns}" \
	    -p '{"imagePullSecrets":[{"name":"$(PULL_SECRET_NAME)"}]}'; \
	done


.PHONY: nfs-ganesha-build
nfs-ganesha-build: ## Build the custom NFS-Ganesha image from docker/nfs-ganesha.Dockerfile.
	docker build -t $(NFS_GANESHA_IMG) -f docker/nfs-ganesha.Dockerfile docker/

.PHONY: kind-load-nfs-ganesha
kind-load-nfs-ganesha: nfs-ganesha-build ## Build and load the NFS-Ganesha image into Kind.
	$(KIND) load docker-image $(NFS_GANESHA_IMG) --name $(KIND_CLUSTER_NAME)

.PHONY: kind-test
kind-test: kind-create kind-deploy ## Create Kind cluster and deploy the operator end-to-end.

.PHONY: kind-sample
kind-sample: ## Apply the SaunaFSCluster sample CR to the Kind cluster.
	$(KIND_KUBECTL) apply -f config/samples/saunafs_v1alpha1_saunafscluster.yaml

.PHONY: kind-reset
kind-reset: ## FULL RESET: delete cluster+data, rebuild operator image and redeploy.
	@echo "==> Deleting Kind cluster (if exists)..."
	-$(KIND) delete cluster --name $(KIND_CLUSTER_NAME)
	@echo "==> Wiping data directories (sudo required for root-owned chunk files)..."
	sudo rm -rf $(KIND_DATA_DIR)
	@echo "==> Recreating cluster and host dirs..."
	$(MAKE) kind-create
	@echo "==> Creating ghcr.io pull secret..."
	$(MAKE) kind-create-pull-secret
	@echo "==> Building controller image..."
	$(MAKE) docker-build
	@echo "==> Loading controller image into Kind..."
	$(KIND) load docker-image $(KIND_IMG) --name $(KIND_CLUSTER_NAME)
	@echo "==> Pulling and loading kube-rbac-proxy..."
	docker pull $(KUBE_RBAC_PROXY_IMG)
	$(KIND) load docker-image $(KUBE_RBAC_PROXY_IMG) --name $(KIND_CLUSTER_NAME)
	@echo "==> Pulling and loading busybox (NFS initContainer)..."
	docker pull busybox:1.36
	$(KIND) load docker-image busybox:1.36 --name $(KIND_CLUSTER_NAME)
	@echo "==> Deploying operator (CRDs + controller)..."
	$(MAKE) kind-install
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(KIND_IMG)
	$(KUSTOMIZE) build config/default | $(KIND_KUBECTL) apply -f -
	@echo "==> Waiting for controller to be ready..."
	$(KIND_KUBECTL) rollout status deployment/saunafs-operator-controller-manager \
	    -n saunafs-operator-system --timeout=120s
	@echo "==> Deploying sample SaunaFSCluster CR..."
	$(MAKE) kind-sample
	@echo "==> Done. Cluster ready."


.PHONY: kind-build-and-deployment
kind-build-and-deployment: ## Build operator image and deploy to an existing Kind cluster.
	@echo "==> Building controller image..."
	$(MAKE) docker-build
	@echo "==> Loading controller image into Kind..."
	$(KIND) load docker-image $(KIND_IMG) --name $(KIND_CLUSTER_NAME)
	@echo "==> Pulling and loading kube-rbac-proxy..."
	docker pull $(KUBE_RBAC_PROXY_IMG)
	$(KIND) load docker-image $(KUBE_RBAC_PROXY_IMG) --name $(KIND_CLUSTER_NAME)
	@echo "==> Pulling and loading busybox (NFS initContainer)..."
	docker pull busybox:1.36
	$(KIND) load docker-image busybox:1.36 --name $(KIND_CLUSTER_NAME)
	@echo "==> Deploying operator (CRDs + controller)..."
	$(MAKE) kind-install
	cd config/manager && $(KUSTOMIZE) edit set image controller=$(KIND_IMG)
	$(KUSTOMIZE) build config/default | $(KIND_KUBECTL) apply -f -
	@echo "==> Waiting for controller to be ready..."
	$(KIND_KUBECTL) rollout status deployment/saunafs-operator-controller-manager \
	    -n saunafs-operator-system --timeout=120s
	@echo "==> Deploying sample SaunaFSCluster CR..."
	$(MAKE) kind-sample
	@echo "==> Deployment Done. App ready."

##@ Deployment

ifndef ignore-not-found
  ignore-not-found = false
endif

.PHONY: install
install: manifests kustomize ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests kustomize ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

.PHONY: deploy
deploy: manifests kustomize ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: kustomize ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=$(ignore-not-found) -f -

##@ Dependencies

## Location to install dependencies to
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

## Tool Binaries
KUBECTL ?= kubectl
KIND    ?= kind
KUSTOMIZE ?= $(LOCALBIN)/kustomize-$(KUSTOMIZE_VERSION)
CONTROLLER_GEN ?= $(LOCALBIN)/controller-gen-$(CONTROLLER_TOOLS_VERSION)
ENVTEST ?= $(LOCALBIN)/setup-envtest-$(ENVTEST_VERSION)
GOLANGCI_LINT = $(LOCALBIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)

## Tool Versions
KUSTOMIZE_VERSION ?= v5.3.0
CONTROLLER_TOOLS_VERSION ?= v0.14.0
ENVTEST_VERSION ?= latest
GOLANGCI_LINT_VERSION ?= v1.54.2

## Kind
KIND_CLUSTER_NAME      ?= saunafs-operator
KIND_CONFIG            ?= scripts/kind-config.yaml
KIND_IMG               ?= $(IMG)
# Root directory on the real host that is bind-mounted into Kind nodes as /mnt/hdd00X
KIND_DATA_DIR          ?= /tmp/saunafs-kind
# kube-rbac-proxy: gcr.io/kubebuilder is defunct; use the upstream quay.io registry
KUBE_RBAC_PROXY_IMG   ?= quay.io/brancz/kube-rbac-proxy:v0.18.0

# kubectl shorthand pre-configured for the Kind cluster context
KIND_KUBECTL   = $(KUBECTL) --context kind-$(KIND_CLUSTER_NAME)

## ghcr.io pull secret
# GitHub PAT with read:packages scope — pass on the command line or export in your shell:
#   export GHCR_PAT=ghp_xxxx
#   make kind-create-pull-secret
GHCR_PAT           ?=
GHCR_USER          ?= henres
PULL_SECRET_NAME   ?= ghcr-pull-secret
# Namespaces that need the pull secret (operator system + workload namespace)
PULL_SECRET_NAMESPACES ?= default saunafs-operator-system

.PHONY: kustomize
kustomize: $(KUSTOMIZE) ## Download kustomize locally if necessary.
$(KUSTOMIZE): $(LOCALBIN)
	$(call go-install-tool,$(KUSTOMIZE),sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VERSION))

.PHONY: controller-gen
controller-gen: $(CONTROLLER_GEN) ## Download controller-gen locally if necessary.
$(CONTROLLER_GEN): $(LOCALBIN)
	$(call go-install-tool,$(CONTROLLER_GEN),sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_TOOLS_VERSION))

.PHONY: envtest
envtest: $(ENVTEST) ## Download setup-envtest locally if necessary.
$(ENVTEST): $(LOCALBIN)
	$(call go-install-tool,$(ENVTEST),sigs.k8s.io/controller-runtime/tools/setup-envtest,$(ENVTEST_VERSION))

.PHONY: golangci-lint
golangci-lint: $(GOLANGCI_LINT) ## Download golangci-lint locally if necessary.
$(GOLANGCI_LINT): $(LOCALBIN)
	$(call go-install-tool,$(GOLANGCI_LINT),github.com/golangci/golangci-lint/cmd/golangci-lint,${GOLANGCI_LINT_VERSION})

# go-install-tool will 'go install' any package with custom target and name of binary, if it doesn't exist
# $1 - target path with name of binary (ideally with version)
# $2 - package url which can be installed
# $3 - specific version of package
define go-install-tool
@[ -f $(1) ] || { \
set -e; \
package=$(2)@$(3) ;\
echo "Downloading $${package}" ;\
GOBIN=$(LOCALBIN) go install $${package} ;\
mv "$$(echo "$(1)" | sed "s/-$(3)$$//")" $(1) ;\
}
endef
