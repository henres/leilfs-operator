/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// log is for logging in this package.
var leilfsclusterlog = logf.Log.WithName("leilfscluster-resource")

// SetupWebhookWithManager registers the LeilFSCluster validating webhook with
// the manager.
func (r *LeilFSCluster) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(r).
		WithValidator(&LeilFSClusterCustomValidator{}).
		Complete()
}

// TODO(user): change verbs to "verbs=create;update;delete" if you want to enable deletion validation.
//
//+kubebuilder:webhook:path=/validate-leilfs-leilfs-operator-io-v1alpha1-leilfscluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=leilfs.leilfs-operator.io,resources=leilfsclusters,verbs=create;update,versions=v1alpha1,name=vleilfscluster.kb.io,admissionReviewVersions=v1

// LeilFSClusterCustomValidator validates LeilFSCluster resources on create and
// update.
//
// This is a standalone type rather than methods on LeilFSCluster itself: as
// of controller-runtime v0.16, the in-type webhook.Validator interface
// (ValidateCreate/ValidateUpdate/ValidateDelete on the API type) is
// deprecated in favor of admission.CustomValidator, registered separately via
// WithValidator(...) above. See sigs.k8s.io/controller-runtime/pkg/webhook.Validator's
// doc comment ("Deprecated: Use CustomValidator instead") — go.mod pins
// controller-runtime v0.17.0, so this repo uses the current pattern.
//
// +kubebuilder:object:generate=false
type LeilFSClusterCustomValidator struct{}

var _ webhook.CustomValidator = &LeilFSClusterCustomValidator{}

// NodePort range accepted by the Kubernetes API server itself
// (--service-node-port-range defaults to 30000-32767). Mirroring it here
// gives fast, aggregated feedback instead of a later apiserver rejection when
// the operator tries to create the backing Service.
const (
	nodePortRangeMin = 30000
	nodePortRangeMax = 32767
)

// ValidateCreate implements admission.CustomValidator.
func (v *LeilFSClusterCustomValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	cluster, ok := obj.(*LeilFSCluster)
	if !ok {
		return nil, fmt.Errorf("expected a LeilFSCluster object but got %T", obj)
	}
	leilfsclusterlog.Info("validate create", "name", cluster.GetName())

	return nil, validateLeilFSCluster(cluster)
}

// ValidateUpdate implements admission.CustomValidator.
func (v *LeilFSClusterCustomValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	cluster, ok := newObj.(*LeilFSCluster)
	if !ok {
		return nil, fmt.Errorf("expected a LeilFSCluster object but got %T", newObj)
	}
	leilfsclusterlog.Info("validate update", "name", cluster.GetName())

	return nil, validateLeilFSCluster(cluster)
}

// ValidateDelete implements admission.CustomValidator. Deletion is always
// allowed — there is nothing in spec to validate on the way out.
func (v *LeilFSClusterCustomValidator) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	cluster, ok := obj.(*LeilFSCluster)
	if !ok {
		return nil, fmt.Errorf("expected a LeilFSCluster object but got %T", obj)
	}
	leilfsclusterlog.Info("validate delete", "name", cluster.GetName())

	return nil, nil
}

// validateLeilFSCluster runs every programmatic validation rule against a
// LeilFSCluster and, if any rule is violated, returns a single aggregated
// *apierrors.StatusError (kind Invalid) listing every violation found — not
// just the first one.
func validateLeilFSCluster(cluster *LeilFSCluster) error {
	var allErrs field.ErrorList

	specPath := field.NewPath("spec")
	serversPath := specPath.Child("chunk", "servers")

	allErrs = append(allErrs, validateChunkServerNames(cluster.Spec.Chunk.Servers, serversPath)...)
	allErrs = append(allErrs, validateChunkServerMountPaths(cluster.Spec.Chunk.Servers, serversPath)...)
	allErrs = append(allErrs, validateNodePorts(cluster, specPath)...)

	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(
		schema.GroupKind{Group: GroupVersion.Group, Kind: "LeilFSCluster"},
		cluster.Name,
		allErrs,
	)
}

// validateChunkServerNames rejects duplicate spec.chunk.servers[].name
// values. reconcileChunkServers derives every generated object name (headless
// Service and hdd ConfigMap in reconcileChunkHeadlessService /
// reconcileChunkHddConfigMap, StatefulSet in reconcileChunkStatefulSet — all
// in internal/controller/leilfscluster_controller.go) as
// "<cluster>-chunk-<name>", so two servers sharing a name would silently
// collide on the same Kubernetes objects, each reconcile overwriting the
// other's Service/ConfigMap/StatefulSet.
func validateChunkServerNames(servers []ChunkServerSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	firstSeenAt := make(map[string]int, len(servers))
	for i, srv := range servers {
		if j, ok := firstSeenAt[srv.Name]; ok {
			allErrs = append(allErrs, field.Duplicate(
				fldPath.Index(i).Child("name"),
				fmt.Sprintf("%s (already used by spec.chunk.servers[%d].name)", srv.Name, j),
			))
			continue
		}
		firstSeenAt[srv.Name] = i
	}
	return allErrs
}

// mountRef identifies the chunk server / mountPath entry that first claimed a
// given HostPath or ClaimName, for duplicate error messages.
type mountRef struct {
	serverIdx, mountIdx int
}

// validateChunkServerMountPaths rejects mountPaths entries that reference the
// exact same underlying storage twice, at two different blast radii:
//
//  1. Within a single chunk server: reconcileChunkHddConfigMap writes one
//     sfshdd.cfg line per mountPath, and buildChunkVolumes/
//     reconcileChunkStatefulSet mounts one Volume per mountPath. The same
//     HostPath or ClaimName listed twice in one server's mountPaths mounts the
//     same physical storage at two container paths, which LeilFS would then
//     account for as two independent chunk storage directories sharing one
//     underlying disk/PVC.
//  2. Across chunk servers: the same ClaimName reused by two different
//     ChunkServerSpec entries mounts the same PersistentVolumeClaim (normally
//     ReadWriteOnce) from two different StatefulSets — the second pod will
//     fail to attach/mount it. The same HostPath reused by two servers pinned
//     to the same NodeName mounts the same host directory into two different
//     chunk server pods scheduled onto that node, corrupting/duplicating
//     accounting for what LeilFS believes is independent storage. HostPath
//     reuse across servers on *different* nodes is fine (and the normal case
//     for auto-discovered/local-disk deployments, where every node exposes
//     the same conventional mount point) so it is intentionally not flagged.
func validateChunkServerMountPaths(servers []ChunkServerSpec, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	claimOwners := make(map[string]mountRef)
	// Keyed by NodeName + "\x00" + HostPath so reuse is only flagged when both
	// match (see rule 2 above).
	hostPathOwners := make(map[string]mountRef)

	for i, srv := range servers {
		mountsPath := fldPath.Index(i).Child("mountPaths")

		hostPathsInServer := make(map[string]int, len(srv.MountPaths))
		claimNamesInServer := make(map[string]int, len(srv.MountPaths))

		for j, mp := range srv.MountPaths {
			entryPath := mountsPath.Index(j)

			if mp.HostPath != "" {
				if k, ok := hostPathsInServer[mp.HostPath]; ok {
					allErrs = append(allErrs, field.Duplicate(
						entryPath.Child("hostPath"),
						fmt.Sprintf("%s (already used by spec.chunk.servers[%d].mountPaths[%d] on the same chunk server)", mp.HostPath, i, k),
					))
				} else {
					hostPathsInServer[mp.HostPath] = j

					key := srv.NodeName + "\x00" + mp.HostPath
					if owner, ok := hostPathOwners[key]; ok {
						allErrs = append(allErrs, field.Duplicate(
							entryPath.Child("hostPath"),
							fmt.Sprintf("%s on node %q (already used by spec.chunk.servers[%d].mountPaths[%d])",
								mp.HostPath, srv.NodeName, owner.serverIdx, owner.mountIdx),
						))
					} else {
						hostPathOwners[key] = mountRef{serverIdx: i, mountIdx: j}
					}
				}
			}

			if mp.ClaimName != "" {
				if k, ok := claimNamesInServer[mp.ClaimName]; ok {
					allErrs = append(allErrs, field.Duplicate(
						entryPath.Child("claimName"),
						fmt.Sprintf("%s (already used by spec.chunk.servers[%d].mountPaths[%d] on the same chunk server)", mp.ClaimName, i, k),
					))
				} else {
					claimNamesInServer[mp.ClaimName] = j

					if owner, ok := claimOwners[mp.ClaimName]; ok {
						allErrs = append(allErrs, field.Duplicate(
							entryPath.Child("claimName"),
							fmt.Sprintf("%s (already used by spec.chunk.servers[%d].mountPaths[%d])",
								mp.ClaimName, owner.serverIdx, owner.mountIdx),
						))
					} else {
						claimOwners[mp.ClaimName] = mountRef{serverIdx: i, mountIdx: j}
					}
				}
			}
		}
	}

	return allErrs
}

// validateNodePorts rejects NodePort values outside the valid Kubernetes
// NodePort range. 0 is always allowed — it means "let Kubernetes allocate a
// port", per ExposeSpec.ClientNodePort/AdminNodePort and NFSSpec.NodePort's
// documented semantics. The +kubebuilder:validation:Minimum/Maximum markers
// on those fields already enforce this same range at the CRD OpenAPI schema
// level for explicitly-set values; this webhook duplicates the check so a
// NodePort violation is reported together with the chunk-server rules above
// in one aggregated response, and so the message matches the apiserver's own
// --service-node-port-range check the operator will otherwise hit later when
// it creates the backing Service.
func validateNodePorts(cluster *LeilFSCluster, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	check := func(value int32, childPath *field.Path) {
		if value == 0 {
			return
		}
		if value < nodePortRangeMin || value > nodePortRangeMax {
			allErrs = append(allErrs, field.Invalid(
				childPath, value,
				fmt.Sprintf("must be 0 (let Kubernetes auto-assign) or between %d and %d", nodePortRangeMin, nodePortRangeMax),
			))
		}
	}

	check(cluster.Spec.Expose.ClientNodePort, fldPath.Child("expose", "clientNodePort"))
	check(cluster.Spec.Expose.AdminNodePort, fldPath.Child("expose", "adminNodePort"))
	check(cluster.Spec.NFS.NodePort, fldPath.Child("nfs", "nodePort"))

	return allErrs
}
