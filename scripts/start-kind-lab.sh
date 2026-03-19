#!/usr/bin/env bash
# start-kind-lab.sh — Create (or restart) the local kind dev cluster and deploy
# the saunafs-operator with a full SaunaFSCluster sample.
#
# Idempotent: safe to run multiple times. Skips steps that are already done.
#
# Usage:
#   bash hack/start-kind-lab.sh           # full setup
#   bash hack/start-kind-lab.sh --skip-build  # skip docker builds (images already built)
set -euo pipefail

SKIP_BUILD=false
for arg in "$@"; do [[ "$arg" == "--skip-build" ]] && SKIP_BUILD=true; done

CLUSTER_NAME=saunafs-operator
REGISTRY_NAME=kind-registry
REGISTRY_PORT=5001

echo "==> [1/7] Checking prerequisites..."
for cmd in kind kubectl docker; do
  command -v "$cmd" >/dev/null || { echo "ERROR: $cmd not found"; exit 1; }
done

echo "==> [2/7] Starting local registry (${REGISTRY_NAME}:${REGISTRY_PORT})..."
if ! docker ps --format '{{.Names}}' | grep -q "^${REGISTRY_NAME}$"; then
  docker run -d --restart=always --name "${REGISTRY_NAME}" \
    -p "127.0.0.1:${REGISTRY_PORT}:5000" registry:2
  echo "    Registry started."
else
  echo "    Registry already running."
fi

echo "==> [3/7] Creating kind cluster (${CLUSTER_NAME})..."
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
  echo "    Cluster already exists."
else
  # Pre-create host disk directories so kind can bind-mount them.
  for w in worker1 worker2 worker3; do
    mkdir -p "/tmp/saunafs-kind/${w}/hdd001"
    mkdir -p "/tmp/saunafs-kind/${w}/hdd002"
  done

  kind create cluster --config hack/kind-config.yaml

  # Connect registry to the kind network so nodes can pull from it.
  docker network connect kind "${REGISTRY_NAME}" 2>/dev/null || true

  # Inform kind nodes about the local registry (ConfigMap convention).
  kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: local-registry-hosting
  namespace: kube-public
data:
  localRegistryHosting.v1: |
    host: "localhost:${REGISTRY_PORT}"
    help: "https://kind.sigs.k8s.io/docs/user/local-registry/"
EOF
fi

echo "==> [4/7] Building SaunaFS images..."
if [[ "$SKIP_BUILD" == "true" ]]; then
  echo "    Skipped (--skip-build)."
else
  bash hack/build-saunafs-images.sh
  docker build -f docker/nfs-ganesha.Dockerfile -t nfs-ganesha:latest . 2>&1 | tail -3
fi

echo "==> [5/7] Pushing images to local registry..."
REGISTRY="localhost:${REGISTRY_PORT}"
for img in saunafs-master saunafs-chunkserver saunafs-client saunafs-cgiserver nfs-ganesha; do
  docker tag "${img}:latest" "${REGISTRY}/${img}:latest"
  docker push "${REGISTRY}/${img}:latest" 2>&1 | tail -1
done

echo "==> [6/7] Building and pushing operator image..."
make docker-build IMG="${REGISTRY}/saunafs-operator:latest" 2>&1 | tail -3
docker push "${REGISTRY}/saunafs-operator:latest" 2>&1 | tail -1

echo "==> [7/7] Deploying operator and SaunaFSCluster..."
make install 2>&1 | tail -2

# Use the registry address reachable from inside kind nodes.
KIND_REGISTRY="kind-registry:5000"
# Patch sample images to use the in-cluster registry address.
PATCHED_IMG="${KIND_REGISTRY}"
make deploy IMG="${KIND_REGISTRY}/saunafs-operator:latest" 2>&1 | tail -3

kubectl wait deployment/saunafs-operator-controller-manager \
  -n saunafs-operator-system --for=condition=Available --timeout=90s

# Allow hostPath volumes in the default namespace.
kubectl label namespace default \
  pod-security.kubernetes.io/enforce=privileged \
  pod-security.kubernetes.io/warn=privileged \
  --overwrite 2>/dev/null || true

# Patch the sample to point images at the in-cluster registry.
sed "s|saunafs-master:latest|${KIND_REGISTRY}/saunafs-master:latest|g;
     s|saunafs-chunkserver:latest|${KIND_REGISTRY}/saunafs-chunkserver:latest|g;
     s|saunafs-cgiserver:latest|${KIND_REGISTRY}/saunafs-cgiserver:latest|g;
     s|nfs-ganesha:latest|${KIND_REGISTRY}/nfs-ganesha:latest|g" \
  config/samples/saunafs_v1alpha1_saunafscluster.yaml \
  | kubectl apply -f -

echo ""
echo "✅ Done! Cluster is up."
echo ""
echo "   Pods:"
kubectl get pods -n default 2>/dev/null || true
echo ""
echo "   SaunaFSCluster status:"
kubectl get saunafscluster -o wide 2>/dev/null || true
echo ""
echo "   Web UI (NodePort):"
kubectl get svc saunafscluster-sample-interface 2>/dev/null \
  | awk 'NR>1 {split($5,p,":"); split(p[2],np,"/"); print "   http://localhost:"np[1]}' || true
