---
name: operator-reconcile
description: Patterns, helpers, and conventions used in the leilfs-operator reconcile loop, including StorageClass precedence, RBAC helpers, imagePullSecrets propagation, and CRD/status update rules.
compatibility: opencode
---

## Overview

The operator is a standard controller-runtime reconciler for the `LeilfsCluster` CRD.
Main file: `internal/controller/leilfscluster_controller.go` (~2148 lines).

## Reconcile entry point

`Reconcile()` calls sub-reconcilers in order:

1. `reconcileMasterStatefulSet` — creates/updates the master StatefulSet
2. `reconcileMasterHA` — passive Lease observer; updates pod labels and Service selector
3. `reconcileMasterHARBAC` — SA + Role + RoleBinding for the sidecar
4. `reconcileExposeService` — NodePort or LoadBalancer service for clients
5. `reconcileChunkServers` — StatefulSet(s) for chunk server pods
6. Status update at the end

`RequeueAfter: 5 * time.Second` is always returned to enable continuous Lease monitoring.

## StorageClass / VolumeClaimTemplate precedence

The master StatefulSet has a **single VolumeClaimTemplate** shared by all pods.
Master spec takes priority; shadow spec is the fallback:

```go
sc := masterSpec.StorageClass
if sc == "" {
    sc = shadowSpec.StorageClass
}
```

This is important: the shadow spec's `MetadataStorage` must NOT silently overwrite the
master spec's config.

## imagePullSecrets propagation

`reconcileMasterHARBAC` copies `imagePullSecrets` from the `default` ServiceAccount in
the same namespace to the `<cluster>-master` ServiceAccount on every reconcile.
This ensures the StatefulSet pods can pull from `ghcr.io/henres/leilfs-container/`.

```go
// pseudocode
defaultSA := corev1.ServiceAccount{}
client.Get(ctx, types.NamespacedName{Namespace: ns, Name: "default"}, &defaultSA)
masterSA.ImagePullSecrets = defaultSA.ImagePullSecrets
client.Update(ctx, &masterSA)
```

## RBAC scoping

The Role created by `reconcileMasterHARBAC` scopes pod/delete to explicit `resourceNames`:

```yaml
rules:
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["delete"]
  resourceNames: ["<cluster>-master-0", "<cluster>-master-1", ...]
- apiGroups: ["coordination.k8s.io"]
  resources: ["leases"]
  verbs: ["get", "update", "patch"]
```

The list of pod names is derived from `spec.MasterSpec.Replicas`.

## HA-aware Service selector

`reconcileExposeService` builds the selector based on HA mode:

```go
if haEnabled {
    selector = map[string]string{
        "leilfs.io/active-master": "true",
    }
} else {
    selector = map[string]string{
        "app": clusterName + "-master",
    }
}
```

Both the ClusterIP (`<cluster>-master`) and the NodePort (`<cluster>-client-expose`) services
use `leilfs.io/active-master=true` in HA mode.

## Status fields

`LeilfsClusterStatus` has:
- `ActiveMaster string` — pod name of the current Lease holder
- `ReadyShadows []string` — names of non-holder master pods

Updated at the end of each reconcile by reading the Lease.

## CRD print columns

Defined in `api/v1alpha1/leilfscluster_types.go`:

```go
// +kubebuilder:printcolumn:name="ActiveMaster",type=string,JSONPath=".status.activeMaster"
// +kubebuilder:printcolumn:name="Shadows",type=string,JSONPath=".status.readyShadows"
```

**Always run `make manifests` after changing API types** to regenerate
`config/crd/bases/leilfs.leilfs-operator.io_leilfsclusters.yaml`.

## Controller-level RBAC

The operator's own ClusterRole is also generated, from
`+kubebuilder:rbac:` annotations on the controller methods. To grant
new cluster-scoped permissions, add an annotation above the relevant
reconciler and re-run `make manifests`.

Example (controller needs to delete Pods to drive rolling restarts):

```go
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
func (r *LeilFSClusterReconciler) reconcileSomething(...) { ... }
```

Then `make manifests` updates `config/rbac/role.yaml` and
`kubectl apply -f config/rbac/role.yaml` deploys the change. Do NOT
hand-edit `role.yaml`; the next `make manifests` run will overwrite
edits.

The Role created by `reconcileMasterHARBAC` (the per-cluster Role
bound to the master ServiceAccount, scoped via `resourceNames`) is a
**different** RBAC object — that one IS managed in code, not via
kubebuilder annotations.

## Known API gaps (backlog)

These fields are declared in the API types but are NOT used in the controller:
- `ShadowSpec.Image`
- `ShadowSpec.NodeSelector`
- `ShadowSpec.Tolerations`
- `ShadowSpec.Resources`

All pods currently use the master spec's scheduling configuration.

## No probes, no PDB

- No readiness/liveness/startup probes on master or sidecar containers
- No `PodDisruptionBudget` for the master StatefulSet
- No `topologySpreadConstraints`

These are known gaps and are not blocking but should be addressed before production use.

## Unit tests

Tests are scaffolded in `internal/controller/leilfscluster_controller_test.go` but contain
no assertions on the StatefulSet, Service, Lease, or RBAC objects yet.
