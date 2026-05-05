# leilfs-operator

Kubernetes operator for LeilFS clusters. Reconciles a `LeilFSCluster` CRD
into master/shadow StatefulSets, chunkservers, metalogger, CGI interface, and
optional NFS-Ganesha gateway and CSI driver.

## Repository layout

```
api/v1alpha1/                 # CRD types (LeilFSCluster, status, printcolumns)
cmd/main.go                   # manager entrypoint
cmd/kubectl-leilfs/          # kubectl plugin (filegoal, etc.)
internal/controller/          # reconciler (leilfscluster_controller.go ~2k LOC)
internal/metrics/             # cluster-level Prometheus metrics
config/crd/bases/             # generated CRD YAML (do not edit by hand)
config/rbac/                  # generated RBAC (do not edit by hand)
config/samples/               # sample LeilFSCluster
docker/                       # NFS-Ganesha image build context
hack/monitoring/              # ServiceMonitor + Grafana dashboards JSON
test/                         # e2e scripts (master-failover.sh, etc.)
docs/adr/                     # architecture decision records
chart/                        # Helm chart
```

The operator depends on **no other operator** at runtime. Chunkserver
auto-discovery watches `PersistentVolume` objects carrying the label
`localdisk-operator.io/disk` (set by the localdisk-operator), but the
operator does not import that project's API types.

## Build, test, deploy

| Command | What it does |
|---|---|
| `make manifests` | Regenerate CRD YAML and RBAC from kubebuilder annotations. **Run after any API or annotation change.** |
| `make generate` | Regenerate `zz_generated.deepcopy.go`. |
| `make fmt vet` | Format and static-check Go sources. |
| `make test` | Runs `manifests`, `generate`, `fmt`, `vet`, then envtest unit tests. |
| `make build` | Build the manager binary into `bin/manager`. |
| `make lint` | golangci-lint + yamllint. |
| `make docker-build` / `make docker-push` | Build/push the operator image (`IMG=...` to override tag). |
| `make docker-build-nfs-ganesha` | Build the NFS-Ganesha image (`docker/Dockerfile.nfs-ganesha`). |
| `make build-installer` | Emit a single consolidated YAML (CRDs + deployment) into `dist/install.yaml`. |
| `make monitoring-up` | Install kube-prometheus-stack + ServiceMonitor + dashboards in the current cluster. |
| `make monitoring-dashboards` | Re-sync `hack/monitoring/dashboards/*.json` into the Grafana ConfigMap. |
| `make test-plugin` | Smoke tests for `kubectl-leilfs` against a live cluster. |

## Generated code rule

CRD YAML, RBAC YAML, and DeepCopy methods are **generated**. Edit the
kubebuilder annotations in `api/v1alpha1/*.go` and on controller methods
(`+kubebuilder:rbac:...`) and run `make manifests generate`. Hand-editing
`config/crd/bases/` or `config/rbac/role.yaml` will be overwritten.

After modifying RBAC annotations on a controller method, both
`make manifests` and a re-deploy (apply `config/rbac/role.yaml`) are
required for the change to take effect in the cluster.

## Commit conventions

Conventional Commits: `type(scope): summary`.

Types: `feat`, `fix`, `refactor`, `test`, `chore`, `docs`, `perf`.

Recurring scopes: `ha`, `operator`, `rbac`, `crd`, `plugin`, `metrics`,
`monitoring`, `ci`.

The body explains **why** the change is needed and any subtle behaviour
the diff alone does not convey. Wrap at ~72 chars. See
`.opencode/skills/commit-after-validation/SKILL.md` for the full
checklist (run `make test` and `make manifests` before staging).

## HA master election

Active-master selection uses a Kubernetes `Lease` and a shell sidecar
embedded in every master pod (no separate image). The operator is a
passive observer that labels the holder pod and updates the Service
selector. Full protocol, init-container logic, sidecar loops, and pitfalls
are documented in `.opencode/skills/ha-lease-election/SKILL.md` and
`docs/adr/0001-master-ha-election-via-kubernetes-lease.md`.

## Reconcile patterns

Sub-reconciler order, StorageClass precedence, imagePullSecrets
propagation, RBAC scoping, and known API gaps are in
`.opencode/skills/operator-reconcile/SKILL.md`.

## Monitoring

The operator exposes metrics on `:8080/metrics` in plain HTTP (no
kube-rbac-proxy sidecar). Scraped by a `ServiceMonitor` shipped in
`hack/monitoring/servicemonitor.yaml` with the label
`release: kube-prom-stack` for kube-prometheus-stack auto-discovery.
Grafana dashboards in `hack/monitoring/dashboards/` are deployed via
`make monitoring-dashboards` into a ConfigMap labelled
`grafana_dashboard=1` (sidecar pickup).

The metrics currently exposed (`leilfs_cluster_*`) are
**Kubernetes-level only**: pod counts per role, HA state (active
master, shadow lag), reconcile timings and errors. They describe how
the operator manages the cluster, not the filesystem itself.

### Backlog: filesystem-level metrics exporter

The rich filesystem statistics shown in the upstream CGI UI
(`/etc/cgi`, port 9425) — total/used chunks, endangered/missing
chunks, replication queue depth, per-chunkserver used/total bytes,
master metadata version, op/s, goal status — are **not** scraped into
Prometheus today.

Planned approach:

- Build a small Prometheus exporter that talks to `sfsmaster` over
  its admin port (9419) using the upstream protocol. Reuse
  `saunafs-admin` JSON output where possible to avoid re-implementing
  the binary protocol.
- Re-publish the values as `leilfs_fs_*` metrics with stable labels
  (`cluster`, `chunkserver`, `goal`, …).
- Deploy either as a sidecar on the active master Pod (so failover
  follows the master Lease holder automatically) or as a standalone
  Deployment that targets the master `Service`. Sidecar is simpler
  for observability of the active master, standalone is simpler for
  RBAC and lifecycle.
- Ship a Grafana dashboard mirroring the CGI UI views (cluster
  health, chunk distribution, per-chunkserver breakdown, goal
  status), alongside the existing `leilfs-operator-overview.json`.

## Skills

This repo ships OpenCode skills under `.opencode/skills/`:

- `commit-after-validation` — pre-commit checklist, message format, scopes
- `dev-workflow` — build/load/deploy on the local test cluster
- `ha-lease-election` — full HA protocol reference
- `operator-reconcile` — reconcile loop patterns and conventions
