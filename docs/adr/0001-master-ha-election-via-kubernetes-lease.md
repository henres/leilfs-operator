# ADR-0001: Master HA Election via Kubernetes Lease

- **Status:** Accepted
- **Date:** 2026-03-30
- **Deciders:** Henri Dauvigny

---

## Context

LeilFS masters operate in two personalities: `master` (active, serves all metadata I/O) and `shadow` (passive replica, syncs from master). Only one master may be active at a time. The operator manages a `StatefulSet` with two replicas: one intended to run as master, the other as shadow.

The operator needs a mechanism to:

1. Elect exactly one pod as master at any given time.
2. Detect master failure and promote the shadow with minimal downtime.
3. Reconfigure each pod's personality (`PERSONALITY = master|shadow` in `sfsmaster.cfg`) before LeilFS starts.

Several approaches were considered.

---

## Decision Drivers

- **No extra images**: avoid building a dedicated sidecar image; the existing `saunafs-master` image contains `wget` but not `kubectl` or `curl`.
- **No operator-managed TTL loop**: the operator should not become a single point of failure for the election; if the operator is down, the active master must continue serving.
- **Fast failover**: shadow must detect master failure and take over within ~30 seconds.
- **Simplicity**: the solution must be understandable and maintainable without distributed systems expertise.
- **Kubernetes-native**: leverage existing primitives rather than introducing an external consensus store (etcd client, ZooKeeper, etc.).

---

## Options Considered

### Option A: Operator-driven election (operator holds the Lease)

The operator runs a controller loop that watches pod readiness, decides which pod is master, and writes `PERSONALITY` into a `ConfigMap` that the pod mounts.

**Pros:** simple operator logic, no in-pod scripting.

**Cons:**
- The operator becomes a single point of failure. If the operator crashes or is restarting during a master failure, no election happens and the cluster stalls.
- Requires the operator to know pod readiness in real time, adding latency between pod failure and ConfigMap update.
- Pod restarts are required to re-read the ConfigMap, but the timing is hard to coordinate.

### Option B: External leader-election library (e.g., `client-go` `leaderelection`)

Run a Go binary as sidecar that uses `client-go`'s leader election package.

**Pros:** battle-tested library, rich semantics.

**Cons:**
- Requires building and maintaining a dedicated sidecar image.
- Adds a Go runtime dependency and image pull overhead to every master pod.
- Over-engineered for the use case (only two replicas, simple boolean election).

### Option C: Sidecar shell script + Kubernetes Lease (chosen)

Each master pod runs an inline shell script sidecar that interacts directly with the Kubernetes API via `wget` and the pod's `ServiceAccount` token. A `Lease` object (`coordination.k8s.io/v1`) acts as the distributed lock. The operator is a passive observer.

---

## Decision

**Option C** was chosen.

### Architecture

```
┌─────────────────────────────────────────────────────┐
│  StatefulSet  leilfscluster-<name>-master          │
│                                                     │
│  Pod master-0              Pod master-1             │
│  ┌─────────────────┐       ┌─────────────────┐      │
│  │ init-config     │       │ init-config     │      │
│  │  GET Lease      │       │  GET Lease      │      │
│  │  if holder==me  │       │  if holder==me  │      │
│  │    → MASTER     │       │    → MASTER     │      │
│  │  else → SHADOW  │       │  else → SHADOW  │      │
│  ├─────────────────┤       ├─────────────────┤      │
│  │ saunafs-master  │       │ saunafs-master  │      │
│  │ (master|shadow) │       │ (master|shadow) │      │
│  ├─────────────────┤       ├─────────────────┤      │
│  │ ha-sidecar      │       │ ha-sidecar      │      │
│  │ if holder==me:  │       │ if holder==me:  │      │
│  │  renew every 10s│       │  renew every 10s│      │
│  │ else:           │       │ else:           │      │
│  │  watch expiry   │       │  watch expiry   │      │
│  │  CAS acquire    │       │  CAS acquire    │      │
│  │  delete_self()  │       │  delete_self()  │      │
│  └─────────────────┘       └─────────────────┘      │
└─────────────────────────────────────────────────────┘
                        │
          Kubernetes Lease: <cluster>-master-ha
                        │
          ┌─────────────────────────┐
          │ holderIdentity: pod-1   │
          │ leaseDurationSeconds: 30│
          │ renewTime: <timestamp>  │
          └─────────────────────────┘
                        │
              Operator (passive observer)
              - syncs Service selector
              - updates status.ActiveMaster
```

### Lease semantics

| Field | Value |
|---|---|
| `holderIdentity` | Name of the active master pod |
| `leaseDurationSeconds` | 30 s |
| Renewal interval | 10 s (by the holder's sidecar) |
| Observe interval | 5 s (by shadow sidecars) |

### Election protocol

**Holder sidecar:**
1. `GET` Lease every 10 s.
2. If `holderIdentity == my pod name`: `PATCH` Lease to update `renewTime`. If the PATCH result shows a different holder, call `delete_self()`.
3. `delete_self()` issues a `DELETE /api/v1/.../pods/<name>` via the Kubernetes API. Kubernetes recreates the pod; init-containers re-run and reconfigure personality correctly.

**Shadow sidecar:**
1. `GET` Lease every 5 s.
2. Compute `age = now - renewTime`. If `age < leaseDurationSeconds`, the Lease is fresh — do nothing.
3. If Lease is expired (or has no `renewTime`), attempt a compare-and-swap `PATCH` using `metadata.resourceVersion` as the optimistic lock. Only one shadow can win the CAS.
4. On CAS success: call `delete_self()` so the pod restarts, init-container reads the new `holderIdentity`, and saunafs-master starts as master.

**Init-container (`init-config`):**
- Reads the Lease via `wget` before saunafs-master starts.
- If `holderIdentity == $POD_NAME` → writes `PERSONALITY = master` to `sfsmaster.cfg`.
- Otherwise → writes `PERSONALITY = shadow` and `MASTER_HOST = <service>`.

**Operator role (passive):**
- Creates the Lease at bootstrap (no `holderIdentity`).
- Reads `holderIdentity` every 5 s and syncs the master `Service` selector to route traffic to the active master pod.
- Updates `status.ActiveMaster` and `status.ReadyShadows` on the CR.
- Never modifies `holderIdentity` itself.

### RBAC

A dedicated `ServiceAccount` (`<cluster>-master`) is created per cluster with a `Role` granting:
- `get`, `update`, `patch` on the specific Lease (by `resourceName`).
- `delete` on `pods` (for `delete_self()`).

The operator's own `ClusterRole` is extended with permissions to manage `ServiceAccount`, `Role`, and `RoleBinding` objects so it can provision this RBAC automatically.

---

## Consequences

**Positive:**
- The operator is not on the critical path of failover. A master failure is detected and recovered by the shadow sidecar even if the operator is down.
- No additional container images are required. The sidecar is an inline shell script.
- The election primitive (`Lease`) is a first-class Kubernetes API, with atomic semantics guaranteed by the API server (optimistic locking via `resourceVersion`).
- Failover time is bounded: `leaseDurationSeconds` (30 s) + pod restart time (~5–10 s).

**Negative / trade-offs:**
- The sidecar script is non-trivial shell code embedded in a Go string literal, which makes it harder to test in isolation and to read diffs.
- JSON parsing is done with `sed` (no `jq` available), which is fragile if the Kubernetes API response format changes significantly.
- The `delete_self()` mechanism relies on Kubernetes recreating the pod (requires the pod to be managed by a StatefulSet with `Always` restart policy). Direct process signalling across containers (e.g., `kill`) was considered but rejected because it requires `shareProcessNamespace` and is less robust across container runtimes.
- `imagePullSecrets` for the SA `<cluster>-master` are automatically propagated from the `default` SA in the same namespace on every reconcile.

---

## References

- [Kubernetes Lease API](https://kubernetes.io/docs/concepts/architecture/leases/)
- [Leader election in Kubernetes using Lease objects](https://kubernetes.io/blog/2023/12/20/kubernetes-1-29-feature-leader-election-ga/)
- `internal/controller/leilfscluster_controller.go`: `reconcileMasterHA`, `reconcileMasterHARBAC`, `reconcileMasterStatefulSet`
