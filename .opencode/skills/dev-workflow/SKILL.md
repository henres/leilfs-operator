---
name: dev-workflow
description: Build, load, deploy, and test workflow for the leilfs-operator on the local 4-VM Lima k3s cluster (sfs-lima context). Replaces the older Kind-based workflow.
compatibility: opencode
---

## Cluster

- kubectl context: `sfs-lima` (local Lima k3s cluster, 1 control-plane
  + 3 workers). Verify with `kubectl config current-context`.
- Namespace for the operator Deployment: `leilfs-operator-system`
- Namespace for the sample CR: `default`
- CR name: `leilfscluster-sample`

The historical `kind-leilfs-operator` Kind cluster has been removed.

## Images

- Registry: `ghcr.io/henres/leilfs-container/` (master, chunkserver,
  metalogger, cgiserver, client) and
  `ghcr.io/henres/leilfs-operator/leilfs-operator` (operator).
- Pull secret: `ghcr-pull-secret`, attached to the `default` SA AND to
  `<cluster>-master` SA (the operator copies pull secrets from the
  default SA to the master SA on every reconcile).
- Build provenance: disabled (`provenance: false`).

## Common commands

### Build operator image
```sh
make docker-build IMG=ghcr.io/henres/leilfs-operator/leilfs-operator:dev
```

### Load images into the Lima cluster
The k3s VMs do not pull from the host registry; images must be saved
and imported into k3s containerd on each VM:
```sh
bash ../sfs-test-env/scripts/load-images.sh
```
This script saves every required `ghcr.io/henres/...` image from the
local Docker daemon and imports it into containerd on each Lima VM via
`limactl shell <vm> sudo k3s ctr images import`.

### Restart the operator after reload
```sh
kubectl --context sfs-lima -n leilfs-operator-system \
  rollout restart deployment/leilfs-operator-controller-manager
```

### After changing API types (CRD fields, status, printcolumns)
```sh
make manifests
kubectl --context sfs-lima apply -f config/crd/bases/leilfs.leilfs-operator.io_leilfsclusters.yaml
```

### After changing controller annotations (RBAC, etc.)
```sh
make manifests
kubectl --context sfs-lima apply -f config/rbac/role.yaml
# operator pod will pick up new permissions on its next reconcile
```

### After changing only controller logic (no API or RBAC change)
```sh
make docker-build IMG=ghcr.io/henres/leilfs-operator/leilfs-operator:dev
bash ../sfs-test-env/scripts/load-images.sh
kubectl --context sfs-lima -n leilfs-operator-system \
  rollout restart deployment/leilfs-operator-controller-manager
```

### Watch master pods
```sh
kubectl --context sfs-lima get pods -l app=leilfscluster-sample-master -w
```

### Watch the HA Lease
```sh
kubectl --context sfs-lima get lease leilfscluster-sample-master-ha -w
```

### Check sidecar logs
```sh
kubectl --context sfs-lima logs leilfscluster-sample-master-0 -c ha-sidecar -f
kubectl --context sfs-lima logs leilfscluster-sample-master-1 -c ha-sidecar -f
```

### Check init-container logs
```sh
kubectl --context sfs-lima logs leilfscluster-sample-master-0 -c init-config
```

### Run failover test
```sh
bash test/master-failover.sh   # honours $KUBECONFIG / current context
```

### Test failover by stopping a worker VM
```sh
limactl stop sfs-w1     # node goes NotReady within ~80 s
limactl start sfs-w1    # wait for node Ready, chunkservers/shadows recover automatically
```

## Operator RBAC

The operator's ClusterRole is **generated** from kubebuilder
`+kubebuilder:rbac:` annotations on controller methods plus
`api/v1alpha1/*.go`. Edit the annotations and run `make manifests` —
do NOT hand-edit `config/rbac/role.yaml`.

The operator needs (in addition to the default kubebuilder set):
- `serviceaccounts` get/list/watch/create/update/patch
- `roles` and `rolebindings` (rbac.authorization.k8s.io)
  get/list/watch/create/update/patch
- `pods` delete (for HA pod-delete loop and rolling restarts)

See `.opencode/skills/operator-reconcile/SKILL.md` for the full picture.

## Relevant directories

```
leilfs-operator/
  api/v1alpha1/              # CRD types
  config/
    crd/bases/               # Generated CRD YAML
    rbac/role.yaml           # Generated operator ClusterRole
    samples/                 # Sample CR
  internal/controller/       # Main reconciler
  test/master-failover.sh    # End-to-end failover test
  docs/adr/                  # Architecture decision records
```

## Makefile targets used in this workflow

| Target | Action |
|---|---|
| `make manifests` | Regenerate CRD YAML + RBAC from annotations |
| `make generate` | Regenerate DeepCopy methods |
| `make docker-build` | Build operator image |
| `make test` | Run unit tests (envtest) |
| `make build` | Build operator binary |
| `make monitoring-up` | Install kube-prometheus-stack + ServiceMonitor + dashboards |
| `make monitoring-dashboards` | Re-sync only the dashboards ConfigMap |
