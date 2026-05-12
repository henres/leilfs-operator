---
name: ha-lease-election
description: Complete reference for the leilfs-operator HA master election protocol using Kubernetes Leases, including init-container, sidecar shell scripts, and failover behaviour.
compatibility: opencode
---

## Overview

The HA system elects one active master per LeilfsCluster using a Kubernetes Lease object.
All pods in the master StatefulSet run both the saunafs-master process AND a shell sidecar that
participates in the election.

## Kubernetes objects involved

| Object | Name pattern | Purpose |
|---|---|---|
| Lease | `<cluster>-master-ha` | Holds `holderIdentity` = active pod name |
| StatefulSet | `<cluster>-master` | Runs N pods; all start as candidates |
| ServiceAccount | `<cluster>-master` | Used by all pods in the StatefulSet |
| Role | `<cluster>-master-ha` | leases get/update/patch + pods delete (resourceNames scoped) |
| RoleBinding | `<cluster>-master-ha` | Binds Role to the SA |
| Service (ClusterIP) | `<cluster>-master` | Selector: `leilfs.io/active-master=true` |
| Service (NodePort) | `<cluster>-client-expose` | In HA mode also uses `leilfs.io/active-master=true` |

## Lease parameters

- `leaseDurationSeconds`: 30
- Renewed by the holder every **10 s** via PATCH
- Acquired by a shadow via a CAS PATCH using `resourceVersion`

## Init-container: `init-config`

Runs once per pod start. Reads the Lease via `wget` against the in-cluster API server.

**Logic:**
1. GET the Lease JSON.
2. Extract `holderIdentity` with `sed "s/.*\"holderIdentity\": *\"([^\"]*)\".*/\1/;t;s/.*//"` plus `grep -v '^$' | head -1` to discard empty lines and the `f:holderIdentity` entry in `managedFields`.
3. If `holderIdentity == $POD_NAME` → write `PERSONALITY=master` to `/config/personality`.
4. If `holderIdentity` is non-empty and `!= $POD_NAME` → write `PERSONALITY=shadow` and `MASTER_HOST=<holderIdentity>.<headless-svc>` to `/config/personality`.
5. If `holderIdentity` is empty → write `PERSONALITY=master` (first boot, no holder yet).

The `/config` volume is shared with the main container via an `emptyDir`.

## Sidecar: `ha-sidecar`

Runs as a second container in every pod alongside saunafs-master.

### `json_field()` helper
```sh
json_field() {
  echo "$1" | sed "s/.*\"$2\": *\"([^\"]*)\".*/\1/;t;s/.*//" | grep -v "^$" | head -1
}
```

### `delete_self()` helper
Triggers a pod restart by deleting the pod via the API:
```sh
delete_self() {
  wget -q --method=DELETE \
    --header="Authorization: Bearer $TOKEN" \
    --header="Content-Type: application/json" \
    --no-check-certificate \
    -O /dev/null \
    "https://$KUBERNETES_SERVICE_HOST:$KUBERNETES_SERVICE_PORT/api/v1/namespaces/$NS/pods/$POD_NAME"
  sleep 10 && exit 0
}
```
After deletion, the StatefulSet controller recreates the pod, running init-config again to
re-read the Lease and adopt the correct role.

### Holder loop (PERSONALITY=master)

Every 10 s:
```
PATCH /apis/coordination.k8s.io/v1/namespaces/$NS/leases/$LEASE_NAME
Body: {"spec":{"holderIdentity":"$POD_NAME","renewTime":"<now>","leaseDurationSeconds":30}}
```
If PATCH fails → `delete_self()`.

### Shadow loop (PERSONALITY=shadow)

Every 5 s:
1. GET the Lease.
2. Compute `age = now - renewTime`. If `age < 30` → continue waiting.
3. Lease has expired → attempt CAS PATCH:
```
PATCH with resourceVersion=$RV
Body: {"spec":{"holderIdentity":"$POD_NAME","renewTime":"<now>","leaseDurationSeconds":30}}
```
4. If PATCH succeeds (HTTP 200) → `delete_self()` so the pod restarts as master.
5. If PATCH fails (HTTP 409 conflict) → another shadow won; continue waiting.

## Operator's role (passive observer)

`reconcileMasterHA` in the controller:
- Creates the Lease at bootstrap (empty `holderIdentity`).
- On every reconcile (every 5 s via `RequeueAfter`):
  - Reads Lease, extracts `holderIdentity`.
  - Labels the holder pod with `leilfs.io/active-master=true`; removes label from others.
  - Updates the ClusterIP Service selector to match that label.
  - Updates `status.ActiveMaster` and `status.ReadyShadows`.

The operator does NOT write to the Lease after creation; election is fully peer-to-peer.

## Failover timing

| Phase | Duration |
|---|---|
| Holder crashes / stops renewing | 0 s |
| Shadow detects expiry (poll 5 s + up to 30 s Lease TTL) | ≤ 35 s |
| CAS PATCH + delete_self + pod restart | ~5 s |
| Init-container reads new holderIdentity | ~2 s |
| Total observed failover | ~25 s |

## Key pitfalls

- **Startup grace must NOT block the Lease renewal**: the sidecar accepts a
  `STARTUP_GRACE` (default 90s) to tolerate slow metadata loading by
  sfsmaster. Its only legitimate effect is to suppress the `pgrep
  sfsmaster` health check during that window. **The renewal loop must
  start at t=0.** A previous version did `sleep ${STARTUP_GRACE}` before
  the loop; with `leaseDurationSeconds=30` the active master's Lease
  expired 60s into every pod start, the shadow stole it via CAS, both
  pods then `delete_self()` and entered their own grace, and the cluster
  flapped continuously between master-0 and master-1 every ~60–120s.
  Chunkservers kept reconnecting to a moving target and only 1–4 of N
  were visible to the exporter at any time. Implementation today tracks
  `START_EPOCH` and skips the `pgrep` check for `STARTUP_GRACE` seconds
  while still issuing PATCH every `RENEW_INTERVAL`.
- **sed pattern must handle space after `:`**: Kubernetes API returns `"holderIdentity": "value"` (space), not `"holderIdentity":"value"`.
- **managedFields false-positive**: the `f:holderIdentity` entry in `managedFields` has no quoted value and is safely ignored by the sed pattern, but `grep -v '^$' | head -1` is still needed.
- **wget only**: the saunafs-master image has `wget` v1.21.4 but NOT `curl` or `kubectl`. All API calls use `wget --method=PATCH/DELETE`.
- **No new image**: the sidecar is an inline shell script in the StatefulSet spec (not a separate image).
- **shareProcessNamespace: true** is set on the pod spec but is NOT functionally required (legacy from a previous design); it can be removed in a future cleanup.

## Files

- Controller: `internal/controller/leilfscluster_controller.go`
  - `reconcileMasterHA` ~line 1778
  - `reconcileMasterHARBAC` ~line 1877
  - `reconcileMasterStatefulSet` ~line 1356 (init-container + sidecar scripts)
  - `reconcileExposeService` ~line 791
- ADR: `docs/adr/0001-master-ha-election-via-kubernetes-lease.md`
- Failover test: `test/master-failover.sh`
