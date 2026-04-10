#!/usr/bin/env bash
# test/loop-disk-test.sh
#
# Creates loop devices on the Kind nodes to simulate bare disks, then verifies
# the full localdisk-operator lifecycle:
#
#   1. Agent detects loop device → LocalDisk created (state=Empty)
#   2. User sets spec.format=true → disk formatted with XFS (state=Ready)
#   3. Controller creates PersistentVolume
#   4. Cleanup: umount, losetup detach, PV deleted
#
# Prerequisites:
#   - Kind cluster running (KIND_CLUSTER, default: localdisk)
#   - localdisk-operator deployed (make deploy / make kind-load + kubectl apply)
#   - kubectl in PATH
#
# Usage:
#   ./test/loop-disk-test.sh [--cleanup-only]
#
set -euo pipefail

KIND_CLUSTER="${KIND_CLUSTER:-localdisk}"
KUBECONTEXT="${KUBECONTEXT:-kind-${KIND_CLUSTER}}"
export KUBECONTEXT
NAMESPACE="${NAMESPACE:-localdisk-operator-system}"
# Size of each fake disk image (sparse file, barely uses real space).
DISK_SIZE_MB="${DISK_SIZE_MB:-512}"
# Kind worker node to use (defaults to first worker).
NODE="${NODE:-}"

# Wrap kubectl so every call uses the right context.
kubectl() { command kubectl --context "${KUBECONTEXT}" "$@"; }

# ── Helpers ───────────────────────────────────────────────────────────────────

log()  { echo "[localdisk-test] $*"; }
info() { log "INFO  $*"; }
ok()   { log "OK    $*"; }
fail() { log "FAIL  $*"; exit 1; }

wait_for() {
  local description="$1" timeout="$2"
  shift 2
  local deadline=$(( $(date +%s) + timeout ))
  while ! "$@" &>/dev/null; do
    if (( $(date +%s) > deadline )); then
      fail "Timed out waiting for: $description"
    fi
    sleep 2
  done
  ok "$description"
}

kubectl_node_exec() {
  # Run a command inside the Kind node container (docker exec).
  docker exec "${NODE_CONTAINER}" bash -c "$1"
}

# ── Setup ─────────────────────────────────────────────────────────────────────

if [[ "${1:-}" == "--cleanup-only" ]]; then
  CLEANUP_ONLY=true
else
  CLEANUP_ONLY=false
fi

# Resolve the Kind node container name.
if [[ -z "${NODE}" ]]; then
  NODE=$(kubectl get nodes --no-headers \
    -l '!node-role.kubernetes.io/control-plane' \
    -o jsonpath='{.items[0].metadata.name}')
fi
NODE_CONTAINER="${KIND_CLUSTER}-${NODE}"
# Kind node containers are named <cluster>-<node> but the worker node name
# may include the cluster prefix already. Try both formats.
if ! docker inspect "${NODE_CONTAINER}" &>/dev/null; then
  NODE_CONTAINER="${NODE}"
fi
info "Using Kind node container: ${NODE_CONTAINER}"

LOOP_IMG="/tmp/localdisk-test-${NODE}.img"
LOOP_DEV=""

cleanup() {
  info "Cleaning up..."
  # Unmount and detach loop device inside the node.
  kubectl_node_exec "
    LOOP=\$(losetup -j /host-tmp/localdisk-test-${NODE}.img 2>/dev/null | cut -d: -f1 || true)
    if [ -n \"\$LOOP\" ]; then
      umount \"\$LOOP\" 2>/dev/null || true
      losetup -d \"\$LOOP\" 2>/dev/null || true
      echo Detached \$LOOP
    fi
  " || true

  # Delete the disk image from inside the container.
  kubectl_node_exec "rm -f /host-tmp/localdisk-test-${NODE}.img" || true

  # Delete the LocalDisk CR and PV.
  kubectl delete localdisk -l localdisk-operator.io/node="${NODE}" \
    --ignore-not-found 2>/dev/null || true
  kubectl delete pv -l localdisk-operator.io/node="${NODE}" \
    --ignore-not-found 2>/dev/null || true

  info "Cleanup done."
}
trap cleanup EXIT

if $CLEANUP_ONLY; then
  cleanup
  trap - EXIT
  exit 0
fi

# ── Step 1: create a loop device on the Kind node ─────────────────────────────

info "Creating ${DISK_SIZE_MB}MiB sparse disk image on node ${NODE}..."

# Create the sparse disk image inside the node container.
docker exec "${NODE_CONTAINER}" mkdir -p /host-tmp
docker exec "${NODE_CONTAINER}" \
  dd if=/dev/zero of=/host-tmp/localdisk-test-${NODE}.img \
     bs=1M count=0 seek=${DISK_SIZE_MB} 2>/dev/null

# losetup --find may fail when all pre-existing /dev/loopN nodes are occupied
# (common on hosts with many snaps). Fall back to creating a high-numbered
# loop device manually.
LOOP_DEV=$(kubectl_node_exec \
  "losetup --find --show /host-tmp/localdisk-test-${NODE}.img 2>/dev/null \
   || { \
     MINOR=\$(( \$(ls /dev/loop* 2>/dev/null | sed 's|/dev/loop||' | sort -n | tail -1) + 1 )); \
     mknod /dev/loop\${MINOR} b 7 \${MINOR} 2>/dev/null || true; \
     losetup /dev/loop\${MINOR} /host-tmp/localdisk-test-${NODE}.img && echo /dev/loop\${MINOR}; \
   }")
info "Loop device created: ${LOOP_DEV} on node ${NODE}"

# ── Step 2: wait for LocalDisk CR to appear ───────────────────────────────────

LOOP_NAME=$(basename "${LOOP_DEV}")   # e.g. loop3
CR_NAME="${LOOP_NAME}-${NODE}"

info "Waiting for LocalDisk CR '${CR_NAME}' to appear (up to 120s)..."
wait_for "LocalDisk CR created" 120 \
  kubectl get localdisk "${CR_NAME}" -o name

STATE=$(kubectl get localdisk "${CR_NAME}" \
  -o jsonpath='{.status.state}')
info "LocalDisk state: ${STATE}"
[[ "${STATE}" == "Empty" ]] || fail "Expected state=Empty, got ${STATE}"

# ── Step 3: approve formatting ────────────────────────────────────────────────

info "Setting spec.format=true on '${CR_NAME}'..."
kubectl patch localdisk "${CR_NAME}" \
  --type merge -p '{"spec":{"format":true}}'

info "Waiting for state=Ready (up to 120s)..."
wait_for "LocalDisk state=Ready" 120 \
  bash -c "kubectl --context \${KUBECONTEXT} get localdisk ${CR_NAME} \
    -o jsonpath='{.status.state}' | grep -q Ready"

UUID=$(kubectl get localdisk "${CR_NAME}" \
  -o jsonpath='{.status.uuid}')
info "Disk formatted, UUID: ${UUID}"

# ── Step 4: verify PersistentVolume ──────────────────────────────────────────

info "Waiting for PersistentVolume '${UUID}' to appear (up to 60s)..."
wait_for "PersistentVolume created" 60 \
  kubectl get pv "${UUID}" -o name

PV_STATUS=$(kubectl get pv "${UUID}" \
  -o jsonpath='{.status.phase}')
info "PV status: ${PV_STATUS}"
[[ "${PV_STATUS}" == "Available" ]] || fail "Expected PV phase=Available, got ${PV_STATUS}"

# ── All good ─────────────────────────────────────────────────────────────────

ok "All checks passed!"
info "LocalDisk: $(kubectl get localdisk ${CR_NAME})"
info "PV:        $(kubectl get pv ${UUID})"

info "Run with --cleanup-only to remove test resources."
# Prevent auto-cleanup so you can inspect the results.
trap - EXIT
