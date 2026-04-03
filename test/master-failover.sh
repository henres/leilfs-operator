#!/usr/bin/env bash
# =============================================================================
# test/master-failover.sh — SaunaFS HA master failover scenario
#
# Simulates a master failure and validates that the shadow automatically
# acquires the Kubernetes Lease and promotes itself to master without any
# manual intervention.
#
# Scenario:
#   1. Pre-flight: verify cluster is healthy, HA Lease exists and has a holder.
#   2. Write test data into the SaunaFS filesystem via the NFS gateway.
#   3. Simulate master failure: cordon the active master pod's node and delete
#      the pod so the StatefulSet cannot reschedule it immediately.
#   4. Wait for the shadow to detect the expired Lease and acquire it
#      (~30 s max).  Verify the Lease holder changes.
#   5. Uncordon the node so the former master can come back as shadow.
#   6. Verify the new master pod started with PERSONALITY=master.
#   7. Verify SaunaFSCluster status.activeMaster is updated.
#   8. Verify the test data written in step 2 is still readable.
#   9. Verify the former master restarted as shadow.
#
# Prerequisites:
#   - Kind cluster "saunafs-operator" running with a deployed SaunaFSCluster
#     that has spec.shadow set (HA mode).
#   - kubectl context kind-saunafs-operator must be accessible.
#   - docker CLI available (used to access NFS via Kind nodes).
#
# Usage:
#   bash test/master-failover.sh
# =============================================================================

set -euo pipefail

KUBE="kubectl --context kind-saunafs-operator"
NS="default"
CLUSTER="saunafscluster-sample"
MASTER_STS="${CLUSTER}-master"
LEASE_NAME="${CLUSTER}-master-ha"
METALOGGER_STS="${CLUSTER}-metalogger"
NFS_DEPLOY="${CLUSTER}-nfs"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

pass() { echo -e "${GREEN}[PASS]${NC} $*"; }
fail() { echo -e "${RED}[FAIL]${NC} $*"; exit 1; }
step() { echo -e "\n${YELLOW}==> $*${NC}"; }

# ---------------------------------------------------------------------------
# Helper: exec into a pod
# ---------------------------------------------------------------------------
kexec() {
    local pod=$1; shift
    $KUBE exec -n "$NS" "$pod" -- "$@"
}

# ---------------------------------------------------------------------------
# Helper: wait for StatefulSet to have all replicas ready
# ---------------------------------------------------------------------------
wait_sts() {
    local name=$1 timeout=${2:-120}
    echo "    Waiting for statefulset/${name} to be ready (timeout ${timeout}s)..."
    $KUBE rollout status statefulset/"$name" -n "$NS" --timeout="${timeout}s"
}

# ---------------------------------------------------------------------------
# Helper: get the current Lease holder (pod name)
# ---------------------------------------------------------------------------
lease_holder() {
    $KUBE get lease "$LEASE_NAME" -n "$NS" \
        -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Helper: get the current Lease renewTime as epoch seconds
# ---------------------------------------------------------------------------
lease_renew_epoch() {
    local rt
    rt=$($KUBE get lease "$LEASE_NAME" -n "$NS" \
        -o jsonpath='{.spec.renewTime}' 2>/dev/null || echo "")
    if [ -z "$rt" ]; then echo 0; return; fi
    date -u -d "$rt" +%s 2>/dev/null || echo 0
}

# ---------------------------------------------------------------------------
# Helper: get NFS service ClusterIP
# ---------------------------------------------------------------------------
nfs_svc_ip() {
    $KUBE get svc "${CLUSTER}-nfs" -n "$NS" -o jsonpath='{.spec.clusterIP}' 2>/dev/null
}

# =============================================================================
step "1. Pre-flight: verify cluster is healthy and HA Lease has a holder"
# =============================================================================

HOLDER=$(lease_holder)
[ -n "$HOLDER" ] || fail "HA Lease '${LEASE_NAME}' has no holder — is HA enabled and cluster healthy?"
pass "Active master (Lease holder): $HOLDER"

ML_READY=$($KUBE get sts "$METALOGGER_STS" -n "$NS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo 0)
[ "$ML_READY" -ge 1 ] || fail "No metalogger replicas ready (got ${ML_READY})"
pass "Metalogger replicas ready: $ML_READY"

NFS_SVC_IP=$(nfs_svc_ip)
[ -n "$NFS_SVC_IP" ] || fail "NFS service not found"
pass "NFS service ClusterIP: $NFS_SVC_IP"

# Determine which node the active master pod is on.
ACTIVE_POD="$HOLDER"
ACTIVE_NODE=$($KUBE get pod "$ACTIVE_POD" -n "$NS" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
[ -n "$ACTIVE_NODE" ] || fail "Could not determine node for pod ${ACTIVE_POD}"
pass "Active master pod '${ACTIVE_POD}' is on node '${ACTIVE_NODE}'"

# =============================================================================
step "2. Write test data into SaunaFS via NFS"
# =============================================================================
TEST_FILE="failover-test-$(date +%s).txt"
TEST_CONTENT="master-failover-sentinel-$(date -u +%Y%m%dT%H%M%SZ)"

echo "    Mounting NFS inside Kind control-plane node and writing test file..."
docker exec saunafs-operator-control-plane bash -c "
    apt-get install -y -q nfs-common 2>/dev/null | tail -1
    mkdir -p /mnt/saunafs-test
    mount -t nfs -o vers=3,nolock ${NFS_SVC_IP}:/ /mnt/saunafs-test 2>/dev/null || \
        mount -t nfs4 ${NFS_SVC_IP}:/ /mnt/saunafs-test
    echo '${TEST_CONTENT}' > /mnt/saunafs-test/${TEST_FILE}
    sync
    echo 'Write OK: ' \$(cat /mnt/saunafs-test/${TEST_FILE})
    umount /mnt/saunafs-test
"
pass "Test file written: ${TEST_FILE} = '${TEST_CONTENT}'"

# =============================================================================
step "3. Simulate master failure (cordon node + delete pod)"
# =============================================================================
echo "    Cordoning node ${ACTIVE_NODE} to prevent pod rescheduling..."
$KUBE cordon "$ACTIVE_NODE"

echo "    Deleting active master pod ${ACTIVE_POD}..."
$KUBE delete pod "$ACTIVE_POD" -n "$NS" --grace-period=0 --force 2>/dev/null || true
pass "Master pod deleted; node cordoned — Lease will expire in ~30s"

# =============================================================================
step "4. Wait for shadow to acquire Lease (max 60 s)"
# =============================================================================
echo "    Polling Lease holder every 3s..."
NEW_HOLDER=""
for i in $(seq 1 20); do
    sleep 3
    NEW_HOLDER=$(lease_holder)
    echo "      t+$((i*3))s: holder=${NEW_HOLDER}"
    if [ -n "$NEW_HOLDER" ] && [ "$NEW_HOLDER" != "$ACTIVE_POD" ]; then
        break
    fi
done

[ -n "$NEW_HOLDER" ] && [ "$NEW_HOLDER" != "$ACTIVE_POD" ] || \
    fail "Shadow did not acquire Lease within 60s (current holder='${NEW_HOLDER}')"
pass "Lease acquired by: ${NEW_HOLDER}"

# Wait for the new master pod to be Running/Ready (it deleted itself and restarted).
echo "    Waiting for new master pod to be Running..."
for i in $(seq 1 20); do
    PHASE=$($KUBE get pod "$NEW_HOLDER" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
    echo "      t+$((i*3))s: ${NEW_HOLDER} phase=${PHASE}"
    [ "$PHASE" = "Running" ] && break
    sleep 3
done
PHASE=$($KUBE get pod "$NEW_HOLDER" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
[ "$PHASE" = "Running" ] || fail "New master pod ${NEW_HOLDER} is not Running (phase=${PHASE})"
pass "New master pod Running"

# =============================================================================
step "5. Uncordon node to allow former master to come back as shadow"
# =============================================================================
$KUBE uncordon "$ACTIVE_NODE"
pass "Node ${ACTIVE_NODE} uncordoned"

# =============================================================================
step "6. Verify new master started with PERSONALITY=master"
# =============================================================================
INIT_LOG=$($KUBE logs "$NEW_HOLDER" -n "$NS" -c init-config 2>/dev/null || echo "")
echo "$INIT_LOG" | grep -q "I am the Lease holder" || \
    fail "init-config did not log 'I am the Lease holder' for ${NEW_HOLDER}"
pass "init-config confirmed PERSONALITY=master for ${NEW_HOLDER}"

# =============================================================================
step "7. Verify SaunaFSCluster status.activeMaster is updated"
# =============================================================================
# Give the operator a reconcile cycle (≤5s).
sleep 8
STATUS_MASTER=$($KUBE get saunafscluster "$CLUSTER" -n "$NS" \
    -o jsonpath='{.status.activeMaster}' 2>/dev/null || echo "")
[ "$STATUS_MASTER" = "$NEW_HOLDER" ] || \
    fail "status.activeMaster='${STATUS_MASTER}', expected '${NEW_HOLDER}'"
pass "status.activeMaster=${STATUS_MASTER}"

# =============================================================================
step "8. Verify test data is still readable after failover"
# =============================================================================
# Restart NFS so ganesha reconnects to the new master.
echo "    Restarting NFS deployment to reconnect to new master..."
$KUBE rollout restart deployment "$NFS_DEPLOY" -n "$NS"
$KUBE rollout status deployment "$NFS_DEPLOY" -n "$NS" --timeout=90s

echo "    Re-mounting NFS and reading test file..."
FOUND=$(docker exec saunafs-operator-control-plane bash -c "
    mkdir -p /mnt/saunafs-test
    mount -t nfs -o vers=3,nolock ${NFS_SVC_IP}:/ /mnt/saunafs-test 2>/dev/null || \
        mount -t nfs4 ${NFS_SVC_IP}:/ /mnt/saunafs-test
    cat /mnt/saunafs-test/${TEST_FILE} 2>/dev/null || echo '__NOT_FOUND__'
    umount /mnt/saunafs-test
")
echo "    File content: '$FOUND'"
[ "$FOUND" = "$TEST_CONTENT" ] || \
    fail "Test file content mismatch. Expected='${TEST_CONTENT}' Got='${FOUND}'"
pass "Test file content matches — data survived master failover"

# =============================================================================
step "9. Wait for former master to come back as shadow"
# =============================================================================
echo "    Waiting for ${ACTIVE_POD} to restart (max 60s)..."
for i in $(seq 1 20); do
    PHASE=$($KUBE get pod "$ACTIVE_POD" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || echo "Pending")
    echo "      t+$((i*3))s: ${ACTIVE_POD} phase=${PHASE}"
    [ "$PHASE" = "Running" ] && break
    sleep 3
done
PHASE=$($KUBE get pod "$ACTIVE_POD" -n "$NS" -o jsonpath='{.status.phase}' 2>/dev/null || echo "")
[ "$PHASE" = "Running" ] || fail "Former master ${ACTIVE_POD} did not restart (phase=${PHASE})"

INIT_LOG_OLD=$($KUBE logs "$ACTIVE_POD" -n "$NS" -c init-config 2>/dev/null || echo "")
echo "$INIT_LOG_OLD" | grep -q "starting as shadow" || \
    fail "Former master ${ACTIVE_POD} did not start as shadow"
pass "Former master ${ACTIVE_POD} restarted as shadow"

# =============================================================================
echo -e "\n${GREEN}=====================================================${NC}"
echo -e "${GREEN}  Master HA failover scenario: ALL STEPS PASSED${NC}"
echo -e "${GREEN}=====================================================${NC}"
