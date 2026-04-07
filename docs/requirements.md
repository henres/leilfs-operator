# SaunaFS Operator — Infrastructure Requirements

> This operator is primarily designed for **homelab use**. The configurations below
> reflect that reality: single-node and 2-node setups are documented and supported,
> with explicit notes on the risks involved.

## Node configurations

| Configuration | Nodes | Chunk fault tolerance | Master HA | Suitable for |
|---|---|---|---|---|
| Single-node dev | 1 | None | No | Local testing only |
| **2-node homelab** | **2** | Disk-level only | Yes (with anti-affinity) | Homelab, non-critical data |
| 3-node standard | 3 | Full node failure | Yes | Small homelab / light prod |
| Recommended | 5+ | Full node failure + headroom | Yes | Production |

---

## 2-node homelab setup

A 2-node cluster is a valid and practical homelab configuration. It gives you HA on the
master layer (active + shadow on separate nodes) and keeps the filesystem available through
most single-disk failures — with the constraints below clearly understood.

### Recommended goal for 2 nodes: `ec(2,1)` or `replication: 2`

The default `ec(4,2)` goal requires 6 chunkservers and is designed for 3+ nodes.
On 2 nodes, use a goal that fits your chunkserver count:

```yaml
spec:
  goals:
    # ec(2,1): 3 CS required, 50% overhead, tolerates 1 disk failure.
    # Safe on 2 nodes as long as both nodes have at least 2 disks each.
    - id: 10
      name: ec_2_1
      ec:
        dataParts: 2
        parityParts: 1
      default: true

    # Or simple replication across both nodes (1 copy per node):
    - id: 11
      name: two_copies
      replication: 2
      nodeLabels: ["node1", "node2"]
```

### 2-node topology example

```
node: node1  →  chunkserver node1-hdd001  (label=node1)
                chunkserver node1-hdd002  (label=node1)
node: node2  →  chunkserver node2-hdd001  (label=node2)
                chunkserver node2-hdd002  (label=node2)
```

Total: 4 chunkservers → satisfies `ec(2,1)` or `replication: 2`.

### Risks on a 2-node setup

| Event | Impact with `ec(2,1)` | Impact with `replication: 2` |
|---|---|---|
| Single disk failure | No data loss, degraded mode | No data loss, degraded mode |
| Full node failure | **Data loss** (1 parity part gone + surviving node is single point of failure) | **Data loss** (only 1 copy remains, no redundancy) |
| Both nodes down simultaneously | Data unavailable | Data unavailable |
| Network partition | Split-brain risk on master if anti-affinity is not respected | Same |

**In summary:** on 2 nodes, you tolerate individual disk failures but **not a full node
failure without risk of data loss**. This is acceptable for a homelab where data can be
reconstructed or is non-critical. It is not acceptable for production workloads.

### Why `ec(4,2)` does not work on 2 nodes

`ec(4,2)` writes 6 chunks (4 data + 2 parity) in parallel. With only 4 chunkservers
(2 per node), there are not enough targets to write 6 chunks — writes will hang or fail.
Even if you add more disks per node to reach 6 chunkservers, a single node failure removes
3 chunks simultaneously, exhausting the entire parity budget and leaving data at risk from
any further disk error.

---

## 3-node standard setup

### Why 3 nodes is the minimum for `ec(4,2)`

The default goal `ec(4,2)` writes 6 chunks (4 data + 2 parity) across chunk servers.
With 3 nodes (2 disks each = 6 chunkservers), a full node failure removes 2 chunks —
within the parity budget. Data survives, and the cluster rebuilds automatically when
the node returns.

The `node_spread` replication goal (3 copies, one per node label) also requires exactly
3 distinct nodes by definition.

**3 nodes is the minimum for node-level fault tolerance with the default `ec(4,2)` goal.**

---

## Node topology for the sample CR (3-node)

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

...means that **2 nodes provides no node-level fault tolerance with the default goals**.
For homelab use, 2 nodes is workable with adapted goals (`ec(2,1)` or `replication: 2`),
accepting that a full node failure puts data at risk. Use 3 nodes minimum for node-level
fault tolerance, 5+ for production headroom.
