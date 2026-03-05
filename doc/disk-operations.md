# Disk Operations

This guide covers common disk maintenance procedures for a SaunaFS cluster managed by the operator.

The recommended layout is **1 chunkserver per disk**: each entry in `spec.chunk.servers` manages
exactly one disk. With 3 nodes and 2 disks per node this gives 6 chunkservers, which enables
`ec(4,2)` erasure coding — 50% storage overhead with tolerance for 2 simultaneous failures.

| Nodes | Disks/node | Chunkservers | Best EC goal | Overhead |
|-------|-----------|--------------|-------------|----------|
| 3 | 2 | 6 | `ec(4,2)` | 50% |
| 3 | 3 | 9 | `ec(4,2)` or `ec(6,3)` | 50% / 50% |
| 5 | 2 | 10 | `ec(8,2)` | 25% |

---

## 1. Replacing a Faulty Disk

SaunaFS continuously monitors chunk availability. When a disk fails, the master detects missing chunks
and automatically triggers re-replication onto other chunk servers (provided the configured `goal`
leaves enough surviving copies).

**Procedure:**

1. Wait for SaunaFS to finish re-replicating the affected chunks (monitor via `saunafs-admin`
   or the web UI).
2. Physically remove the faulty disk and install the replacement.
3. Format and mount the new disk at the **same host path** as the old one (e.g. `/mnt/disk2`).
4. The chunk server process will restart or re-scan its data directories and re-register with
   the master using the now-empty disk.
5. SaunaFS will gradually fill the new disk as new writes arrive and as the rebalancer redistributes
   existing chunks.

> **No CRD change is needed** as long as the replacement disk is mounted at the same `hostPath`.
> If the path changes, update the corresponding `mountPaths` entry in the `SaunaFSCluster` resource
> and the operator will roll out the updated StatefulSet.

---

## 2. Replacing a Disk with a Larger One

The procedure is identical to replacing a faulty disk. Once the larger disk is mounted at the same
`hostPath`, SaunaFS will automatically prefer it for new writes (the master allocates chunks
proportionally to available free space). No manual rebalancing is required.

**Procedure:**

1. Drain the disk if possible: use `saunafs-admin` to mark the chunk server as removing data,
   or simply proceed directly if downtime is acceptable.
2. Physically swap the disk and mount the new one at the same `hostPath`.
3. The chunk server re-registers with the master. SaunaFS starts routing new writes to the
   larger disk preferentially.

> Again, **no CRD change is needed** if the `hostPath` stays the same.

---

## 3. Adding a New Disk to a Node

With the 1 chunkserver per disk model, adding a disk means **adding a new server entry** to
`spec.chunk.servers`. The existing chunkservers on that node are not affected.

**Example** — adding a third disk on `k8s-worker-1`:

```yaml
spec:
  chunk:
    servers:
      - name: node1-disk1          # untouched
        nodeName: k8s-worker-1
        mountPaths:
          - path: /mnt/hdd001
            hostPath: /mnt/hdd001
      - name: node1-disk2          # untouched
        nodeName: k8s-worker-1
        mountPaths:
          - path: /mnt/hdd002
            hostPath: /mnt/hdd002
      - name: node1-disk3          # ← new entry only
        nodeName: k8s-worker-1
        mountPaths:
          - path: /mnt/hdd003
            hostPath: /mnt/hdd003
```

**Procedure:**

1. Format and mount the new disk on the host node (e.g. at `/mnt/hdd003`).
2. Add the new entry to `spec.chunk.servers` in the `SaunaFSCluster` resource.
3. The operator creates a new StatefulSet for this chunkserver only — existing pods are not restarted.
4. SaunaFS registers the new chunkserver and starts routing writes to it.

> With the 1 chunkserver per disk model, adding a disk creates a **new** StatefulSet without
> touching the existing ones — no restart of running chunkservers. This is the main operational
> advantage of this layout over the multi-disk-per-chunkserver model.
