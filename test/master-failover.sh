#!/usr/bin/env bash
# =============================================================================
# test/master-failover.sh — SaunaFS master failover scenario
#
# Simulates a master failure and validates that the cluster can be recovered
# using metadata preserved by a metalogger replica.
#
# Scenario:
#   1. Write test data into the SaunaFS filesystem via the NFS gateway.
#   2. Kill the master (delete its pod, simulating a crash).
#   3. Rebuild metadata.sfs from the metalogger journal (sfsmetarestore).
#   4. Replace the master PVC data with the metalogger-rebuilt metadata.
#   5. Restart the master; verify it comes back Ready.
#   6. Verify the test data is still readable.
#
# Prerequisites:
#   - Kind cluster "saunafs-operator" running with a deployed SaunaFSCluster.
#   - kubectl context kind-saunafs-operator must be accessible.
#   - NFS mounted locally OR saunafs-mount available; script uses kubectl exec
#     into the NFS pod as a proxy to avoid requiring a local NFS mount.
#
# Usage:
#   bash test/master-failover.sh
# =============================================================================

set -euo pipefail

KUBE="kubectl --context kind-saunafs-operator"
NS="default"
CLUSTER="saunafscluster-sample"
MASTER_DEPLOY="${CLUSTER}-master"
METALOGGER_STS="${CLUSTER}-metalogger"
METALOGGER_POD="${METALOGGER_STS}-0"
NFS_DEPLOY="${CLUSTER}-nfs"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}[PASS]${NC} $*"; }
fail() { echo -e "${RED}[FAIL]${NC} $*"; exit 1; }
step() { echo -e "\n${YELLOW}==> $*${NC}"; }

# ---------------------------------------------------------------------------
# Helper: exec into a pod (defaulting to first container)
# ---------------------------------------------------------------------------
kexec() {
    local pod=$1; shift
    $KUBE exec -n "$NS" "$pod" -- "$@"
}

# ---------------------------------------------------------------------------
# Helper: wait for a deployment to have all replicas ready
# ---------------------------------------------------------------------------
wait_deploy() {
    local name=$1 timeout=${2:-120}
    echo "    Waiting for deployment/${name} to be ready (timeout ${timeout}s)..."
    $KUBE rollout status deployment/"$name" -n "$NS" --timeout="${timeout}s"
}

# ---------------------------------------------------------------------------
# Helper: get the current master pod name
# ---------------------------------------------------------------------------
master_pod() {
    $KUBE get pods -n "$NS" -l "app.kubernetes.io/name=saunafs-master" \
        --field-selector=status.phase=Running \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# ---------------------------------------------------------------------------
# Helper: get the current NFS pod name
# ---------------------------------------------------------------------------
nfs_pod() {
    $KUBE get pods -n "$NS" -l "app.kubernetes.io/name=saunafs-nfs" \
        --field-selector=status.phase=Running \
        -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

# =============================================================================
step "1. Pre-flight: verify cluster is healthy"
# =============================================================================

MPOD=$(master_pod)
[ -n "$MPOD" ] || fail "No running master pod found"
pass "Master pod: $MPOD"

ML_READY=$($KUBE get sts "$METALOGGER_STS" -n "$NS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
[ "$ML_READY" -ge 1 ] || fail "No metalogger replicas ready (got ${ML_READY})"
pass "Metalogger replicas ready: $ML_READY"

NPOD=$(nfs_pod)
[ -n "$NPOD" ] || fail "No running NFS pod found"
pass "NFS pod: $NPOD"

# =============================================================================
step "2. Write test data into SaunaFS via NFS pod"
# =============================================================================
# The NFS pod has the SaunaFS FSAL but no local FUSE mount; we use the master
# container directly (it has /var/lib/saunafs but not the FS client). Instead,
# we exec into a chunkserver which also doesn't have a client. Best approach:
# exec into the master pod (it has bash + full OS) and use sfsmaster to mount.
# Simplest alternative: write a sentinel file via the NFS ganesha container
# which mounts SaunaFS via the FSAL internally. We trigger a file creation via
# the ganesha control socket, or better — mount NFS inside the Kind node.

# Practical approach: mount NFS inside one of the Kind worker nodes using
# docker exec (Kind nodes are Docker containers with full OS).
WORKER_NODE="saunafs-operator-worker"
TEST_FILE="failover-test-$(date +%s).txt"
TEST_CONTENT="master-failover-sentinel-$(date -u +%Y%m%dT%H%M%SZ)"

NFS_SVC_IP=$($KUBE get svc "${CLUSTER}-nfs" -n "$NS" -o jsonpath='{.spec.clusterIP}')
echo "    NFS service ClusterIP: $NFS_SVC_IP"

# Mount NFS inside the control-plane node (has nfs-common) and write the file.
echo "    Mounting NFS inside Kind control-plane node..."
docker exec saunafs-operator-control-plane bash -c "
    apt-get install -y -q nfs-common 2>/dev/null | tail -1 &&
    mkdir -p /mnt/saunafs-test &&
    mount -t nfs -o vers=3,nolock ${NFS_SVC_IP}:/ /mnt/saunafs-test 2>/dev/null || \
        mount -t nfs4 ${NFS_SVC_IP}:/ /mnt/saunafs-test &&
    echo '${TEST_CONTENT}' > /mnt/saunafs-test/${TEST_FILE} &&
    sync &&
    echo 'Write OK: ' \$(cat /mnt/saunafs-test/${TEST_FILE}) &&
    umount /mnt/saunafs-test
"
pass "Test file written: ${TEST_FILE} = '${TEST_CONTENT}'"

# =============================================================================
step "3. Force-kill the master pod (simulate crash)"
# =============================================================================
echo "    Deleting master pod: $MPOD"
$KUBE delete pod "$MPOD" -n "$NS" --grace-period=0 --force 2>/dev/null || true
pass "Master pod deleted"

# Wait for Deployment controller to detect the deletion and mark unavailable.
sleep 3

# Confirm master is down (new pod not yet Running).
NEW_MPOD=$(master_pod 2>/dev/null || true)
if [ -n "$NEW_MPOD" ] && [ "$NEW_MPOD" != "$MPOD" ]; then
    echo "    Deployment already restarted a new pod: $NEW_MPOD — scaling down to keep it down"
    $KUBE scale deployment "$MASTER_DEPLOY" -n "$NS" --replicas=0
    sleep 5
else
    echo "    Master is down (no running pod)"
fi
pass "Master confirmed down"

# =============================================================================
step "4+5. Rebuild metadata.sfs from metalogger journal and write it into the master PVC"
# =============================================================================
# The metalogger image does NOT include sfsmetarestore — only the master image
# does. We therefore spin up a temporary pod (RESTORE_POD) using the master
# image that mounts:
#   - the metalogger-0 PVC (read-only) at /ml
#   - the master PVC (read-write) at /data
# and runs sfsmetarestore directly, writing metadata.sfs into the master PVC.
# This combines the old steps 4 and 5 into a single atomic operation.

MASTER_PVC="${CLUSTER}-master-metadata"
ML_PVC="metalogger-data-${METALOGGER_STS}-0"
RESTORE_POD="master-failover-restore"
MASTER_IMAGE=$(${KUBE} get deployment "${MASTER_DEPLOY}" -n "${NS}" \
    -o jsonpath='{.spec.template.spec.containers[0].image}')
echo "    Master image: ${MASTER_IMAGE}"
echo "    Master PVC:   ${MASTER_PVC}"
echo "    Metalogger PVC: ${ML_PVC}"

# Clean up any leftover restore pod.
$KUBE delete pod "$RESTORE_POD" -n "$NS" --ignore-not-found --grace-period=0 2>/dev/null || true
sleep 2

echo "    Listing metalogger data dir (via kubectl exec):"
kexec "$METALOGGER_POD" ls -lh /var/lib/saunafs/

# Build the sfsmetarestore invocation.
# changelog_ml.sfs.* files are sorted numerically by the last extension component.
RESTORE_CMD='
set -e
ML=/ml
OUT=/data
WORK=/tmp/sfsrestore

echo "[restore] metalogger data:"
ls -lh $ML/

# sfsmetarestore tries to open a lock file in the same directory as the input
# metadata file, so we must copy inputs to a writable temp directory first.
mkdir -p $WORK
cp $ML/metadata_ml.sfs $WORK/metadata_ml.sfs

CHANGELOGS_ML=$(ls $ML/changelog_ml.sfs $ML/changelog_ml.sfs.* 2>/dev/null | sort -t. -k3 -n || true)
CHANGELOGS_WORK=""
for f in $CHANGELOGS_ML; do
    base=$(basename $f)
    cp $f $WORK/$base
    CHANGELOGS_WORK="$CHANGELOGS_WORK $WORK/$base"
done
echo "[restore] changelogs copied: ${CHANGELOGS_WORK:-<none>}"

if [ -z "$CHANGELOGS_WORK" ]; then
    echo "[restore] No changelogs — copying snapshot as-is."
    cp $WORK/metadata_ml.sfs $OUT/metadata.sfs
else
    echo "[restore] Running sfsmetarestore..."
    sfsmetarestore -m $WORK/metadata_ml.sfs -o $OUT/metadata.sfs $CHANGELOGS_WORK
fi

echo "[restore] Removing stale lock file (if any)..."
rm -f $OUT/metadata.sfs.lock

echo "[restore] Result:"
ls -lh $OUT/metadata.sfs
echo "[restore] Done."
'

echo "    Spawning restore pod (master image + both PVCs)..."
$KUBE run "$RESTORE_POD" -n "$NS" \
    --image="${MASTER_IMAGE}" \
    --restart=Never \
    --overrides="{
        \"spec\": {
            \"imagePullSecrets\": [{\"name\": \"ghcr-pull-secret\"}],
            \"volumes\": [
                {\"name\": \"master-data\", \"persistentVolumeClaim\": {\"claimName\": \"${MASTER_PVC}\"}},
                {\"name\": \"ml-data\",     \"persistentVolumeClaim\": {\"claimName\": \"${ML_PVC}\", \"readOnly\": true}}
            ],
            \"containers\": [{
                \"name\": \"restore\",
                \"image\": \"${MASTER_IMAGE}\",
                \"command\": [\"sh\", \"-c\", $(echo "$RESTORE_CMD" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')],
                \"volumeMounts\": [
                    {\"name\": \"master-data\", \"mountPath\": \"/data\"},
                    {\"name\": \"ml-data\",     \"mountPath\": \"/ml\"}
                ]
            }]
        }
    }"

echo "    Waiting for restore pod to complete (timeout 120s)..."
# The pod runs to completion; poll until phase is Succeeded or Failed.
for i in $(seq 1 60); do
    RESTORE_STATUS=$($KUBE get pod "$RESTORE_POD" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Unknown")
    case "$RESTORE_STATUS" in
        Succeeded|Failed) break ;;
        *) sleep 2 ;;
    esac
done
echo "    Restore pod logs:"
$KUBE logs -n "$NS" "$RESTORE_POD" 2>/dev/null || true

RESTORE_STATUS=$($KUBE get pod "$RESTORE_POD" -n "$NS" -o jsonpath='{.status.phase}')
echo "    Restore pod phase: ${RESTORE_STATUS}"
[ "$RESTORE_STATUS" = "Succeeded" ] || {
    echo "    Restore pod logs:"
    $KUBE logs -n "$NS" "$RESTORE_POD" 2>/dev/null || true
    fail "Restore pod did not succeed (phase=${RESTORE_STATUS})"
}

echo "    Cleaning up restore pod..."
$KUBE delete pod "$RESTORE_POD" -n "$NS" --grace-period=0 2>/dev/null || true

pass "metadata.sfs rebuilt and written to master PVC"

# =============================================================================
step "6. Restart the master and NFS, wait for Ready"
# =============================================================================
echo "    Scaling master deployment back to 1..."
$KUBE scale deployment "$MASTER_DEPLOY" -n "$NS" --replicas=1 2>/dev/null || true

wait_deploy "$MASTER_DEPLOY" 120
NEW_MPOD=$(master_pod)
[ -n "$NEW_MPOD" ] || fail "Master pod not running after restart"
pass "Master restarted: $NEW_MPOD"

echo "    Master logs (last 10 lines):"
$KUBE logs -n "$NS" "$NEW_MPOD" --tail=10 2>/dev/null || true

# NFS-Ganesha (saunafs FSAL) loses its connection to the master when the
# master is killed. It must be restarted after the master is back up to
# re-establish the connection and clear stale file handles.
echo "    Restarting NFS deployment to reconnect ganesha to new master..."
$KUBE rollout restart deployment "$NFS_DEPLOY" -n "$NS"
$KUBE rollout status deployment "$NFS_DEPLOY" -n "$NS" --timeout=90s
pass "NFS deployment restarted"

# =============================================================================
step "7. Verify SaunaFSCluster status is Ready"
# =============================================================================
sleep 10
STATUS=$($KUBE get saunafscluster "$CLUSTER" -n "$NS" \
    -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
[ "$STATUS" = "True" ] || fail "SaunaFSCluster not Ready after failover (status=${STATUS})"
pass "SaunaFSCluster: Ready=True"

# =============================================================================
step "8. Verify test data is still readable after failover"
# =============================================================================
echo "    Re-mounting NFS and checking test file..."
FOUND=$(docker exec saunafs-operator-control-plane bash -c "
    mkdir -p /mnt/saunafs-test &&
    mount -t nfs -o vers=3,nolock ${NFS_SVC_IP}:/ /mnt/saunafs-test 2>/dev/null || \
        mount -t nfs4 ${NFS_SVC_IP}:/ /mnt/saunafs-test &&
    cat /mnt/saunafs-test/${TEST_FILE} 2>/dev/null || echo '__NOT_FOUND__' &&
    umount /mnt/saunafs-test
")

echo "    File content: '$FOUND'"
if [ "$FOUND" = "$TEST_CONTENT" ]; then
    pass "Test file content matches — data survived master failover"
else
    fail "Test file content mismatch. Expected='${TEST_CONTENT}' Got='${FOUND}'"
fi

# =============================================================================
echo -e "\n${GREEN}=====================================================${NC}"
echo -e "${GREEN}  Master failover scenario: ALL STEPS PASSED${NC}"
echo -e "${GREEN}=====================================================${NC}"
