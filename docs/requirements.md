# SaunaFS Operator — Infrastructure Requirements

## Minimum node count

| Configuration | Min nodes | Rationale |
|---|---|---|
| Dev / single-node | 1 | Only with `replication: 1`; no fault tolerance |
| HA master + shadow only | 2 | Requires anti-affinity; no chunk fault tolerance |
| **Production minimum** | **3** | Required for `ec(4,2)` node-level fault tolerance |
| Recommended | 5+ | Full node failure tolerant with headroom |

### Why 2 nodes is not enough for production

The default goal `ec(4,2)` writes 6 chunks (4 data + 2 parity) across chunk servers.
With only 2 nodes (e.g. 3 chunk servers per node), a single node failure removes 3 chunks
simultaneously — exactly the parity budget. Any additional disk error on the surviving node
causes irrecoverable data loss.

The `node_spread` replication goal (3 copies, one per node label) also requires 3 distinct
nodes by definition.

**Minimum 3 nodes is a hard requirement for production workloads.**

---

## Node topology for the sample CR

The sample CR (`config/samples/saunafs_v1alpha1_saunafscluster.yaml`) targets this layout:

```
node: worker1  →  chunkserver worker1-hdd001  (label=worker1)
                  chunkserver worker1-hdd002  (label=worker1)
node: worker2  →  chunkserver worker2-hdd001  (label=worker2)
                  chunkserver worker2-hdd002  (label=worker2)
node: worker3  →  chunkserver worker3-hdd001  (label=worker3)
                  chunkserver worker3-hdd002  (label=worker3)
```

Total: 6 chunkservers → satisfies `ec(4,2)`. One full node can be lost without data loss.

---

## Resource requirements

All values below are the **built-in defaults** applied by the operator when no
`resources` field is set in the CR spec. Override per-component via
`spec.<component>.resources.requests/limits`.

### Per-pod defaults

| Component | CPU request | CPU limit | RAM request | RAM limit | Notes |
|---|---|---|---|---|---|
| `master` (active) | 100m | 1000m | 512Mi | 2Gi | Scales with metadata size (see below) |
| `master` (shadow) | 100m | 1000m | 512Mi | 2Gi | Same spec as active master |
| `ha-sidecar` | 5m | 50m | 16Mi | 32Mi | wget every 10 s, minimal footprint |
| `chunkserver` | 100m | 2000m | 256Mi | 1Gi | Per-disk pod; adjust for high-IOPS |
| `metalogger` | 50m | 200m | 128Mi | 512Mi | Changelog replay only |
| `nfs-ganesha` | 100m | 2000m | 256Mi | 1Gi | Per-client state; CPU-intensive under load |
| `cgiserver` | 10m | 200m | 64Mi | 128Mi | Lightweight web UI |

### Master RAM sizing — metadata

The master keeps **all filesystem metadata in RAM**. Estimate:

```
~300 bytes per file/directory/chunk
```

| Files | Master RAM needed | Recommended limit |
|---|---|---|
| 1 million | ~300 Mi | 1 Gi |
| 5 million | ~1.5 Gi | 3 Gi |
| 10 million | ~3 Gi | 6 Gi |
| 50 million | ~15 Gi | 24 Gi |

**The shadow must be allocated the same RAM as the master** — it maintains an identical
in-memory copy of the metadata.

Override in the CR:

```yaml
spec:
  master:
    resources:
      requests:
        memory: "4Gi"
        cpu: "500m"
      limits:
        memory: "8Gi"
        cpu: "2000m"
```

### Total cluster footprint (3-node, sample CR)

| | Requests | Limits |
|---|---|---|
| CPU | ~1.0 core | ~11 cores |
| RAM | ~2.5 Gi | ~9 Gi |
| PVC (metadata) | 4 × 1 Gi = 4 Gi | 4 Gi |
| Disk (chunk data) | hostPath — unbounded | depends on dataset |

---

## Anti-affinity

The operator sets a **preferred** (`weight: 100`) pod anti-affinity on the master
StatefulSet using topology key `kubernetes.io/hostname`. This spreads the active master
and shadow pods across different nodes when possible.

"Preferred" rather than "required" is intentional: it allows single-node dev clusters
to function while still providing a strong scheduler hint in production.

**In production, ensure your cluster has at least 2 schedulable nodes for the master
pods** (one for master-0, one per shadow). Without distinct nodes, a single-node failure
kills both the active master and all shadows simultaneously.

---

## Storage

| Component | Volume type | Default size | Notes |
|---|---|---|---|
| master / shadow | PVC (VolumeClaimTemplate) | 1 Gi | Persists `/var/lib/saunafs` |
| metalogger | PVC (VolumeClaimTemplate) | 1 Gi | Persists changelog journal |
| chunkserver | hostPath or PVC | — | Configured per-server in `spec.chunk.servers[].mountPaths` |

PVCs created by VolumeClaimTemplates are **not deleted** when the StatefulSet or CR is
deleted — this is intentional for metadata persistence.

---

## No 2-node support — summary

SaunaFS does not document a minimum node count, but the combination of:
- `ec(4,2)` requiring 6 independent chunk servers
- node-spread replication goals requiring N distinct node labels
- HA master/shadow needing 2 distinct nodes for anti-affinity to be effective

...means that **2 nodes provides no meaningful fault tolerance** and is not a supported
configuration for production use. Use 3 nodes minimum, 5+ recommended.
