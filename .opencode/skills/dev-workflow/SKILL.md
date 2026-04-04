---
name: dev-workflow
description: Build, load, deploy, and test workflow for the saunafs-operator on a local Kind cluster named kind-saunafs-operator.
compatibility: opencode
---

## Cluster

- Kind cluster: `kind-saunafs-operator`
- Namespace: `default`
- CR: `saunafscluster-sample`

## Images

- Registry: `ghcr.io/henres/saunafs-container/`
- Pull secret: `ghcr-pull-secret` (attached to SA `default` AND SA `<cluster>-master`)
- Build provenance: disabled (`provenance: false`)

## Common commands

### Build operator image and load into Kind
```sh
make kind-load
```
This builds the operator Docker image and loads it into the Kind cluster.

### After loading, restart the operator
```sh
kubectl rollout restart deployment/saunafs-operator-controller-manager -n saunafs-operator-system
```

### After changing API types (CRD fields, status, printcolumns)
```sh
make manifests
# Then re-apply the CRD:
kubectl apply -f config/crd/bases/saunafs.saunafs-operator.io_saunafsclusters.yaml
```

### After changing the controller only (no API changes)
```sh
make kind-load
kubectl rollout restart deployment/...
```

### Watch master pods
```sh
kubectl get pods -l app=saunafscluster-sample-master -w
```

### Watch the HA Lease
```sh
kubectl get lease saunafscluster-sample-master-ha -w
```

### Check sidecar logs
```sh
kubectl logs saunafscluster-sample-master-0 -c ha-sidecar -f
kubectl logs saunafscluster-sample-master-1 -c ha-sidecar -f
```

### Check init-container logs
```sh
kubectl logs saunafscluster-sample-master-0 -c init-config
```

### Run failover test
```sh
bash test/master-failover.sh
```

## Operator RBAC note

The operator's ClusterRole (`saunafs-operator-manager-role` in `config/rbac/role.yaml`) must
include permissions for `serviceaccounts`, `roles`, and `rolebindings` so that
`reconcileMasterHARBAC` can function. This was patched manually; ensure it is present when
re-applying the operator RBAC:

```yaml
- apiGroups: [""]
  resources: ["serviceaccounts"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
- apiGroups: ["rbac.authorization.k8s.io"]
  resources: ["roles", "rolebindings"]
  verbs: ["get", "list", "watch", "create", "update", "patch"]
```

## Relevant directories

```
saunafs-operator/
  api/v1alpha1/              # CRD types
  config/
    crd/bases/               # Generated CRD YAML
    rbac/role.yaml           # Operator ClusterRole
    samples/                 # Sample CR
  internal/controller/       # Main reconciler
  test/master-failover.sh    # End-to-end failover test
  docs/adr/                  # Architecture decision records
```

## Makefile targets

| Target | Action |
|---|---|
| `make manifests` | Regenerate CRD YAML from API types |
| `make generate` | Regenerate DeepCopy methods |
| `make kind-load` | Build + load operator image into Kind |
| `make test` | Run unit tests |
| `make build` | Build operator binary |
