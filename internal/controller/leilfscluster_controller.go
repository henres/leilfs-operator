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

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	saunafsv1alpha1 "github.com/henres/leilfs-operator/api/v1alpha1"
	"github.com/henres/leilfs-operator/internal/metrics"
)

// LeilFSClusterReconciler reconciles a LeilFSCluster object
type LeilFSClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// HA labels and lease constants.
const (
	// labelActiveMaster is set to "true" on whichever master/shadow pod
	// currently holds the active-master role. The master Service selector
	// matches this label so traffic always reaches the active pod.
	labelActiveMaster = "leilfs.io/active-master"

	defaultMasterImage = "ghcr.io/henres/leilfs-container/leilfs-master:5.10.1"

	// leaseDuration is how long a pod's Lease is valid without renewal.
	// When the holder dies or is isolated, after this period a shadow can
	// take over.  Keep it short enough for fast failover, long enough to
	// survive transient API-server hiccups (e.g. leader election).
	leaseDuration = 30 * time.Second

	// leaseRenewInterval is how often the sidecar renews the Lease.
	// Must be well below leaseDuration.
	leaseRenewInterval = 10 * time.Second

	// leaseObserveInterval is how often the operator observes the Lease to
	// sync the Service selector and status.  It does NOT renew the Lease —
	// that is the sidecar's responsibility.
	leaseObserveInterval = 5 * time.Second

	// leilfsClusterFinalizer blocks API-server deletion of a LeilFSCluster
	// until finalizeCluster has run. Almost every object this controller
	// creates already carries an owner reference back to the LeilFSCluster
	// (see the SetControllerReference calls throughout this file), so the
	// Kubernetes garbage collector cascades their deletion on its own; this
	// finalizer exists as a safety net / extension point for the day a new
	// dynamically-provisioned resource is added without one — see
	// finalizeCluster's doc comment for the full picture.
	leilfsClusterFinalizer = "leilfs.leilfs-operator.io/cleanup"
)

type unsupportedSpecError struct {
	problems []string
}

func (e *unsupportedSpecError) Error() string {
	return "unsupported LeilFSCluster spec: " + strings.Join(e.problems, "; ")
}

func isUnsupportedSpecError(err error) bool {
	for err != nil {
		if _, ok := err.(*unsupportedSpecError); ok {
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}

func validateClusterSpec(cluster *saunafsv1alpha1.LeilFSCluster) error {
	var problems []string

	if cluster.Spec.CSI.Enabled != nil && *cluster.Spec.CSI.Enabled {
		problems = append(problems, "spec.csi.enabled is not supported: the CSI driver is not implemented yet")
	}

	masterImage := cluster.Spec.Master.Image
	if masterImage == "" {
		masterImage = defaultMasterImage
	}
	if shadow := cluster.Spec.Shadow; shadow != nil {
		if shadow.Image != "" && shadow.Image != masterImage {
			problems = append(problems, "spec.shadow.image differs from spec.master.image but per-shadow images are not supported by the unified master StatefulSet")
		}
		if len(shadow.NodeSelector) > 0 {
			problems = append(problems, "spec.shadow.nodeSelector is not supported: master and shadow pods share one StatefulSet PodTemplate")
		}
		if len(shadow.Tolerations) > 0 {
			problems = append(problems, "spec.shadow.tolerations is not supported: master and shadow pods share one StatefulSet PodTemplate")
		}
		if len(shadow.Resources.Requests) > 0 || len(shadow.Resources.Limits) > 0 {
			problems = append(problems, "spec.shadow.resources is not supported: master and shadow pods share one StatefulSet PodTemplate")
		}
	}

	for i := range cluster.Spec.Chunk.Servers {
		server := &cluster.Spec.Chunk.Servers[i]
		for j := range server.MountPaths {
			mp := server.MountPaths[j]
			if mp.Path != "" && mp.HostPath == "" && mp.ClaimName == "" {
				problems = append(problems, fmt.Sprintf(
					"spec.chunk.servers[%d].mountPaths[%d]: dynamic PVC provisioning for chunk mountPaths is not implemented; set hostPath for local disks or claimName for an existing PVC",
					i, j,
				))
			}
		}
	}

	if len(problems) > 0 {
		return &unsupportedSpecError{problems: problems}
	}
	return nil
}

// defaultResources returns r if any requests or limits are already set,
// otherwise it returns the provided defaults.  This allows users to override
// the built-in baselines by setting spec.<component>.resources in the CR.
func defaultResources(r corev1.ResourceRequirements, defaults corev1.ResourceRequirements) corev1.ResourceRequirements {
	if len(r.Requests) > 0 || len(r.Limits) > 0 {
		return r
	}
	return defaults
}

// masterDefaultResources returns conservative but realistic resource defaults
// for a leilfs-master / shadow pod.
// RAM: the master keeps ALL metadata in memory (~300 bytes/file). 512Mi covers
// ~1.7M files at idle; the limit of 2Gi gives headroom for large namespaces.
// CPU: metadata operations are bursty; 100m idle, 1 core burst is a safe start.
func masterDefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("2Gi"),
		},
	}
}

// chunkDefaultResources returns defaults for a leilfs-chunkserver pod.
// RAM: chunk servers maintain a block map in memory; 256Mi request / 1Gi limit
// covers most workloads. Adjust upward for high-IOPS deployments.
func chunkDefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2000m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

// metaloggerDefaultResources returns defaults for a leilfs-metalogger pod.
// Metaloggers are lightweight: they only replay changelogs.
func metaloggerDefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("512Mi"),
		},
	}
}

// nfsDefaultResources returns defaults for an nfs-ganesha pod.
// NFS-Ganesha keeps per-client state and can be CPU-intensive under load.
func nfsDefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2000m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

// webUIDefaultResources returns defaults for the leilfs-cgiserver pod.
// The CGI interface is very lightweight.
func webUIDefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("200m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
}

// sidecarDefaultResources returns defaults for the ha-sidecar shell container.
// It only runs wget every 10 s — truly minimal footprint.
func sidecarDefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("5m"),
			corev1.ResourceMemory: resource.MustParse("16Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("50m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
	}
}

// exporterDefaultResources returns defaults for the leilfs-exporter
// sidecar. The work is bursty (one fork per saunafs-admin subcommand at
// each scrape, ~9 commands per scrape, scrape interval typically 30s),
// so requests stay tiny but limits leave headroom for the parsing and
// the JSON-encoded HTTP response.
func exporterDefaultResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("10m"),
			corev1.ResourceMemory: resource.MustParse("32Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
	}
}

// masterPodAntiAffinity returns a preferred anti-affinity rule that spreads
// master StatefulSet pods across different nodes.  Using "preferred" rather
// than "required" avoids unschedulable pods on single-node dev clusters while
// still giving the scheduler a strong hint to separate master and shadows in
// production.
func masterPodAntiAffinity(stsName string) *corev1.Affinity {
	return &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{
				{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						LabelSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app.kubernetes.io/name": "leilfs-master",
							},
						},
						TopologyKey: "kubernetes.io/hostname",
					},
				},
			},
		},
	}
}

//+kubebuilder:rbac:groups=leilfs.leilfs-operator.io,resources=leilfsclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=leilfs.leilfs-operator.io,resources=leilfsclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=leilfs.leilfs-operator.io,resources=leilfsclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=daemonsets;statefulsets;deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims;configmaps;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch;delete
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch
//+kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete

func (r *LeilFSClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cluster := &saunafsv1alpha1.LeilFSCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if errors.IsNotFound(err) {
			// Cluster was deleted: drop all of its metrics so Prometheus
			// doesn't keep stale series for non-existent objects.
			metrics.DeleteCluster(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling LeilFSCluster", "name", cluster.Name)

	// ── Finalizer handling ────────────────────────────────────────────────────
	// Standard controller-runtime finalizer dance: register the finalizer on
	// first sight of a live object; once the object is marked for deletion,
	// run cleanup and only then let the finalizer go so the API server can
	// actually remove it.
	if cluster.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(cluster, leilfsClusterFinalizer) {
			controllerutil.AddFinalizer(cluster, leilfsClusterFinalizer)
			if err := r.Update(ctx, cluster); err != nil {
				return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
			}
		}
	} else {
		if controllerutil.ContainsFinalizer(cluster, leilfsClusterFinalizer) {
			if err := r.finalizeCluster(ctx, cluster); err != nil {
				return ctrl.Result{}, fmt.Errorf("finalizing cluster: %w", err)
			}
			controllerutil.RemoveFinalizer(cluster, leilfsClusterFinalizer)
			if err := r.Update(ctx, cluster); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		// Deletion in progress: skip normal reconciliation entirely. There is
		// nothing left to do once cleanup has run (or the finalizer was
		// already gone) — creating/updating owned objects for a cluster that
		// is being torn down would just be undone by the garbage collector.
		return ctrl.Result{}, nil
	}

	// Snapshot the cluster object BEFORE any reconcile step mutates its status,
	// so the MergePatch below captures all status changes made during reconcile.
	statusPatchBase := client.MergeFrom(cluster.DeepCopy())

	// Run all sub-reconcilers; on first error record a Failed condition.
	var reconcileErr error
	steps := []struct {
		name string
		fn   func(context.Context, *saunafsv1alpha1.LeilFSCluster) error
	}{
		{"validate spec", func(_ context.Context, c *saunafsv1alpha1.LeilFSCluster) error { return validateClusterSpec(c) }},
		{"goals configmap", r.reconcileGoalsConfigMap},
		{"migrate legacy master objects", r.migrateMasterToStatefulSet},
		{"master statefulset", r.reconcileMasterStatefulSet},
		{"master service", r.reconcileMasterService},
		{"master ha rbac", r.reconcileMasterHARBAC},
		{"master ha", r.reconcileMasterHA},
		{"master pdb", r.reconcileMasterPodDisruptionBudget},
		{"chunk servers", r.reconcileChunkServers},
		{"auto-discover chunk servers", r.reconcileAutoDiscoverChunkServers},
		{"metaloggers", r.reconcileMetaloggers},
		{"interface", r.reconcileInterface},
		{"expose service", r.reconcileExposeService},
		{"nfs", r.reconcileNFS},
	}
	for _, step := range steps {
		if err := step.fn(ctx, cluster); err != nil {
			reconcileErr = fmt.Errorf("reconcile %s: %w", step.name, err)
			break
		}
	}

	cluster.Status.TotalChunkServers = r.countTotalChunkServers(ctx, cluster)

	if reconcileErr != nil {
		reason := saunafsv1alpha1.ReasonReconcileError
		if isUnsupportedSpecError(reconcileErr) {
			reason = saunafsv1alpha1.ReasonUnsupportedSpec
		}
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               saunafsv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             reason,
			Message:            reconcileErr.Error(),
			ObservedGeneration: cluster.Generation,
		})
	} else {
		ready := r.countReadyChunkServers(ctx, cluster)
		cluster.Status.ReadyChunkServers = ready

		readyML := r.countReadyMetaloggers(ctx, cluster)
		cluster.Status.ReadyMetaloggers = readyML

		cluster.Status.ReadyShadows = r.countReadyShadows(ctx, cluster)

		msg := fmt.Sprintf("All components reconciled (%d/%d chunk servers ready)",
			ready, cluster.Status.TotalChunkServers)
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               saunafsv1alpha1.ConditionReady,
			Status:             metav1.ConditionTrue,
			Reason:             saunafsv1alpha1.ReasonReady,
			Message:            msg,
			ObservedGeneration: cluster.Generation,
		})
	}

	if err := r.Status().Patch(ctx, cluster, statusPatchBase); err != nil {
		logger.Error(err, "Failed to update LeilFSCluster status")
		return ctrl.Result{}, err
	}

	// Publish Prometheus metrics from the freshly observed status. We do
	// this after Status().Patch so the values shown in metrics are
	// consistent with what is persisted on the cluster object.
	r.publishMetrics(ctx, cluster, reconcileErr)

	// When shadow HA is configured, requeue regularly so the operator can observe
	// Lease changes and sync Service selector / status without waiting for a watch event.
	if cluster.Spec.Shadow != nil && reconcileErr == nil {
		return ctrl.Result{RequeueAfter: leaseObserveInterval}, nil
	}

	return ctrl.Result{}, reconcileErr
}

// finalizeCluster runs cleanup for a LeilFSCluster that is being deleted,
// before its finalizer is removed and the API server is allowed to finish
// deleting the object.
//
// What actually needs cleanup here (audited by checking every SetControllerReference
// call in this file): NOTHING, as of today. Every object this controller
// creates — the goals ConfigMap, master/chunk/metalogger headless Services,
// the chunk hdd ConfigMap, chunk/metalogger StatefulSets, the interface and
// NFS Deployments/Services/ConfigMaps, the master HA Lease and its
// ServiceAccount/Role/RoleBinding, and — importantly — the PVC that
// ensureAutoDiscoverPVC creates for each auto-discovered PV — all carry an
// owner reference back to the LeilFSCluster. The Kubernetes garbage
// collector already cascades their deletion; there is no gap for a
// finalizer to fill for those.
//
// The one thing that is deliberately NOT touched here is the master/shadow
// metadata PVC created via the master StatefulSet's VolumeClaimTemplate
// (see reconcileMasterStatefulSet's doc comment): those PVCs intentionally
// survive both StatefulSet and CR deletion so filesystem metadata isn't
// destroyed by an operator action. Deleting them would defeat the entire
// point of "hardening" the cluster lifecycle, so this finalizer must never
// do so — not even as an opt-in.
//
// This function is therefore a deliberate no-op today. It is kept as the
// designated extension point for any future dynamically-provisioned
// resource that can't carry an owner reference (e.g. anything cluster-scoped,
// since owner references cannot cross from a namespaced owner to a
// cluster-scoped object) or that is otherwise created without one by
// mistake.
func (r *LeilFSClusterReconciler) finalizeCluster(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	logger := log.FromContext(ctx)
	logger.Info("Finalizing LeilFSCluster: all owned resources are garbage-collected via owner references; no explicit cleanup required",
		"name", cluster.Name, "namespace", cluster.Namespace)
	return nil
}

// publishMetrics updates Prometheus collectors from the freshly
// reconciled cluster state. It is best-effort: any errors while
// counting auto-discovered PVs are logged and don't fail the reconcile.
func (r *LeilFSClusterReconciler) publishMetrics(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster, reconcileErr error) {
	logger := log.FromContext(ctx)
	ns := cluster.Namespace
	name := cluster.Name

	// Cluster info: tag with the master image so dashboards can group by
	// LeilFS version. Empty string when the user didn't pin an image.
	version := cluster.Spec.Master.Image
	metrics.ClusterInfo.WithLabelValues(ns, name, version).Set(1)

	// Phase: derive from the latest Ready condition.
	phase := "Pending"
	if cond := apimeta.FindStatusCondition(cluster.Status.Conditions, saunafsv1alpha1.ConditionReady); cond != nil {
		switch cond.Status {
		case metav1.ConditionTrue:
			phase = "Ready"
		case metav1.ConditionFalse:
			if reconcileErr != nil {
				phase = "Failed"
			} else {
				phase = "Reconciling"
			}
		}
	}
	metrics.SetPhase(ns, name, phase)

	// Master replicas (desired = 1 base + len(Shadow.Replicas) when HA).
	desiredMasters := int32(1)
	if cluster.Spec.Shadow != nil && cluster.Spec.Shadow.Replicas != nil {
		desiredMasters += *cluster.Spec.Shadow.Replicas
	}
	metrics.MasterReplicasDesired.WithLabelValues(ns, name).Set(float64(desiredMasters))
	readyShadows := cluster.Status.ReadyShadows
	readyMasters := readyShadows
	if cluster.Status.ActiveMaster != "" {
		readyMasters++
	}
	metrics.MasterReplicasReady.WithLabelValues(ns, name).Set(float64(readyMasters))
	metrics.ShadowReplicasReady.WithLabelValues(ns, name).Set(float64(readyShadows))

	// Metaloggers.
	metrics.MetaloggersReady.WithLabelValues(ns, name).Set(float64(cluster.Status.ReadyMetaloggers))

	// Chunkservers: split desired between manual and auto-discover.
	manualDesired := int32(len(cluster.Spec.Chunk.Servers))
	metrics.ChunkServersDesired.WithLabelValues(ns, name, metrics.SourceManual).Set(float64(manualDesired))

	autoDesired := int32(0)
	if ad := cluster.Spec.Chunk.AutoDiscover; ad != nil && ad.Enabled {
		// Re-list PVs to obtain a current count of matched PVs. This is
		// the same selector logic used by reconcileAutoDiscoverChunkServers.
		pvList := &corev1.PersistentVolumeList{}
		if err := r.List(ctx, pvList); err != nil {
			logger.Error(err, "metrics: listing PVs for autoDiscover count")
		} else {
			selector := ad.PVLabelSelector
			if len(selector) == 0 {
				selector = map[string]string{}
			}
			for _, pv := range pvList.Items {
				if pvMatchesSelector(pv, selector) {
					autoDesired++
				}
			}
		}
	}
	metrics.ChunkServersDesired.WithLabelValues(ns, name, metrics.SourceAutoDiscover).Set(float64(autoDesired))
	metrics.AutoDiscoverPVsMatched.WithLabelValues(ns, name).Set(float64(autoDesired))

	// Total ready chunkservers (status field already aggregates manual + auto).
	metrics.ChunkServersReady.WithLabelValues(ns, name).Set(float64(cluster.Status.ReadyChunkServers))

	// Reconcile errors: increment counter on every failed reconcile so
	// rate() panels show the error frequency.
	if reconcileErr != nil {
		metrics.ReconcileErrorsTotal.WithLabelValues(ns, name).Inc()
	}
}

// countReadyChunkServers returns the number of chunk-server StatefulSets
// (both manually declared and auto-discovered) whose desired replicas
// are all ready. We list by label rather than enumerating spec entries
// so auto-discover STSes are included.
func (r *LeilFSClusterReconciler) countReadyChunkServers(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) int32 {
	stsList := &appsv1.StatefulSetList{}
	if err := r.List(ctx, stsList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":     "leilfs-chunkserver",
			"app.kubernetes.io/instance": cluster.Name,
		},
	); err != nil {
		return 0
	}
	var ready int32
	for i := range stsList.Items {
		sts := &stsList.Items[i]
		if sts.Spec.Replicas != nil && sts.Status.ReadyReplicas >= *sts.Spec.Replicas {
			ready++
		}
	}
	return ready
}

// countTotalChunkServers returns the total number of chunk-server
// StatefulSets currently managed by this cluster (manual + auto).
func (r *LeilFSClusterReconciler) countTotalChunkServers(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) int32 {
	stsList := &appsv1.StatefulSetList{}
	if err := r.List(ctx, stsList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":     "leilfs-chunkserver",
			"app.kubernetes.io/instance": cluster.Name,
		},
	); err != nil {
		return 0
	}
	return int32(len(stsList.Items))
}

// countReadyMetaloggers returns the number of ready metalogger replicas.
func (r *LeilFSClusterReconciler) countReadyMetaloggers(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) int32 {
	if cluster.Spec.Metalogger == nil {
		return 0
	}
	name := fmt.Sprintf("%s-metalogger", cluster.Name)
	sts := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, sts); err != nil {
		return 0
	}
	return sts.Status.ReadyReplicas
}

// ── Master migration: legacy objects → unified StatefulSet ──────────────────

// migrateMasterToStatefulSet is a one-time migration step that removes legacy
// objects (DaemonSet master, Deployment master, shadow StatefulSet, shadow
// headless Service) so the new unified master StatefulSet can take over.
// It also renames the standalone master PVC to match the VolumeClaimTemplate
// naming convention used by the new StatefulSet.
func (r *LeilFSClusterReconciler) migrateMasterToStatefulSet(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	name := fmt.Sprintf("%s-master", cluster.Name)

	// 1. Delete legacy DaemonSet if present.
	ds := &appsv1.DaemonSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, ds); err == nil {
		if err := r.Delete(ctx, ds); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	// 2. Delete legacy master Deployment if present.
	dep := &appsv1.Deployment{}
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, dep); err == nil {
		if err := r.Delete(ctx, dep); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	// 3. Delete legacy shadow StatefulSet and its headless Service if present.
	shadowName := fmt.Sprintf("%s-shadow", cluster.Name)
	shadowSTS := &appsv1.StatefulSet{}
	if err := r.Get(ctx, types.NamespacedName{Name: shadowName, Namespace: cluster.Namespace}, shadowSTS); err == nil {
		if err := r.Delete(ctx, shadowSTS); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	shadowSvc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: shadowName, Namespace: cluster.Namespace}, shadowSvc); err == nil {
		if err := r.Delete(ctx, shadowSvc); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

// ── Goals ConfigMap ──────────────────────────────────────────────────────────

// reconcileGoalsConfigMap creates or updates a ConfigMap that holds the
// generated sfsgoals.cfg file. The file is mounted into the master Deployment
// as a SubPath volume so only /etc/saunafs/sfsgoals.cfg is overridden.
//
// The ConfigMap carries two keys:
//   - sfsgoals.cfg  — mounted automatically by the master Deployment.
//   - sfsmaster-default-goal.txt — informational snippet showing the
//     SFSMASTER_DEFAULT_GOAL line to add to sfsmaster.cfg when Default=true
//     is set on a goal. Not mounted automatically (requires a custom image or
//     init-container to inject into sfsmaster.cfg).
//
// When spec.goals is empty the ConfigMap is deleted (if it existed) and the
// master uses its built-in default goals.
func (r *LeilFSClusterReconciler) reconcileGoalsConfigMap(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	cmName := fmt.Sprintf("%s-master-goals", cluster.Name)

	if len(cluster.Spec.Goals) == 0 {
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: cluster.Namespace}}
		return client.IgnoreNotFound(r.Delete(ctx, cm))
	}

	goalsContent, defaultSnippet := buildGoalsConfig(cluster.Spec.Goals)

	data := map[string]string{"sfsgoals.cfg": goalsContent}
	if defaultSnippet != "" {
		data["sfsmaster-default-goal.txt"] = defaultSnippet
	}

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: cluster.Namespace},
		Data:       data,
	}
	if err := ctrl.SetControllerReference(cluster, cm, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), existing); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return r.Create(ctx, cm)
	}
	existing.Data = cm.Data
	return r.Update(ctx, existing)
}

// buildGoalsConfig renders the sfsgoals.cfg content from a GoalSpec slice and
// returns an optional sfsmaster.cfg snippet for the default goal.
//
// Each line has the format:   <id> <name> : <pattern>
//   - Replication goal (anonymous):    pattern is N space-separated underscores: "_ _ _"
//   - Replication goal (node-pinned):  pattern is space-separated labels: "worker1 worker2 worker3"
//     The { label parts } grouping syntax is only valid for EC goals, NOT for replication.
//   - EC goal:                         pattern is $ec(<dataParts>,<parityParts>).
//     EC with node-pinned parts:       $ec(d,p) { label label … }
func buildGoalsConfig(goals []saunafsv1alpha1.GoalSpec) (goalsFile, masterSnippet string) {
	var sb strings.Builder
	sb.WriteString("# sfsgoals.cfg — generated by leilfs-operator (do not edit manually)\n")
	sb.WriteString("# id  name                  : pattern\n")
	var defaultGoalName string
	for _, g := range goals {
		pattern := ""
		switch {
		case g.EC != nil:
			pattern = fmt.Sprintf("$ec(%d,%d)", g.EC.DataParts, g.EC.ParityParts)
			if len(g.NodeLabels) > 0 {
				// EC with node-pinned parts: $ec(d,p) { label label … }
				pattern = fmt.Sprintf("%s { %s }", pattern, strings.Join(g.NodeLabels, " "))
			}
		case g.Replication != nil:
			if len(g.NodeLabels) > 0 {
				// Node-pinned replication: space-separated labels, one copy per label.
				// e.g.  "worker1 worker2 worker3"
				pattern = strings.Join(g.NodeLabels, " ")
			} else {
				// Anonymous replication: "_ _ _"
				copies := make([]string, *g.Replication)
				for i := range copies {
					copies[i] = "_"
				}
				pattern = strings.Join(copies, " ")
			}
		default:
			// Neither EC nor Replication set — skip invalid entry.
			continue
		}
		marker := ""
		if g.Default {
			marker = "  # ← default"
			defaultGoalName = g.Name
		}
		sb.WriteString(fmt.Sprintf("%-4d %-22s : %s%s\n", g.ID, g.Name, pattern, marker))
	}
	if defaultGoalName != "" {
		masterSnippet = fmt.Sprintf(
			"# Add this line to /etc/saunafs/sfsmaster.cfg to set the default goal:\nSFSMASTER_DEFAULT_GOAL = %s\n",
			defaultGoalName,
		)
	}
	return sb.String(), masterSnippet
}

func masterContainerPorts(cluster *saunafsv1alpha1.LeilFSCluster) []corev1.ContainerPort {
	if len(cluster.Spec.Master.Ports) > 0 {
		ports := make([]corev1.ContainerPort, len(cluster.Spec.Master.Ports))
		for i, p := range cluster.Spec.Master.Ports {
			ports[i] = corev1.ContainerPort{Name: p.Name, ContainerPort: p.ContainerPort}
		}
		return ports
	}
	return []corev1.ContainerPort{
		{Name: "admin", ContainerPort: 9419},
		{Name: "cs", ContainerPort: 9420},
		{Name: "client", ContainerPort: 9421},
	}
}

// masterProbePort picks the container port most indicative that sfsmaster has
// finished loading metadata and is actually accepting connections: prefer the
// "client" port (the one saunafs-client mounts and the master Service target
// primarily forwards), falling back to "admin" if a spec.master.ports override
// omits "client", and finally to the hardcoded default client port if the
// override has neither name.
func masterProbePort(ports []corev1.ContainerPort) int32 {
	for _, p := range ports {
		if p.Name == "client" {
			return p.ContainerPort
		}
	}
	for _, p := range ports {
		if p.Name == "admin" {
			return p.ContainerPort
		}
	}
	return 9421
}

// ── Master Service ──────────────────────────────────────────────────────────

func (r *LeilFSClusterReconciler) reconcileMasterService(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	name := fmt.Sprintf("%s-master", cluster.Name)
	svcType := cluster.Spec.Master.ServiceType
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}

	// When HA is configured, the Service selector tracks the pod that holds the
	// active-master label (set by reconcileMasterHA). This ensures traffic
	// follows the active pod regardless of whether it is the primary master or a
	// promoted shadow.
	//
	// On the very first reconcile (before any election has run), there is no pod
	// with active-master=true yet, so the Service returns no endpoints — which
	// is acceptable because the master pod itself is still starting. The HA
	// reconciler will set the label on the first ready pod within seconds.
	var selector map[string]string
	if cluster.Spec.Shadow != nil {
		selector = map[string]string{
			"app.kubernetes.io/instance": cluster.Name,
			labelActiveMaster:            "true",
		}
	} else {
		selector = map[string]string{
			"app.kubernetes.io/name":     "leilfs-master",
			"app.kubernetes.io/instance": cluster.Name,
		}
	}

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: selector,
			Ports: []corev1.ServicePort{
				{Name: "admin", Port: 9419, TargetPort: intstr.FromInt(9419)},
				{Name: "cs", Port: 9420, TargetPort: intstr.FromInt(9420)},
				{Name: "client", Port: 9421, TargetPort: intstr.FromInt(9421)},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Type = desired.Spec.Type
	return r.Update(ctx, existing)
}

// ── Chunk StatefulSets ──────────────────────────────────────────────────────

func (r *LeilFSClusterReconciler) reconcileChunkServers(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	for i := range cluster.Spec.Chunk.Servers {
		srv := &cluster.Spec.Chunk.Servers[i]
		if err := r.reconcileChunkHeadlessService(ctx, cluster, srv); err != nil {
			return fmt.Errorf("chunk server %s headless service: %w", srv.Name, err)
		}
		if err := r.reconcileChunkHddConfigMap(ctx, cluster, srv); err != nil {
			return fmt.Errorf("chunk server %s hdd configmap: %w", srv.Name, err)
		}
		if err := r.reconcileChunkStatefulSet(ctx, cluster, srv); err != nil {
			return fmt.Errorf("chunk server %s: %w", srv.Name, err)
		}
	}
	return nil
}

// reconcileChunkHeadlessService ensures the Headless Service referenced by
// the chunk StatefulSet's ServiceName field exists. Without it, the StatefulSet
// controller cannot assign stable DNS names to pods and may refuse to start.
func (r *LeilFSClusterReconciler) reconcileChunkHeadlessService(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster, srv *saunafsv1alpha1.ChunkServerSpec) error {
	name := fmt.Sprintf("%s-chunk-%s", cluster.Name, srv.Name)
	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
		},
		Spec: corev1.ServiceSpec{
			// ClusterIP: None makes this a headless service — required for
			// StatefulSet pod identity and stable DNS (podname.svcname.ns.svc…).
			ClusterIP: "None",
			Selector: map[string]string{
				"app.kubernetes.io/name":     "leilfs-chunkserver",
				"app.kubernetes.io/instance": cluster.Name,
				"leilfs.io/chunk-server":     srv.Name,
			},
			Ports: []corev1.ServicePort{
				{Name: "data", Port: 9422, Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// ClusterIP is immutable after creation — only update selector and ports.
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	return r.Update(ctx, existing)
}

// reconcileChunkHddConfigMap creates or updates a ConfigMap containing the
// sfshdd.cfg file for the given chunk server. sfshdd.cfg lists one storage
// path per line; the chunkserver reads it at startup to discover its disks.
// Without this file the chunkserver starts but stores nothing.
func (r *LeilFSClusterReconciler) reconcileChunkHddConfigMap(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster, srv *saunafsv1alpha1.ChunkServerSpec) error {
	cmName := fmt.Sprintf("%s-chunk-%s-hdd", cluster.Name, srv.Name)

	var sb strings.Builder
	sb.WriteString("# sfshdd.cfg — generated by leilfs-operator (do not edit manually)\n")
	for _, mp := range srv.MountPaths {
		sb.WriteString(mp.Path + "\n")
	}
	hddContent := sb.String()

	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: cluster.Namespace,
		},
		Data: map[string]string{"sfshdd.cfg": hddContent},
	}
	if err := ctrl.SetControllerReference(cluster, cm, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cm), existing); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return r.Create(ctx, cm)
	}
	existing.Data = cm.Data
	return r.Update(ctx, existing)
}

func (r *LeilFSClusterReconciler) reconcileChunkStatefulSet(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster, srv *saunafsv1alpha1.ChunkServerSpec) error {
	name := fmt.Sprintf("%s-chunk-%s", cluster.Name, srv.Name)

	image := srv.Image
	if image == "" {
		image = cluster.Spec.Chunk.Image
	}
	if image == "" {
		image = "ghcr.io/henres/leilfs-container/leilfs-chunkserver:5.10.1"
	}

	var replicas int32 = 1

	// Master service DNS for this cluster
	masterHost := fmt.Sprintf("%s-master.%s.svc.cluster.local", cluster.Name, cluster.Namespace)

	// emptyDir shared between init and main container for /etc/saunafs
	cfgVolume := corev1.Volume{
		Name:         "leilfs-cfg",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	cfgMount := corev1.VolumeMount{Name: "leilfs-cfg", MountPath: "/etc/saunafs"}

	dataVolumeMounts, dataVolumes := buildChunkVolumes(srv)
	allVolumes := append(dataVolumes, cfgVolume)
	// Main container mounts: data dirs + cfg dir.
	// NOTE: sfshdd.cfg is intentionally NOT mounted from the ConfigMap here —
	// the chunkserver start script auto-detects /mnt/hdd* paths and writes
	// sfshdd.cfg itself. Mounting a read-only SubPath on that file would cause
	// the script to fail with "Read-only file system". The ConfigMap is kept
	// (reconcileChunkHddConfigMap) for audit/debug purposes only.
	allMounts := append(dataVolumeMounts, cfgMount)

	chunkPorts := chunkContainerPorts(cluster)

	// Pre-create sfschunkserver.cfg with the correct MASTER_HOST (and
	// optionally LABEL). The start script only rewrites the *commented* line
	// ("# MASTER_HOST = sfsmaster") so our already-uncommented value survives.
	initCmd := fmt.Sprintf(
		`mkdir -p /etc/saunafs && `+
			`cp /usr/share/doc/saunafs-chunkserver/examples/sfschunkserver.cfg /etc/saunafs/sfschunkserver.cfg && `+
			`sed -i 's/^# *MASTER_HOST *= *sfsmaster/MASTER_HOST = %s/' /etc/saunafs/sfschunkserver.cfg`,
		masterHost,
	)
	if srv.Label != "" {
		// Append the LABEL line; the example cfg has no LABEL entry so we add
		// it unconditionally (idempotent: sed on a fresh copy each init run).
		initCmd += fmt.Sprintf(
			` && echo 'LABEL = %s' >> /etc/saunafs/sfschunkserver.cfg`,
			srv.Label,
		)
	}

	desired := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":     "leilfs-chunkserver",
				"app.kubernetes.io/instance": cluster.Name,
				"leilfs.io/chunk-server":     srv.Name,
			},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: name,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "leilfs-chunkserver",
					"app.kubernetes.io/instance": cluster.Name,
					"leilfs.io/chunk-server":     srv.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":     "leilfs-chunkserver",
						"app.kubernetes.io/instance": cluster.Name,
						"leilfs.io/chunk-server":     srv.Name,
					},
				},
				Spec: corev1.PodSpec{
					NodeName:    srv.NodeName,
					Tolerations: srv.Tolerations,
					// Each chunk server is pinned to a dedicated node (nodeName).
					// hostNetwork=true makes the chunk server register with the
					// master using the node's routable IP so that external LeilFS
					// clients (when expose.enabled) can reach it directly.
					// ClusterFirstWithHostNet preserves in-cluster DNS.
					HostNetwork: exposeEnabled(cluster),
					DNSPolicy:   chunkDNSPolicy(cluster),
					InitContainers: []corev1.Container{
						{
							Name:            "init-config",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", initCmd},
							VolumeMounts:    []corev1.VolumeMount{cfgMount},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "leilfs-chunkserver",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports:           chunkPorts,
							Resources:       defaultResources(srv.Resources, chunkDefaultResources()),
							VolumeMounts:    allMounts,
						},
					},
					Volumes: allVolumes,
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdateStatefulSet(ctx, desired)
}

func chunkContainerPorts(cluster *saunafsv1alpha1.LeilFSCluster) []corev1.ContainerPort {
	if len(cluster.Spec.Chunk.Ports) > 0 {
		ports := make([]corev1.ContainerPort, len(cluster.Spec.Chunk.Ports))
		for i, p := range cluster.Spec.Chunk.Ports {
			ports[i] = corev1.ContainerPort{Name: p.Name, ContainerPort: p.ContainerPort}
		}
		return ports
	}
	return []corev1.ContainerPort{
		{Name: "data", ContainerPort: 9422},
	}
}

func buildChunkVolumes(srv *saunafsv1alpha1.ChunkServerSpec) ([]corev1.VolumeMount, []corev1.Volume) {
	var mounts []corev1.VolumeMount
	var volumes []corev1.Volume

	for i, mp := range srv.MountPaths {
		volName := fmt.Sprintf("data-%d", i)

		mounts = append(mounts, corev1.VolumeMount{
			Name:      volName,
			MountPath: mp.Path,
		})

		var volSrc corev1.VolumeSource
		switch {
		case mp.HostPath != "":
			volSrc = corev1.VolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: mp.HostPath},
			}
		case mp.ClaimName != "":
			volSrc = corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: mp.ClaimName,
				},
			}
		default:
			// EmptyDir fallback for Kind testing (no real disk)
			volSrc = corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}
		}

		volumes = append(volumes, corev1.Volume{Name: volName, VolumeSource: volSrc})
	}
	return mounts, volumes
}

// ── Auto-Discover Chunk Servers from PVs ─────────────────────────────────────

// reconcileAutoDiscoverChunkServers watches PVs matching the configured label
// selector and creates one chunkserver StatefulSet per discovered PV. Each PV
// gets a dedicated PVC and StatefulSet, mirroring the manual ChunkServerSpec
// pattern but driven entirely by PV existence.
func (r *LeilFSClusterReconciler) reconcileAutoDiscoverChunkServers(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	ad := cluster.Spec.Chunk.AutoDiscover
	if ad == nil || !ad.Enabled {
		return nil
	}

	logger := log.FromContext(ctx)

	// Build label selector for PVs.
	selector := ad.PVLabelSelector
	if len(selector) == 0 {
		selector = map[string]string{}
	}
	// Always require the localdisk-operator managed label.
	if _, ok := selector["localdisk-operator.io/disk"]; !ok {
		// If user didn't specify any label, match all localdisk PVs by checking
		// for the existence of the disk label (list all PVs with that label).
	}

	// List all PVs.
	pvList := &corev1.PersistentVolumeList{}
	if err := r.List(ctx, pvList); err != nil {
		return fmt.Errorf("listing PVs: %w", err)
	}

	// Filter PVs that match the selector labels.
	var matchedPVs []corev1.PersistentVolume
	for _, pv := range pvList.Items {
		if !pvMatchesSelector(pv, selector) {
			continue
		}
		matchedPVs = append(matchedPVs, pv)
	}

	logger.Info("Auto-discover: found matching PVs", "count", len(matchedPVs))

	// For each matched PV, ensure a PVC and chunkserver StatefulSet exists.
	for i := range matchedPVs {
		pv := &matchedPVs[i]

		// Extract node from PV nodeAffinity.
		nodeName := nodeNameFromPV(pv)
		if nodeName == "" {
			logger.Info("Auto-discover: PV has no nodeAffinity, skipping", "pv", pv.Name)
			continue
		}

		// Skip PVs that are in a failed/released state (disk removed).
		if pv.Status.Phase != corev1.VolumeAvailable && pv.Status.Phase != corev1.VolumeBound {
			logger.Info("Auto-discover: PV not available/bound, skipping",
				"pv", pv.Name, "phase", pv.Status.Phase)
			continue
		}

		// Derive a stable chunkserver name from the PV name (which is the disk UUID).
		// Use only the first 8 chars of UUID to keep total name under 63 chars.
		// Format: "ad-<uuid8>" — the "ad" prefix stands for "auto-discover".
		// The full resource name will be "<cluster>-chunk-ad-<uuid8>" which stays
		// well under 63 chars even with long cluster names.
		pvShort := pv.Name
		if len(pvShort) > 8 {
			pvShort = pvShort[:8]
		}
		srvName := fmt.Sprintf("ad-%s", pvShort)

		// Ensure PVC exists for this PV.
		pvcName := fmt.Sprintf("%s-chunk-%s", cluster.Name, srvName)
		if err := r.ensureAutoDiscoverPVC(ctx, cluster, pv, pvcName); err != nil {
			return fmt.Errorf("auto-discover PVC for PV %s: %w", pv.Name, err)
		}

		// Build a synthetic ChunkServerSpec and reconcile it like a manual one.
		// The mount path MUST match the /mnt/hdd* glob that the chunkserver
		// start script (leilfs-chunkserver.start.sh) uses to auto-detect data
		// directories — otherwise sfshdd.cfg ends up empty and the chunkserver
		// reports 0B of storage. Each auto-discovered chunkserver owns exactly
		// one disk, so /mnt/hdd0 is sufficient.
		mountPath := "/mnt/hdd0"
		srv := &saunafsv1alpha1.ChunkServerSpec{
			Name:     srvName,
			NodeName: nodeName,
			Label:    saunafsLabel(nodeName),
			MountPaths: []saunafsv1alpha1.MountPath{
				{
					Path:      mountPath,
					ClaimName: pvcName,
				},
			},
			Tolerations: ad.Tolerations,
			Resources:   ad.Resources,
		}

		if err := r.reconcileChunkHeadlessService(ctx, cluster, srv); err != nil {
			return fmt.Errorf("auto-discover chunk server %s headless service: %w", srvName, err)
		}
		if err := r.reconcileChunkHddConfigMap(ctx, cluster, srv); err != nil {
			return fmt.Errorf("auto-discover chunk server %s hdd configmap: %w", srvName, err)
		}
		if err := r.reconcileChunkStatefulSet(ctx, cluster, srv); err != nil {
			return fmt.Errorf("auto-discover chunk server %s: %w", srvName, err)
		}
	}

	return nil
}

// ensureAutoDiscoverPVC creates a PVC that binds to a specific PV if it doesn't exist.
func (r *LeilFSClusterReconciler) ensureAutoDiscoverPVC(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster, pv *corev1.PersistentVolume, pvcName string) error {
	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: cluster.Namespace}, existing)
	if err == nil {
		return nil // PVC already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	// Determine storage size from PV.
	storage := pv.Spec.Capacity[corev1.ResourceStorage]

	// Empty storageClassName to match PVs with no class.
	emptyClass := ""

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: cluster.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "leilfs-chunkserver",
				"app.kubernetes.io/instance":   cluster.Name,
				"app.kubernetes.io/managed-by": "leilfs-operator",
				"leilfs.io/auto-discover":      "true",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: &emptyClass,
			VolumeName:       pv.Name, // Bind directly to this PV.
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storage,
				},
			},
		},
	}

	if err := ctrl.SetControllerReference(cluster, pvc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pvc)
}

// pvMatchesSelector checks if a PV has all the required labels.
// If a selector value is empty, only the key needs to exist.
func pvMatchesSelector(pv corev1.PersistentVolume, selector map[string]string) bool {
	if len(selector) == 0 {
		// No selector means match all PVs with the localdisk label.
		_, hasLabel := pv.Labels["localdisk-operator.io/disk"]
		return hasLabel
	}
	for k, v := range selector {
		pvVal, ok := pv.Labels[k]
		if !ok {
			return false
		}
		if v != "" && pvVal != v {
			return false
		}
	}
	return true
}

// nodeNameFromPV extracts the node name from a PV's nodeAffinity.
func nodeNameFromPV(pv *corev1.PersistentVolume) string {
	if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil {
		return ""
	}
	for _, term := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms {
		for _, expr := range term.MatchExpressions {
			if expr.Key == "kubernetes.io/hostname" && expr.Operator == corev1.NodeSelectorOpIn && len(expr.Values) > 0 {
				return expr.Values[0]
			}
		}
	}
	return ""
}

// sanitizeName converts a string to a valid Kubernetes name component.
func sanitizeName(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	s = strings.ReplaceAll(s, "_", "-")
	// Remove any characters that are not alphanumeric or hyphens.
	var result []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			result = append(result, c)
		}
	}
	return string(result)
}

// saunafsLabel converts a string to a valid LeilFS LABEL value.
// LeilFS labels must be alphanumeric with underscores only (no hyphens).
func saunafsLabel(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "-", "_")
	s = strings.ReplaceAll(s, " ", "_")
	var result []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			result = append(result, c)
		}
	}
	return string(result)
}

// ── CGI Interface Deployment ─────────────────────────────────────────────────

func (r *LeilFSClusterReconciler) reconcileInterface(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	iface := &cluster.Spec.WebUI
	name := fmt.Sprintf("%s-interface", cluster.Name)

	// When not explicitly enabled, clean up the Deployment/Service if they
	// exist, mirroring the cleanup-on-disable behavior of the other
	// toggle-based sub-reconcilers (reconcileExposeService, reconcileNFS,
	// reconcileMetaloggers).
	if iface.Enabled == nil || !*iface.Enabled {
		for _, obj := range []client.Object{
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace}},
		} {
			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		return nil
	}

	image := iface.Image
	if image == "" {
		image = "ghcr.io/henres/leilfs-container/leilfs-cgiserver:5.10.1"
	}

	port := iface.Port
	if port == 0 {
		port = 9425
	}

	var replicas int32 = 1
	if iface.Replicas != nil {
		replicas = *iface.Replicas
	}

	svcType := iface.ServiceType
	if svcType == "" {
		svcType = corev1.ServiceTypeClusterIP
	}

	labels := map[string]string{
		"app.kubernetes.io/name":     "leilfs-interface",
		"app.kubernetes.io/instance": cluster.Name,
	}

	desired := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: iface.NodeSelector,
					Tolerations:  iface.Tolerations,
					Containers: []corev1.Container{
						{
							Name:            "leilfs-cgiserver",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							// masterhost is passed as a URL query param by the browser;
							// we just need the HTTP server to bind on all interfaces.
							Command: []string{
								"/usr/bin/python3",
								"/usr/sbin/saunafs-cgiserver",
								"-v",
								"-P", fmt.Sprintf("%d", port),
							},
							Ports:     []corev1.ContainerPort{{Name: "http", ContainerPort: port}},
							Resources: defaultResources(iface.Resources, webUIDefaultResources()),
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateDeployment(ctx, desired); err != nil {
		return err
	}

	// Service
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Type:     svcType,
			Ports: []corev1.ServicePort{
				{Name: "http", Port: port, TargetPort: intstr.FromInt(int(port))},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, svc, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}
	existing.Spec.Ports = svc.Spec.Ports
	existing.Spec.Type = svc.Spec.Type
	existing.Spec.Selector = svc.Spec.Selector
	return r.Update(ctx, existing)
}

// ── Expose NodePort Service ──────────────────────────────────────────────────

// reconcileExposeService creates (or deletes) a NodePort Service that lets
// external LeilFS clients connect to the master and mount the filesystem.
// The service targets the master pod on the standard LeilFS client port (9421)
// and, optionally, the admin port (9419).
//
// Usage from a LeilFS client node (replace <node-ip> and <node-port>):
//
//	saunafs-mount -H <node-ip> -P <node-port> /mnt/saunafs
func (r *LeilFSClusterReconciler) reconcileExposeService(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	expose := &cluster.Spec.Expose
	name := fmt.Sprintf("%s-client-expose", cluster.Name)

	// When not explicitly enabled, clean up the service if it exists.
	if expose.Enabled == nil || !*expose.Enabled {
		existing := &corev1.Service{}
		err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, existing)
		if errors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return r.Delete(ctx, existing)
	}

	clientPort := corev1.ServicePort{
		Name:       "client",
		Port:       9421,
		TargetPort: intstr.FromInt(9421),
		Protocol:   corev1.ProtocolTCP,
	}
	if expose.ClientNodePort > 0 {
		clientPort.NodePort = expose.ClientNodePort
	}

	servicePorts := []corev1.ServicePort{clientPort}

	if expose.AdminNodePort > 0 {
		servicePorts = append(servicePorts, corev1.ServicePort{
			Name:       "admin",
			Port:       9419,
			TargetPort: intstr.FromInt(9419),
			Protocol:   corev1.ProtocolTCP,
			NodePort:   expose.AdminNodePort,
		})
	}

	// In HA mode, route only to the active master pod so that external
	// clients never land on a shadow.  In non-HA mode the broad selector
	// (all master pods) is fine because there is only one.
	exposeSelector := map[string]string{
		"app.kubernetes.io/name":     "leilfs-master",
		"app.kubernetes.io/instance": cluster.Name,
	}
	if cluster.Spec.Shadow != nil {
		exposeSelector = map[string]string{
			"app.kubernetes.io/instance": cluster.Name,
			labelActiveMaster:            "true",
		}
	}

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeNodePort,
			Selector: exposeSelector,
			Ports:    servicePorts,
		},
	}
	if err := ctrl.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}

	existing := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec.Ports = desired.Spec.Ports
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Type = desired.Spec.Type
	return r.Update(ctx, existing)
}

// exposeEnabled returns true when the cluster's Expose flag is set.
func exposeEnabled(cluster *saunafsv1alpha1.LeilFSCluster) bool {
	return cluster.Spec.Expose.Enabled != nil && *cluster.Spec.Expose.Enabled
}

// ── NFS-Ganesha Gateway ──────────────────────────────────────────────────────

// reconcileNFS creates (or cleans up) a NFS-Ganesha Deployment and its
// NodePort Service.
//
// Architecture:
//
//	NFS client  ──►  NodePort:2049  ──►  NFS-Ganesha pod
//	                                       ├─ leilfs-client sidecar (FUSE)
//	                                       │    mounts LeilFS → /exports
//	                                       └─ izdock/nfs-ganesha
//	                                            re-exports /exports via VFS FSAL
//
// The leilfs-client sidecar mounts the filesystem via FUSE into a shared
// emptyDir volume (/exports). NFS-Ganesha then exports that local path.
// Both containers are privileged so the FUSE mount is visible across them
// via mountPropagation:Bidirectional.
func (r *LeilFSClusterReconciler) reconcileNFS(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	nfs := &cluster.Spec.NFS
	name := fmt.Sprintf("%s-nfs", cluster.Name)

	// ── cleanup when disabled ────────────────────────────────────────────────
	if nfs.Enabled == nil || !*nfs.Enabled {
		for _, obj := range []client.Object{
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name + "-ganesha-conf", Namespace: cluster.Namespace}},
		} {
			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		return nil
	}

	// ── defaults ─────────────────────────────────────────────────────────────
	ganeshaImage := nfs.Image
	if ganeshaImage == "" {
		ganeshaImage = "ghcr.io/henres/leilfs-operator/nfs-ganesha:latest"
	}
	var replicas int32 = 1
	if nfs.Replicas != nil {
		replicas = *nfs.Replicas
	}
	exportPath := nfs.ExportPath
	if exportPath == "" {
		exportPath = "/"
	}
	squash := nfs.Squash
	if squash == "" {
		squash = "No_Root_Squash"
	}
	svcType := nfs.ServiceType
	if svcType == "" {
		svcType = corev1.ServiceTypeNodePort
	}
	masterHost := fmt.Sprintf("%s-master.%s.svc.cluster.local", cluster.Name, cluster.Namespace)

	// ── ConfigMap: ganesha.conf ──────────────────────────────────────────────
	// Format follows the official LeilFS documentation exactly.
	// Ganesha connects directly to the LeilFS master via the LeilFS FSAL —
	// no local FUSE mount needed.
	// NFS_Core_Param is required so that NFSv4 clients can resolve the pseudo
	// path "/". Without mount_path_pseudo = true the client sees an empty
	// export list and the mount fails.
	ganeshaConf := fmt.Sprintf(`NFS_Core_Param
{
    mount_path_pseudo = true;
    Protocols         = 3, 4;
}

EXPORT
{
    Export_Id            = 1;
    Path                 = "%s";
    Pseudo               = "/";
    Access_Type          = RW;
    Squash               = %s;
    Attr_Expiration_Time = 0;
    Transports           = TCP;
    Protocols            = 3, 4;

    FSAL {
        Name                     = SaunaFS;
        hostname                 = "%s";
        port                     = "9421";
        io_retries               = 5;
        cache_expiration_time_ms = 2500;
    }
}
`, exportPath, squash, masterHost)

	ganeshaConfCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-ganesha-conf",
			Namespace: cluster.Namespace,
		},
		Data: map[string]string{"ganesha.conf": ganeshaConf},
	}
	if err := ctrl.SetControllerReference(cluster, ganeshaConfCM, r.Scheme); err != nil {
		return err
	}
	existingCM := &corev1.ConfigMap{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(ganeshaConfCM), existingCM); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		if err := r.Create(ctx, ganeshaConfCM); err != nil {
			return err
		}
	} else {
		existingCM.Data = ganeshaConfCM.Data
		if err := r.Update(ctx, existingCM); err != nil {
			return err
		}
	}

	privileged := true

	labels := map[string]string{
		"app.kubernetes.io/name":     "leilfs-nfs",
		"app.kubernetes.io/instance": cluster.Name,
	}

	// ── Deployment ───────────────────────────────────────────────────────────
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: nfs.NodeSelector,
					Tolerations:  nfs.Tolerations,
					// Wait for the LeilFS master to accept TCP connections on
					// port 9421 before starting ganesha. Without this guard,
					// ganesha tries to mount the LeilFS filesystem during its
					// startup sequence and, if the master is not yet ready (e.g.
					// after a simultaneous restart), it fails permanently — it
					// does not retry the mount on its own.
					InitContainers: []corev1.Container{
						{
							Name:            "wait-for-master",
							Image:           "busybox:1.36",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{"sh", "-c",
								fmt.Sprintf(
									`until nc -z %s 9421 2>/dev/null; do echo "waiting for leilfs-master:9421..."; sleep 2; done; echo "leilfs-master ready"`,
									masterHost,
								),
							},
						},
					},
					Containers: []corev1.Container{
						{
							// Single container: ganesha.nfsd with the LeilFS
							// FSAL connects directly to the master — no FUSE
							// sidecar needed.
							Name:            "nfs-ganesha",
							Image:           ganeshaImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources:       defaultResources(nfs.Resources, nfsDefaultResources()),
							// Start rpcbind (NFSv3 portmapper) as a background
							// daemon before launching ganesha.nfsd. A brief sleep
							// ensures rpcbind is ready before Ganesha registers its
							// RPCv3 programs. The `|| true` guard prevents a
							// non-zero exit from aborting the shell.
							//
							// `ulimit -n 1048576` raises the per-process NOFILE
							// limit before the daemons start. With the default
							// soft limit (1024), ntirpc occasionally fails to
							// register a transport via epoll_ctl with EBADF when
							// its sentinel/control fd ends up on or above 1024.
							// 1048576 is the value recommended by the
							// nfs-ganesha project (LimitNOFILE in their systemd
							// unit) and matches what most production deployments
							// use.
							Command: []string{"sh", "-c",
								"ulimit -n 1048576; rpcbind -f & sleep 2; ganesha.nfsd -F -f /etc/ganesha/ganesha.conf -N NIV_WARN -L /dev/stdout",
							},
							SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
							Ports: []corev1.ContainerPort{
								{Name: "nfs", ContainerPort: 2049, Protocol: corev1.ProtocolTCP},
								{Name: "rpcbind-tcp", ContainerPort: 111, Protocol: corev1.ProtocolTCP},
								{Name: "rpcbind-udp", ContainerPort: 111, Protocol: corev1.ProtocolUDP},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "run", MountPath: "/run"},
								{Name: "varrun-ganesha", MountPath: "/var/run/ganesha"},
								// Override /etc/ganesha/ganesha.conf with our
								// ConfigMap. Since we own the Dockerfile there
								// is no entrypoint script that would overwrite it.
								{Name: "ganesha-conf", MountPath: "/etc/ganesha/ganesha.conf", SubPath: "ganesha.conf"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name:         "run",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
						{
							// ganesha.nfsd writes its PID file here.
							Name:         "varrun-ganesha",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
						{
							// ganesha.conf with LeilFS FSAL settings,
							// generated from the LeilFSCluster spec.
							Name: "ganesha-conf",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: name + "-ganesha-conf",
									},
								},
							},
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, deployment, r.Scheme); err != nil {
		return err
	}
	if err := r.createOrUpdateDeployment(ctx, deployment); err != nil {
		return err
	}

	// ── Service ──────────────────────────────────────────────────────────────
	nfsPort := corev1.ServicePort{
		Name:       "nfs",
		Port:       2049,
		TargetPort: intstr.FromInt(2049),
		Protocol:   corev1.ProtocolTCP,
	}
	if nfs.NodePort > 0 {
		nfsPort.NodePort = nfs.NodePort
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cluster.Namespace},
		Spec: corev1.ServiceSpec{
			Type:     svcType,
			Selector: labels,
			// Port 2049 for NFS; ports 111 TCP+UDP for rpcbind/portmapper
			// (required by NFSv3 clients and by Ganesha's own NFSv3 support).
			Ports: []corev1.ServicePort{
				nfsPort,
				{Name: "rpcbind-tcp", Port: 111, TargetPort: intstr.FromInt(111), Protocol: corev1.ProtocolTCP},
				{Name: "rpcbind-udp", Port: 111, TargetPort: intstr.FromInt(111), Protocol: corev1.ProtocolUDP},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, svc, r.Scheme); err != nil {
		return err
	}
	existingSvc := &corev1.Service{}
	err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, existingSvc)
	if errors.IsNotFound(err) {
		return r.Create(ctx, svc)
	}
	if err != nil {
		return err
	}
	existingSvc.Spec.Ports = svc.Spec.Ports
	existingSvc.Spec.Selector = svc.Spec.Selector
	existingSvc.Spec.Type = svc.Spec.Type
	return r.Update(ctx, existingSvc)
}

// chunkDNSPolicy returns ClusterFirstWithHostNet when hostNetwork is active
// (expose enabled) so that in-cluster DNS keeps working, and ClusterFirst
// otherwise.
func chunkDNSPolicy(cluster *saunafsv1alpha1.LeilFSCluster) corev1.DNSPolicy {
	if exposeEnabled(cluster) {
		return corev1.DNSClusterFirstWithHostNet
	}
	return corev1.DNSClusterFirst
}

// ── Metaloggers StatefulSet ──────────────────────────────────────────────────

// reconcileMetaloggers creates or updates the metalogger StatefulSet (and its
// Headless Service). When spec.metalogger is nil or replicas == 0 both objects
// are deleted if they exist.
//
// Each metalogger replica:
//   - Runs sfsmetal, which connects to the master on port 9419 and downloads
//     a continuous stream of metadata changes.
//   - Persists the journal to a dedicated PVC so it can resume after a restart.
//
// The headless service gives each replica a stable DNS name:
//
//	<cluster>-metalogger-<i>.<cluster>-metalogger.<ns>.svc.cluster.local
func (r *LeilFSClusterReconciler) reconcileMetaloggers(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	ml := cluster.Spec.Metalogger
	stsName := fmt.Sprintf("%s-metalogger", cluster.Name)
	svcName := stsName

	// ── cleanup when disabled ────────────────────────────────────────────────
	wantReplicas := int32(0)
	if ml != nil && ml.Replicas != nil {
		wantReplicas = *ml.Replicas
	}
	if ml == nil || wantReplicas == 0 {
		for _, obj := range []client.Object{
			&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: cluster.Namespace}},
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: cluster.Namespace}},
		} {
			if err := r.Delete(ctx, obj); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
		return nil
	}

	// ── defaults ─────────────────────────────────────────────────────────────
	image := ml.Image
	if image == "" {
		image = "ghcr.io/henres/leilfs-container/leilfs-metalogger:5.10.1"
	}

	storageSize := resource.MustParse("5Gi")
	var storageClassName *string
	if ml.MetadataStorage != nil {
		if !ml.MetadataStorage.Size.IsZero() {
			storageSize = ml.MetadataStorage.Size
		}
		if ml.MetadataStorage.StorageClassName != "" {
			sc := ml.MetadataStorage.StorageClassName
			storageClassName = &sc
		}
	}

	masterHost := fmt.Sprintf("%s-master.%s.svc.cluster.local", cluster.Name, cluster.Namespace)

	// ── Headless Service ─────────────────────────────────────────────────────
	labels := map[string]string{
		"app.kubernetes.io/name":      "leilfs-metalogger",
		"app.kubernetes.io/instance":  cluster.Name,
		"app.kubernetes.io/component": "metalogger",
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: cluster.Namespace},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector:  labels,
			Ports: []corev1.ServicePort{
				// sfsmetal communicates with the master, not the other way around.
				// The headless service exists purely for stable pod DNS.
				{Name: "metalogger", Port: 9419, Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, svc, r.Scheme); err != nil {
		return err
	}
	existingSvc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: svc.Name, Namespace: svc.Namespace}, existingSvc); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		if err := r.Create(ctx, svc); err != nil {
			return err
		}
	} else {
		existingSvc.Spec.Selector = svc.Spec.Selector
		existingSvc.Spec.Ports = svc.Spec.Ports
		if err := r.Update(ctx, existingSvc); err != nil {
			return err
		}
	}

	// ── init command ─────────────────────────────────────────────────────────
	// Seed /etc/saunafs with the packaged defaults and set MASTER_HOST.
	// sfsmetal reads /etc/saunafs/sfsmetalogger.cfg (or sfsmetal.cfg depending
	// on distro packaging); we patch whatever the example provides.
	initCmd := fmt.Sprintf(
		`mkdir -p /etc/saunafs /var/lib/saunafs && `+
			`cp /usr/share/doc/saunafs-metalogger/examples/sfsmetalogger.cfg /etc/saunafs/sfsmetalogger.cfg 2>/dev/null || `+
			`cp /usr/share/doc/saunafs-metalogger/examples/sfsmetal.cfg /etc/saunafs/sfsmetalogger.cfg && `+
			`sed -i 's/^# *MASTER_HOST *= *sfsmaster/MASTER_HOST = %s/' /etc/saunafs/sfsmetalogger.cfg`,
		masterHost,
	)

	// emptyDir for /etc/saunafs (written by init-container, read by main).
	cfgVolume := corev1.Volume{
		Name:         "leilfs-cfg",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	cfgMount := corev1.VolumeMount{Name: "leilfs-cfg", MountPath: "/etc/saunafs"}

	// data volume mount (backed by PVC template below).
	dataMount := corev1.VolumeMount{Name: "metalogger-data", MountPath: "/var/lib/saunafs"}

	// ── StatefulSet ──────────────────────────────────────────────────────────
	desired := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: cluster.Namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &wantReplicas,
			ServiceName: svcName,
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					NodeSelector: ml.NodeSelector,
					Tolerations:  ml.Tolerations,
					Volumes:      []corev1.Volume{cfgVolume},
					InitContainers: []corev1.Container{
						{
							Name:            "init-config",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", initCmd},
							VolumeMounts:    []corev1.VolumeMount{cfgMount},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "leilfs-metalogger",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources:       defaultResources(ml.Resources, metaloggerDefaultResources()),
							VolumeMounts:    []corev1.VolumeMount{cfgMount, dataMount},
						},
					},
				},
			},
			// Each replica gets its own PVC for the metadata journal.
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "metalogger-data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: storageClassName,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: storageSize,
							},
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdateStatefulSet(ctx, desired)
}

// ── helpers ─────────────────────────────────────────────────────────────────

func (r *LeilFSClusterReconciler) createOrUpdateStatefulSet(ctx context.Context, desired *appsv1.StatefulSet) error {
	existing := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.Replicas = desired.Spec.Replicas
	// Merge labels from desired onto existing so newly added labels (e.g.
	// the chunk-server discovery labels) propagate to pre-existing STSes.
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for k, v := range desired.Labels {
		existing.Labels[k] = v
	}
	return r.Update(ctx, existing)
}

func (r *LeilFSClusterReconciler) createOrUpdateDeployment(ctx context.Context, desired *appsv1.Deployment) error {
	existing := &appsv1.Deployment{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec.Template = desired.Spec.Template
	existing.Spec.Replicas = desired.Spec.Replicas
	return r.Update(ctx, existing)
}

// ── Unified master StatefulSet ───────────────────────────────────────────────

// reconcileMasterStatefulSet creates or updates a single StatefulSet that runs
// ALL master-role pods: pod-0 is the primary master, pods 1..N are shadows.
//
// Total replicas = 1 (primary) + spec.shadow.Replicas (shadows).
// When spec.shadow is nil, replicas = 1 (primary only, no HA).
//
// Each pod's init-container is ordinal-aware:
//   - pod-0 → copies example configs, stays as primary master (default PERSONALITY)
//   - pod-N → copies example configs, patches PERSONALITY=shadow + MASTER_HOST
//
// The main container runs a wrapper script that checks for a .promote sentinel
// file on the PVC. If found: patches the config to master, removes the sentinel,
// restarts as primary. This is how shadows are promoted during failover.
//
// PVC: VolumeClaimTemplate "master-data" → one PVC per pod, named
//
//	master-data-<cluster>-master-<ordinal>
//
// IMPORTANT: PVCs created by VolumeClaimTemplates are NOT deleted when the
// StatefulSet is deleted — they survive CR deletion just like the old
// standalone PVC. This is the intended behaviour for metadata persistence.
func (r *LeilFSClusterReconciler) reconcileMasterStatefulSet(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	stsName := fmt.Sprintf("%s-master", cluster.Name)
	hlSvcName := fmt.Sprintf("%s-master-hl", cluster.Name)

	image := cluster.Spec.Master.Image
	if image == "" {
		image = defaultMasterImage
	}

	// Total replicas: 1 primary + N shadows.
	var shadowReplicas int32
	if cluster.Spec.Shadow != nil && cluster.Spec.Shadow.Replicas != nil {
		shadowReplicas = *cluster.Spec.Shadow.Replicas
	}
	totalReplicas := int32(1) + shadowReplicas

	// Storage size & class: master spec takes priority; shadow spec provides
	// fallback defaults.  This avoids the shadow config silently overriding
	// the master's storage class for pod-0.  Because a StatefulSet has a
	// single VolumeClaimTemplate shared by all pods, both master and shadow
	// pods will use the same storage class — choose the master's preference.
	storageSize := resource.MustParse("1Gi")
	var storageClassName *string
	if sh := cluster.Spec.Shadow; sh != nil && sh.MetadataStorage != nil {
		if !sh.MetadataStorage.Size.IsZero() {
			storageSize = sh.MetadataStorage.Size
		}
		if sh.MetadataStorage.StorageClassName != "" {
			sc := sh.MetadataStorage.StorageClassName
			storageClassName = &sc
		}
	}
	// Master spec overrides shadow spec.
	if ms := cluster.Spec.Master.MetadataStorage; ms != nil {
		if !ms.Size.IsZero() {
			storageSize = ms.Size
		}
		if ms.StorageClassName != "" {
			sc := ms.StorageClassName
			storageClassName = &sc
		}
	}

	// The headless service DNS used by shadow pods to connect to the active master.
	// This always points to the active master ClusterIP service.
	masterHost := fmt.Sprintf("%s-master.%s.svc.cluster.local", cluster.Name, cluster.Namespace)

	// ── Headless Service (required by StatefulSet for stable DNS) ────────────
	masterLabels := map[string]string{
		"app.kubernetes.io/name":     "leilfs-master",
		"app.kubernetes.io/instance": cluster.Name,
	}
	hlSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: hlSvcName, Namespace: cluster.Namespace},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector:  masterLabels,
			Ports: []corev1.ServicePort{
				{Name: "admin", Port: 9419, Protocol: corev1.ProtocolTCP},
				{Name: "cs", Port: 9420, Protocol: corev1.ProtocolTCP},
				{Name: "client", Port: 9421, Protocol: corev1.ProtocolTCP},
				// Exposed by the optional leilfs-exporter sidecar.
				// Always declared so PodMonitors can target it
				// uniformly even when the sidecar is disabled
				// (the port simply has no listener in that case).
				{Name: "metrics", Port: 9418, Protocol: corev1.ProtocolTCP},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, hlSvc, r.Scheme); err != nil {
		return err
	}
	existingHlSvc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: hlSvcName, Namespace: cluster.Namespace}, existingHlSvc); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		if err := r.Create(ctx, hlSvc); err != nil {
			return err
		}
	} else {
		existingHlSvc.Spec.Selector = hlSvc.Spec.Selector
		existingHlSvc.Spec.Ports = hlSvc.Spec.Ports
		if err := r.Update(ctx, existingHlSvc); err != nil {
			return err
		}
	}

	// ── init-config command ───────────────────────────────────────────────────
	// Each pod reads the HA Lease to determine whether it should start as master
	// or shadow.  If it holds the Lease (holderIdentity == pod name) it starts
	// as master; otherwise it starts as shadow pointing at the master Service.
	// On the very first bootstrap (empty Lease / no holder) every pod defaults
	// to shadow — the sidecar of the first pod to acquire the Lease will kill
	// sfsmaster so it restarts as master via this init-container logic.
	goalsCmd := ""
	goalsVolume := []corev1.Volume{}
	goalsMounts := []corev1.VolumeMount{}
	if len(cluster.Spec.Goals) > 0 {
		cmName := fmt.Sprintf("%s-master-goals", cluster.Name)
		goalsVolume = append(goalsVolume, corev1.Volume{
			Name: "leilfs-goals",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
				},
			},
		})
		goalsMounts = append(goalsMounts, corev1.VolumeMount{
			Name:      "leilfs-goals",
			MountPath: "/etc/leilfs-goals",
			ReadOnly:  true,
		})
		goalsCmd = `[ -f /etc/leilfs-goals/sfsgoals.cfg ] && cp /etc/leilfs-goals/sfsgoals.cfg /etc/saunafs/sfsgoals.cfg
`
	}

	// ── Default goal (SFSMASTER_DEFAULT_GOAL) ─────────────────────────────────
	// reconcileGoalsConfigMap only ever wrote an *informational* snippet
	// (sfsmaster-default-goal.txt) describing this line — nothing actually
	// added it to the sfsmaster.cfg the master reads. Thread the default
	// goal's name (if any) into the init-config script using the same
	// idempotent sed/grep pattern as PERSONALITY/MASTER_HOST below. At most
	// one goal should have Default=true; the first one found wins. When no
	// goal is marked default, defaultGoalCmd stays empty and no line is added
	// (preserving the master's built-in default goal).
	var defaultGoalCmd string
	for _, g := range cluster.Spec.Goals {
		if g.Default {
			defaultGoalCmd = fmt.Sprintf(`
# ── Apply default goal ───────────────────────────────────────────────────
sed -i 's/^# *SFSMASTER_DEFAULT_GOAL *= *.*/SFSMASTER_DEFAULT_GOAL = %s/' /etc/saunafs/sfsmaster.cfg
grep -q "^SFSMASTER_DEFAULT_GOAL" /etc/saunafs/sfsmaster.cfg || echo "SFSMASTER_DEFAULT_GOAL = %s" >> /etc/saunafs/sfsmaster.cfg`,
				g.Name, g.Name)
			break
		}
	}

	leaseName := fmt.Sprintf("%s-master-ha", cluster.Name)
	saName := fmt.Sprintf("%s-master", cluster.Name)

	const defaultStartupGrace = 30 * time.Second
	startupGrace := defaultStartupGrace
	if cluster.Spec.Master.StartupGracePeriod != nil {
		startupGrace = cluster.Spec.Master.StartupGracePeriod.Duration
	}

	// init-config: read Lease → decide master or shadow personality.
	// Uses wget (available in the leilfs-master image) to call the kube API.
	// Falls back to shadow if HA is not configured (no Lease name injected).
	initConfigCmd := fmt.Sprintf(`mkdir -p /etc/saunafs /var/lib/saunafs
cp -r /usr/share/doc/saunafs-master/examples/. /etc/saunafs/
%s
# ── Determine personality via HA Lease ───────────────────────────────────────
LEASE_NAME="%s"
MY_POD_NAME="${POD_NAME}"
TOKEN=$(cat /var/run/secrets/kubernetes.io/serviceaccount/token)
CA=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
NS=$(cat /var/run/secrets/kubernetes.io/serviceaccount/namespace)
APISERVER=https://kubernetes.default.svc
LEASE_URL="${APISERVER}/apis/coordination.k8s.io/v1/namespaces/${NS}/leases/${LEASE_NAME}"

HOLDER=""
LEASE_JSON=$(wget --ca-certificate=${CA} \
  --header="Authorization: Bearer ${TOKEN}" \
  -qO- "${LEASE_URL}" 2>/dev/null) && true
if [ -n "${LEASE_JSON}" ]; then
  # Extract holderIdentity: API returns "key": "value" (space after colon).
  # The managedFields section also contains "f:holderIdentity":{} (no value),
  # which the pattern correctly ignores because it has no quoted string value.
  HOLDER=$(printf '%%s' "${LEASE_JSON}" | sed 's/.*"holderIdentity": *"\([^"]*\)".*/\1/;t;s/.*//' | grep -v '^$' | head -1)
fi

if [ "${HOLDER}" = "${MY_POD_NAME}" ]; then
  echo "[ha] I am the Lease holder — starting as master"
  sed -i 's/^# *PERSONALITY *= *.*/PERSONALITY = master/' /etc/saunafs/sfsmaster.cfg
  grep -q "^PERSONALITY" /etc/saunafs/sfsmaster.cfg || echo "PERSONALITY = master" >> /etc/saunafs/sfsmaster.cfg
  sed -i '/^MASTER_HOST/d' /etc/saunafs/sfsmaster.cfg
  sed -i '/^MASTER_PORT/d' /etc/saunafs/sfsmaster.cfg
  sed -i '/^MASTER_RECONNECTION_DELAY/d' /etc/saunafs/sfsmaster.cfg
  sed -i '/^MASTER_TIMEOUT/d' /etc/saunafs/sfsmaster.cfg
else
  echo "[ha] Not the Lease holder (holder='${HOLDER}') — starting as shadow"
  sed -i 's/^# *PERSONALITY *= *.*/PERSONALITY = shadow/' /etc/saunafs/sfsmaster.cfg
  grep -q "^PERSONALITY" /etc/saunafs/sfsmaster.cfg || echo "PERSONALITY = shadow" >> /etc/saunafs/sfsmaster.cfg
  sed -i 's/^# *MASTER_HOST *= *sfsmaster/MASTER_HOST = %s/' /etc/saunafs/sfsmaster.cfg
  grep -q "^MASTER_HOST" /etc/saunafs/sfsmaster.cfg || echo "MASTER_HOST = %s" >> /etc/saunafs/sfsmaster.cfg
  grep -q "^MASTER_PORT" /etc/saunafs/sfsmaster.cfg || echo "MASTER_PORT = 9419" >> /etc/saunafs/sfsmaster.cfg
  grep -q "^MASTER_RECONNECTION_DELAY" /etc/saunafs/sfsmaster.cfg || echo "MASTER_RECONNECTION_DELAY = 5" >> /etc/saunafs/sfsmaster.cfg
  grep -q "^MASTER_TIMEOUT" /etc/saunafs/sfsmaster.cfg || echo "MASTER_TIMEOUT = 60" >> /etc/saunafs/sfsmaster.cfg
fi
%s`,
		goalsCmd, leaseName, masterHost, masterHost, defaultGoalCmd)

	initMetaCmd := "cp -n /opt/saunafs/templates/metadata.sfs.empty /var/lib/saunafs/metadata.sfs 2>/dev/null || true"

	// ── Main container wrapper script ─────────────────────────────────────────
	// Simpler than before: personality is already set by the init-container.
	// We only remove the lock file (safe on both master and shadow restarts).
	masterRunCmd := `
# Always remove the lock file before starting.
rm -f /var/lib/saunafs/metadata.sfs.lock
# Remove leftover temporary metadata file from a previous unclean shutdown
# (e.g. OOM-kill, node reboot). Without this, sfsmaster refuses to start with:
#   "temporary metadata file (metadata.sfs.tmp) exists, metadata directory is in dirty state"
rm -f /var/lib/saunafs/metadata.sfs.tmp
exec /leilfs-master.start.sh
`

	// ── Sidecar script ────────────────────────────────────────────────────────
	// Two modes depending on whether this pod holds the Lease:
	//   - Holder: renew every leaseRenewInterval. If renewal fails or another
	//     pod has taken over, kill sfsmaster → pod restarts → init-container
	//     re-reads Lease → starts as shadow.
	//   - Non-holder (shadow): poll every 5s. If Lease is expired, attempt an
	//     atomic compare-and-swap PATCH (using resourceVersion). On success,
	//     kill sfsmaster → pod restarts → init-container → starts as master.
	sidecarCmd := fmt.Sprintf(`
CA=/var/run/secrets/kubernetes.io/serviceaccount/ca.crt
NS=$(cat /var/run/secrets/kubernetes.io/serviceaccount/namespace)
APISERVER=https://kubernetes.default.svc
LEASE_NAME="%s"
MY_POD="${POD_NAME}"
LEASE_URL="${APISERVER}/apis/coordination.k8s.io/v1/namespaces/${NS}/leases/${LEASE_NAME}"
RENEW_INTERVAL=%d
OBSERVE_INTERVAL=%d
LEASE_DURATION=%d
STARTUP_GRACE=%d
TOKEN_FILE=/var/run/secrets/kubernetes.io/serviceaccount/token

# Helper: GET Lease JSON
# Token is re-read on every call: projected SA tokens rotate every ~1h.
get_lease() {
  wget --ca-certificate=${CA} \
    --header="Authorization: Bearer $(cat ${TOKEN_FILE})" \
    -qO- "${LEASE_URL}" 2>/dev/null
}

# Helper: PATCH Lease (merge-patch)
patch_lease() {
  wget --ca-certificate=${CA} \
    --header="Authorization: Bearer $(cat ${TOKEN_FILE})" \
    --header="Content-Type: application/merge-patch+json" \
    --method=PATCH \
    --body-data="$1" \
    -qO- "${LEASE_URL}" 2>/dev/null
}

# Helper: extract string field from JSON (no jq, pure sed)
# Handles both "key":"value" and "key": "value" (with optional space).
# sed processes line-by-line; we filter blank lines and take the first match.
json_field() {
  printf '%%s' "$1" | sed "s/.*\"$2\": *\"\([^\"]*\)\".*/\1/;t;s/.*//" | grep -v "^$" | head -1
}
json_int() {
  printf '%%s' "$1" | sed "s/.*\"$2\": *\([0-9]*\).*/\1/;t;s/.*//" | grep -v "^$" | head -1
}

# Helper: delete this pod (triggers full restart including init-containers)
delete_self() {
  POD_URL="${APISERVER}/api/v1/namespaces/${NS}/pods/${MY_POD}"
  wget --ca-certificate=${CA} \
    --header="Authorization: Bearer $(cat ${TOKEN_FILE})" \
    --method=DELETE \
    -qO- "${POD_URL}" 2>/dev/null || true
  # If delete succeeded the pod will be evicted; sleep to avoid tight loop
  sleep 60
}

echo "[ha-sidecar] starting, pod=${MY_POD}, startup_grace=${STARTUP_GRACE}s"

# Record start time. The startup grace period suppresses the sfsmaster
# health check (pgrep) for STARTUP_GRACE seconds — the master can take
# tens of seconds to load metadata on large filesystems. The Lease
# renewal loop still runs from t=0; otherwise the Lease would expire
# (leaseDurationSeconds=30) before the grace period ends and a shadow
# would steal it, causing a flapping election.
START_EPOCH=$(date -u +%%s)

while true; do
  LEASE=$(get_lease)
  if [ -z "${LEASE}" ]; then
    echo "[ha-sidecar] could not read Lease, sleeping"
    sleep ${OBSERVE_INTERVAL}
    continue
  fi

  HOLDER=$(json_field "${LEASE}" "holderIdentity")
  RES_VER=$(json_field "${LEASE}" "resourceVersion")

  if [ "${HOLDER}" = "${MY_POD}" ]; then
    # ── I am the holder: check sfsmaster is alive, then renew ───────────────
    NOW_EPOCH=$(date -u +%%s)
    IN_GRACE=0
    if [ $((NOW_EPOCH - START_EPOCH)) -lt ${STARTUP_GRACE} ]; then
      IN_GRACE=1
    fi
    if [ ${IN_GRACE} -eq 0 ] && ! pgrep -x sfsmaster > /dev/null 2>&1; then
      echo "[ha-sidecar] sfsmaster not running, releasing Lease so a shadow can take over"
      # Release: PATCH holderIdentity to empty so shadows see an expired lease
      NOW=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%S.000000Z)
      RELEASE_BODY="{\"spec\":{\"holderIdentity\":\"\",\"renewTime\":\"${NOW}\",\"leaseDurationSeconds\":${LEASE_DURATION}}}"
      patch_lease "${RELEASE_BODY}" > /dev/null
      delete_self
      continue
    fi
    NOW=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%S.000000Z)
    BODY="{\"spec\":{\"holderIdentity\":\"${MY_POD}\",\"renewTime\":\"${NOW}\",\"leaseDurationSeconds\":${LEASE_DURATION}}}"
    RESULT=$(patch_lease "${BODY}")
    NEW_HOLDER=$(json_field "${RESULT}" "holderIdentity")
    if [ "${NEW_HOLDER}" != "${MY_POD}" ]; then
      echo "[ha-sidecar] lost Lease (holder is now '${NEW_HOLDER}'), deleting pod to restart as shadow"
      delete_self
    fi
    sleep ${RENEW_INTERVAL}
  else
    # ── I am a shadow: watch for Lease expiry ───────────────────────────────
    RENEW_TIME=$(json_field "${LEASE}" "renewTime")
    if [ -n "${RENEW_TIME}" ]; then
      # Convert renewTime to epoch seconds (busybox date supports this format)
      RENEW_EPOCH=$(date -u -d "${RENEW_TIME}" +%%s 2>/dev/null || echo 0)
      NOW_EPOCH=$(date -u +%%s)
      AGE=$((NOW_EPOCH - RENEW_EPOCH))
      if [ ${AGE} -lt ${LEASE_DURATION} ]; then
        # Lease is fresh — nothing to do
        sleep ${OBSERVE_INTERVAL}
        continue
      fi
      echo "[ha-sidecar] Lease expired (age=${AGE}s), attempting acquisition"
    else
      # No renewTime → brand-new empty Lease — try to acquire
      echo "[ha-sidecar] Lease has no renewTime, attempting acquisition"
    fi

    # ── Bounded staleness heuristic before acquisition ──────────────────────
    # KNOWN, DOCUMENTED TRADE-OFF (ROADMAP.md "Durcissement opérateur" / ADR-0001):
    # the CAS below is still first-come-first-served — whichever shadow's PATCH
    # lands first with a matching resourceVersion wins, full stop. This block
    # does NOT change that and does NOT add a second coordination mechanism.
    # It only delays THIS shadow's own CAS attempt when its local metadata
    # looks stale, giving a fresher shadow a head start to win the race.
    #
    # What it does NOT guarantee:
    #   - It does NOT guarantee the most up-to-date shadow is promoted. If all
    #     shadows are equally stale (or equally fresh), or if the delay below
    #     races against another shadow's delayed attempt, an arbitrary one
    #     still wins — exactly as before this change.
    #   - It can misfire on an idle filesystem: if no metadata operations have
    #     happened recently, changelog.sfs looks "stale" on every shadow even
    #     though all of them are perfectly in sync. In that case every shadow
    #     computes a similar delay and the relative ordering this heuristic
    #     relies on degenerates to roughly no signal — it does not make things
    #     worse than today, it just fails to help.
    #   - It only ever delays the CAS *attempt*; it never blocks, weakens, or
    #     bypasses the atomic resourceVersion CAS itself. If a fresher shadow
    #     (or one with a shorter/no delay) wins during our sleep, our stale
    #     RES_VER simply fails to match server-side and this shadow's PATCH is
    #     rejected — handled by the existing "acquisition lost race" branch
    #     below, no new failure mode introduced.
    #
    # Signal: mtime of /var/lib/saunafs/changelog.sfs. SaunaFS appends to this
    # file continuously as the shadow receives metadata changes streamed from
    # the active master ("immediately updated incremental logs" per SaunaFS
    # docs), so a well-connected shadow's changelog.sfs is touched far more
    # recently than one that has been lagging or disconnected. Requires the
    # ha-sidecar container to read-only-mount the master-data volume (see
    # VolumeMounts above) — without that mount, stat always fails and this
    # falls through to the ordinal fallback below on every attempt.
    STALE_DELAY=0
    CHANGELOG_MTIME=$(stat -c %%Y /var/lib/saunafs/changelog.sfs 2>/dev/null || echo 0)
    if [ "${CHANGELOG_MTIME}" != "0" ]; then
      CHANGELOG_AGE=$(( $(date -u +%%s) - CHANGELOG_MTIME ))
      if [ ${CHANGELOG_AGE} -gt ${LEASE_DURATION} ]; then
        # Only penalize staleness *beyond* what a silent (dead) master alone
        # would already explain, and halve it so the delay is a nudge, not a
        # second failover timer.
        STALE_DELAY=$(( (CHANGELOG_AGE - LEASE_DURATION) / 2 ))
      fi
    else
      # No local changelog observed (e.g. very first bootstrap, before any
      # metadata write has ever happened). This is NOT a staleness signal —
      # it is a crude, deterministic first-mover tie-breaker only, per
      # ROADMAP.md's explicit fallback: jitter proportional to the pod's
      # StatefulSet ordinal, so shadows don't all attempt the CAS in lockstep.
      ORDINAL=$(printf '%%s' "${MY_POD}" | sed 's/.*-\([0-9][0-9]*\)$/\1/')
      case "${ORDINAL}" in ''|*[!0-9]*) ORDINAL=0 ;; esac
      STALE_DELAY=${ORDINAL}
    fi
    # Cap: this is a bounded nudge, not a new source of failover latency —
    # 15s keeps the worst-case extra delay well under a second full Lease
    # duration (30s).
    [ ${STALE_DELAY} -gt 15 ] && STALE_DELAY=15
    if [ ${STALE_DELAY} -gt 0 ]; then
      echo "[ha-sidecar] backing off ${STALE_DELAY}s before acquisition attempt (staleness heuristic, changelog_age=${CHANGELOG_AGE:-n/a}s)"
      sleep ${STALE_DELAY}
    fi

    # Atomic CAS: PATCH only if resourceVersion matches
    NOW=$(date -u +%%Y-%%m-%%dT%%H:%%M:%%S.000000Z)
    CAS_BODY="{\"metadata\":{\"resourceVersion\":\"${RES_VER}\"},\"spec\":{\"holderIdentity\":\"${MY_POD}\",\"acquireTime\":\"${NOW}\",\"renewTime\":\"${NOW}\",\"leaseDurationSeconds\":${LEASE_DURATION}}}"
    CAS_RESULT=$(patch_lease "${CAS_BODY}")
    NEW_HOLDER=$(json_field "${CAS_RESULT}" "holderIdentity")
    if [ "${NEW_HOLDER}" = "${MY_POD}" ]; then
      echo "[ha-sidecar] acquired Lease! deleting pod to restart as master"
      delete_self
    else
      echo "[ha-sidecar] acquisition lost race (holder='${NEW_HOLDER}'), continuing as shadow"
    fi
    sleep ${OBSERVE_INTERVAL}
  fi
done
`,
		leaseName,
		int(leaseRenewInterval.Seconds()),
		int(leaseObserveInterval.Seconds()),
		int(leaseDuration.Seconds()),
		int(startupGrace.Seconds()),
	)

	cfgVolume := corev1.Volume{
		Name:         "leilfs-cfg",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	cfgMount := corev1.VolumeMount{Name: "leilfs-cfg", MountPath: "/etc/saunafs"}
	dataMount := corev1.VolumeMount{Name: "master-data", MountPath: "/var/lib/saunafs"}

	podLabels := map[string]string{
		"app.kubernetes.io/name":     "leilfs-master",
		"app.kubernetes.io/instance": cluster.Name,
	}

	initConfigVolMounts := append([]corev1.VolumeMount{cfgMount, dataMount}, goalsMounts...)
	volumes := append([]corev1.Volume{cfgVolume}, goalsVolume...)

	ports := masterContainerPorts(cluster)
	probePort := masterProbePort(ports)

	// Readiness/liveness probes: a bare TCP connect to the client port is
	// enough to detect "sfsmaster hasn't finished loading metadata and isn't
	// listening yet" without requiring new tooling in the image. Before this,
	// nothing gated Service traffic on the master actually being up, so a pod
	// could receive requests immediately after container start.
	//
	// Readiness is deliberately more lenient than liveness (higher
	// FailureThreshold, no InitialDelaySeconds) since a failing readiness
	// probe only pulls the pod out of Service endpoints — cheap to retry
	// often. Liveness reuses startupGrace (the same grace period the
	// ha-sidecar already gives sfsmaster before it starts pgrep-checking it)
	// as its InitialDelaySeconds, and a low FailureThreshold, because a
	// failing liveness probe kills the container — restarting a master that
	// is still legitimately loading a large metadata file would be
	// counter-productive.
	readinessProbe := &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(int(probePort))}},
		InitialDelaySeconds: 5,
		PeriodSeconds:       5,
		TimeoutSeconds:      3,
		FailureThreshold:    6,
	}
	livenessProbe := &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt(int(probePort))}},
		InitialDelaySeconds: int32(startupGrace.Seconds()),
		PeriodSeconds:       10,
		TimeoutSeconds:      3,
		FailureThreshold:    3,
	}

	// Resources: use master resources for all pods in the StatefulSet.
	// Shadow resources (if different) would require per-ordinal overrides —
	// not yet supported; master spec governs all pods.
	// Apply built-in defaults when the user has not set any requests/limits.
	resources := defaultResources(cluster.Spec.Master.Resources, masterDefaultResources())

	// POD_NAME env var is injected via the downward API so both init-containers
	// and the sidecar know their own pod name without kubectl.
	podNameEnv := corev1.EnvVar{
		Name: "POD_NAME",
		ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"},
		},
	}

	// Only inject sidecar + RBAC machinery when HA (shadow) is configured.
	var sidecarContainers []corev1.Container
	if cluster.Spec.Shadow != nil {
		sidecarContainers = append(sidecarContainers, corev1.Container{
			Name:            "ha-sidecar",
			Image:           image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Command:         []string{"sh", "-c", sidecarCmd},
			Env:             []corev1.EnvVar{podNameEnv},
			Resources:       sidecarDefaultResources(),
			// Read-only mount of the same PVC the main container uses for
			// /var/lib/saunafs, so the shadow-acquisition staleness heuristic
			// below can stat() the local changelog file. Read-only: the
			// sidecar must never be able to write/corrupt master metadata.
			VolumeMounts: []corev1.VolumeMount{
				{Name: "master-data", MountPath: "/var/lib/saunafs", ReadOnly: true},
			},
		})
	}

	// Optional leilfs-exporter sidecar. Enabled by default on every
	// master Pod (one per replica); set `master.exporter.enabled: false`
	// to opt out. Only the active master returns useful series — the
	// shadow rejects most admin queries and reports an empty metadata
	// view. Filtering is delegated to the PodMonitor via the
	// `leilfs.io/active-master=true` label, so we keep the same PodSpec
	// on every replica and let Prometheus drop scrapes whose target
	// lacks that label.
	exp := cluster.Spec.Master.Exporter
	exporterEnabled := true
	if exp != nil && exp.Enabled != nil {
		exporterEnabled = *exp.Enabled
	}
	if exporterEnabled {
		expImage := ""
		if exp != nil {
			expImage = exp.Image
		}
		if expImage == "" {
			expImage = "ghcr.io/henres/leilfs-operator/leilfs-exporter:dev"
		}
		expArgs := []string{
			"--listen-address=:9418",
			"--master-host=127.0.0.1",
			"--master-port=9421",
		}
		if exp != nil && exp.ScrapeTimeout != nil && exp.ScrapeTimeout.Duration > 0 {
			expArgs = append(expArgs, fmt.Sprintf("--scrape-timeout=%s", exp.ScrapeTimeout.Duration))
		}
		var expResources corev1.ResourceRequirements
		if exp != nil {
			expResources = exp.Resources
		}
		if expResources.Requests == nil && expResources.Limits == nil {
			expResources = exporterDefaultResources()
		}
		sidecarContainers = append(sidecarContainers, corev1.Container{
			Name:            "leilfs-exporter",
			Image:           expImage,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Args:            expArgs,
			Ports: []corev1.ContainerPort{
				{Name: "metrics", ContainerPort: 9418, Protocol: corev1.ProtocolTCP},
			},
			ReadinessProbe: &corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					HTTPGet: &corev1.HTTPGetAction{
						Path: "/healthz",
						Port: intstr.FromInt(9418),
					},
				},
				InitialDelaySeconds: 5,
				PeriodSeconds:       10,
			},
			Resources: expResources,
		})
	}

	desired := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: cluster.Namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &totalReplicas,
			ServiceName: hlSvcName,
			Selector:    &metav1.LabelSelector{MatchLabels: masterLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					// shareProcessNamespace allows the ha-sidecar to pgrep/kill
					// sfsmaster running in the main container, so it can trigger
					// a pod restart after a Lease transition.
					ShareProcessNamespace: func() *bool { b := true; return &b }(),
					ServiceAccountName:    saName,
					NodeSelector:          cluster.Spec.Master.NodeSelector,
					Tolerations:           cluster.Spec.Master.Tolerations,
					// Prefer spreading master/shadow pods across different nodes so
					// that a single-node failure does not take down both the active
					// master and all shadows simultaneously.  "Preferred" (weight 100)
					// rather than "required" keeps single-node dev clusters working.
					Affinity: masterPodAntiAffinity(stsName),
					// Belt-and-suspenders alongside the anti-affinity preference above:
					// a topology spread constraint gives the scheduler an explicit
					// skew budget across nodes. whenUnsatisfiable=ScheduleAnyway (not
					// DoNotSchedule) so pods still schedule on small/single-node dev
					// clusters (e.g. sfs-lima) instead of going Pending.
					TopologySpreadConstraints: []corev1.TopologySpreadConstraint{
						{
							MaxSkew:           1,
							TopologyKey:       "kubernetes.io/hostname",
							WhenUnsatisfiable: corev1.ScheduleAnyway,
							LabelSelector:     &metav1.LabelSelector{MatchLabels: podLabels},
						},
					},
					Volumes: volumes,
					InitContainers: []corev1.Container{
						{
							Name:            "init-config",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", initConfigCmd},
							VolumeMounts:    initConfigVolMounts,
							Env:             []corev1.EnvVar{podNameEnv},
						},
						{
							Name:            "init-metadata",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", initMetaCmd},
							VolumeMounts:    []corev1.VolumeMount{dataMount},
						},
					},
					Containers: append([]corev1.Container{
						{
							Name:            "leilfs-master",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", masterRunCmd},
							Ports:           ports,
							Resources:       resources,
							VolumeMounts:    []corev1.VolumeMount{cfgMount, dataMount},
							ReadinessProbe:  readinessProbe,
							LivenessProbe:   livenessProbe,
						},
					}, sidecarContainers...),
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					ObjectMeta: metav1.ObjectMeta{Name: "master-data"},
					Spec: corev1.PersistentVolumeClaimSpec{
						AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
						StorageClassName: storageClassName,
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceStorage: storageSize,
							},
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdateStatefulSet(ctx, desired)
}

// ── Master PodDisruptionBudget ───────────────────────────────────────────────

// reconcileMasterPodDisruptionBudget maintains a PodDisruptionBudget covering
// the master StatefulSet's pods, but only when shadow HA is configured.
//
// A PDB with minAvailable=1 only makes sense once there is more than one
// replica to budget against. For a single, non-HA master (spec.shadow ==
// nil, totalReplicas == 1) a minAvailable=1 PDB would require the one and
// only pod to always stay Available, which blocks routine node draining
// (kubectl drain, cluster autoscaler, k3s node upgrades on sfs-lima, etc.)
// forever — the eviction can never satisfy the budget. So the PDB is only
// created once a shadow exists to take over during a voluntary disruption,
// and is deleted if Shadow is later removed (mirroring how reconcileMasterHA
// deletes a stale Lease when Shadow == nil).
func (r *LeilFSClusterReconciler) reconcileMasterPodDisruptionBudget(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	pdbName := fmt.Sprintf("%s-master", cluster.Name)

	if cluster.Spec.Shadow == nil {
		pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: pdbName, Namespace: cluster.Namespace}}
		return client.IgnoreNotFound(r.Delete(ctx, pdb))
	}

	masterLabels := map[string]string{
		"app.kubernetes.io/name":     "leilfs-master",
		"app.kubernetes.io/instance": cluster.Name,
	}
	minAvailable := intstr.FromInt(1)

	desired := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: pdbName, Namespace: cluster.Namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: masterLabels},
		},
	}
	if err := ctrl.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}

	existing := &policyv1.PodDisruptionBudget{}
	if err := r.Get(ctx, types.NamespacedName{Name: pdbName, Namespace: cluster.Namespace}, existing); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return r.Create(ctx, desired)
	}
	existing.Spec.MinAvailable = desired.Spec.MinAvailable
	existing.Spec.Selector = desired.Spec.Selector
	return r.Update(ctx, existing)
}

// ── Master HA (Lease observer) ───────────────────────────────────────────────

// reconcileMasterHA is a passive observer of the HA Lease.
//
// The election is driven entirely by the pod sidecars: each sidecar either
// renews its own Lease (if it is the holder) or watches for expiry and
// attempts an atomic compare-and-swap PATCH to acquire the Lease.
//
// The operator's only responsibilities here are:
//  1. Bootstrap: create an empty Lease (no holderIdentity) if none exists.
//  2. Observe: read holderIdentity and sync the Service selector + pod labels.
//  3. Status: update status.ActiveMaster and status.ReadyShadows.
//
// The operator never renews nor acquires the Lease itself.
// It requeues every leaseObserveInterval so changes are picked up promptly.
func (r *LeilFSClusterReconciler) reconcileMasterHA(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	logger := log.FromContext(ctx)

	if cluster.Spec.Shadow == nil {
		// HA not configured — ensure no stale Lease lingers.
		lease := &coordinationv1.Lease{}
		leaseName := fmt.Sprintf("%s-master-ha", cluster.Name)
		if err := r.Get(ctx, types.NamespacedName{Name: leaseName, Namespace: cluster.Namespace}, lease); err == nil {
			_ = r.Delete(ctx, lease)
		}
		return nil
	}

	leaseName := fmt.Sprintf("%s-master-ha", cluster.Name)
	leaseNN := types.NamespacedName{Name: leaseName, Namespace: cluster.Namespace}
	durationSec := int32(leaseDuration.Seconds())

	// ── Bootstrap: create empty Lease if none exists ─────────────────────────
	lease := &coordinationv1.Lease{}
	if err := r.Get(ctx, leaseNN, lease); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		// Lease does not exist yet — create it with no holderIdentity.
		// The sidecar of the first ready pod will acquire it.
		empty := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: cluster.Namespace},
			Spec: coordinationv1.LeaseSpec{
				LeaseDurationSeconds: &durationSec,
			},
		}
		if err := ctrl.SetControllerReference(cluster, empty, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, empty); err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
		logger.Info("Created empty HA Lease", "lease", leaseName)
		cluster.Status.ReadyShadows = r.countReadyShadows(ctx, cluster)
		return nil
	}

	// ── Read holderIdentity ───────────────────────────────────────────────────
	var holder string
	if lease.Spec.HolderIdentity != nil {
		holder = *lease.Spec.HolderIdentity
	}

	if holder == "" {
		// No holder yet — sidecars are competing; nothing to sync.
		logger.Info("HA Lease has no holder yet", "lease", leaseName)
		cluster.Status.ReadyShadows = r.countReadyShadows(ctx, cluster)
		return nil
	}

	// ── Check whether the Lease is still fresh ───────────────────────────────
	leaseExpired := false
	if lease.Spec.RenewTime != nil {
		age := time.Since(lease.Spec.RenewTime.Time)
		if age > leaseDuration {
			leaseExpired = true
			logger.Info("HA Lease is expired", "holder", holder, "age", age.Round(time.Second))
		}
	}
	if leaseExpired {
		// Sidecar has not renewed in time; clear our status until a new holder
		// acquires the Lease.
		cluster.Status.ActiveMaster = ""
		cluster.Status.ReadyShadows = r.countReadyShadows(ctx, cluster)
		return nil
	}

	// ── Sync Service selector + pod labels ───────────────────────────────────
	logger.Info("HA Lease holder", "pod", holder)

	allPods, err := r.listAllMasterStatefulSetPods(ctx, cluster)
	if err != nil {
		return err
	}
	for i := range allPods {
		wantActive := allPods[i].Name == holder
		if err := r.setPodActiveMasterLabel(ctx, &allPods[i], wantActive); err != nil {
			logger.Error(err, "Failed to set active-master label", "pod", allPods[i].Name)
		}
	}

	if err := r.setMasterServiceSelector(ctx, cluster, holder); err != nil {
		return err
	}

	cluster.Status.ActiveMaster = holder
	cluster.Status.ReadyShadows = r.countReadyShadows(ctx, cluster)
	return nil
}

// reconcileMasterHARBAC ensures the ServiceAccount, Role, and RoleBinding that
// allow master pods to read/patch the HA Lease exist in the cluster namespace.
func (r *LeilFSClusterReconciler) reconcileMasterHARBAC(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) error {
	if cluster.Spec.Shadow == nil {
		return nil
	}

	saName := fmt.Sprintf("%s-master", cluster.Name)
	roleName := fmt.Sprintf("%s-master-ha", cluster.Name)
	leaseName := fmt.Sprintf("%s-master-ha", cluster.Name)

	// ── ServiceAccount ────────────────────────────────────────────────────────
	// Copy imagePullSecrets from the namespace's default SA so that pods using
	// this SA can pull images from private registries without manual patching.
	var pullSecrets []corev1.LocalObjectReference
	defaultSA := &corev1.ServiceAccount{}
	if err := r.Get(ctx, types.NamespacedName{Name: "default", Namespace: cluster.Namespace}, defaultSA); err == nil {
		pullSecrets = defaultSA.ImagePullSecrets
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta:       metav1.ObjectMeta{Name: saName, Namespace: cluster.Namespace},
		ImagePullSecrets: pullSecrets,
	}
	if err := ctrl.SetControllerReference(cluster, sa, r.Scheme); err != nil {
		return err
	}
	existingSA := &corev1.ServiceAccount{}
	if err := r.Get(ctx, types.NamespacedName{Name: saName, Namespace: cluster.Namespace}, existingSA); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		if err := r.Create(ctx, sa); err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
	} else {
		// Always sync imagePullSecrets so they stay up to date if the default SA changes.
		existingSA.ImagePullSecrets = pullSecrets
		if err := r.Update(ctx, existingSA); err != nil {
			return err
		}
	}

	// ── Role ─────────────────────────────────────────────────────────────────
	// Compute the exact set of pod names the sidecar is allowed to delete.
	// StatefulSet pod names are deterministic: <cluster>-master-0 .. N-1.
	var shadowReplicas int32 = 1
	if cluster.Spec.Shadow != nil && cluster.Spec.Shadow.Replicas != nil {
		shadowReplicas = *cluster.Spec.Shadow.Replicas
	}
	masterStsName := fmt.Sprintf("%s-master", cluster.Name)
	masterPodNames := make([]string, 0, 1+shadowReplicas)
	for i := int32(0); i < 1+shadowReplicas; i++ {
		masterPodNames = append(masterPodNames, fmt.Sprintf("%s-%d", masterStsName, i))
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: cluster.Namespace},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{"coordination.k8s.io"},
				Resources:     []string{"leases"},
				ResourceNames: []string{leaseName},
				Verbs:         []string{"get", "update", "patch"},
			},
			{
				// Allows the sidecar to delete its own pod (triggers full pod
				// restart including init-containers, which re-reads the Lease
				// to determine master vs shadow personality).
				// Scoped to the master StatefulSet pods only.
				APIGroups:     []string{""},
				Resources:     []string{"pods"},
				ResourceNames: masterPodNames,
				Verbs:         []string{"delete"},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, role, r.Scheme); err != nil {
		return err
	}
	existingRole := &rbacv1.Role{}
	if err := r.Get(ctx, types.NamespacedName{Name: roleName, Namespace: cluster.Namespace}, existingRole); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		if err := r.Create(ctx, role); err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
	} else {
		existingRole.Rules = role.Rules
		if err := r.Update(ctx, existingRole); err != nil {
			return err
		}
	}

	// ── RoleBinding ───────────────────────────────────────────────────────────
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: roleName, Namespace: cluster.Namespace},
		Subjects: []rbacv1.Subject{
			{Kind: "ServiceAccount", Name: saName, Namespace: cluster.Namespace},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     roleName,
		},
	}
	if err := ctrl.SetControllerReference(cluster, rb, r.Scheme); err != nil {
		return err
	}
	existingRB := &rbacv1.RoleBinding{}
	if err := r.Get(ctx, types.NamespacedName{Name: roleName, Namespace: cluster.Namespace}, existingRB); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		if err := r.Create(ctx, rb); err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
	} else {
		existingRB.Subjects = rb.Subjects
		existingRB.RoleRef = rb.RoleRef
		if err := r.Update(ctx, existingRB); err != nil {
			return err
		}
	}

	return nil
}

// setMasterServiceSelector updates the master Service so its selector routes
// traffic to the elected active pod via the active-master label.
// When the active pod is the primary master Deployment pod we use the standard
// app label selector (unchanged). When a shadow pod is elected we switch to
// the active-master label so the Service follows the pod regardless of its
// StatefulSet ordinal.
func (r *LeilFSClusterReconciler) setMasterServiceSelector(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster, activePodName string) error {
	svcName := fmt.Sprintf("%s-master", cluster.Name)
	svc := &corev1.Service{}
	if err := r.Get(ctx, types.NamespacedName{Name: svcName, Namespace: cluster.Namespace}, svc); err != nil {
		return err
	}

	svc.Spec.Selector = map[string]string{
		"app.kubernetes.io/instance": cluster.Name,
		labelActiveMaster:            "true",
	}
	return r.Update(ctx, svc)
}

// setPodActiveMasterLabel adds or removes the "leilfs.io/active-master=true"
// label on a pod. Kubernetes does not allow editing pod spec labels via the
// standard Update — we must use a Patch.
func (r *LeilFSClusterReconciler) setPodActiveMasterLabel(ctx context.Context, pod *corev1.Pod, active bool) error {
	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	if active {
		pod.Labels[labelActiveMaster] = "true"
	} else {
		delete(pod.Labels, labelActiveMaster)
	}
	return r.Patch(ctx, pod, patch)
}

// listMasterPods returns the pod currently acting as master, identified by
// cluster.Status.ActiveMaster.  Returns an empty slice if no active master is
// recorded yet (bootstrap) or if the pod no longer exists.
func (r *LeilFSClusterReconciler) listMasterPods(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) ([]corev1.Pod, error) {
	if cluster.Status.ActiveMaster == "" {
		return nil, nil
	}
	allPods, err := r.listAllMasterStatefulSetPods(ctx, cluster)
	if err != nil {
		return nil, err
	}
	for _, p := range allPods {
		if p.Name == cluster.Status.ActiveMaster {
			return []corev1.Pod{p}, nil
		}
	}
	return nil, nil
}

// listShadowPods returns all master StatefulSet pods that are NOT the current
// active master (i.e. they run as shadow or are starting up).
func (r *LeilFSClusterReconciler) listShadowPods(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) ([]corev1.Pod, error) {
	allPods, err := r.listAllMasterStatefulSetPods(ctx, cluster)
	if err != nil {
		return nil, err
	}
	var shadows []corev1.Pod
	for _, p := range allPods {
		if p.Name != cluster.Status.ActiveMaster {
			shadows = append(shadows, p)
		}
	}
	return shadows, nil
}

// listAllMasterStatefulSetPods lists all pods belonging to the unified master StatefulSet.
func (r *LeilFSClusterReconciler) listAllMasterStatefulSetPods(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	err := r.List(ctx, podList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":     "leilfs-master",
			"app.kubernetes.io/instance": cluster.Name,
		},
	)
	return podList.Items, err
}

// countReadyShadows counts ready shadow pods (all master StatefulSet pods except the active master).
func (r *LeilFSClusterReconciler) countReadyShadows(ctx context.Context, cluster *saunafsv1alpha1.LeilFSCluster) int32 {
	if cluster.Spec.Shadow == nil {
		return 0
	}
	pods, err := r.listShadowPods(ctx, cluster)
	if err != nil {
		return 0
	}
	var count int32
	for _, p := range pods {
		if isPodRunningReady(&p) {
			count++
		}
	}
	return count
}

// isPodRunningReady returns true when a pod is in the Running phase and all
// containers report Ready.
func isPodRunningReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *LeilFSClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&saunafsv1alpha1.LeilFSCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		Owns(&coordinationv1.Lease{}).
		// Watch PVs with the localdisk label to trigger reconciliation when
		// new disks are discovered by the localdisk-operator.
		Watches(&corev1.PersistentVolume{}, handler.EnqueueRequestsFromMapFunc(r.pvToClusterRequests)).
		Complete(r)
}

// pvToClusterRequests maps a PV change to reconcile requests for all
// LeilFSCluster resources that have autoDiscover enabled.
func (r *LeilFSClusterReconciler) pvToClusterRequests(ctx context.Context, obj client.Object) []ctrl.Request {
	pv, ok := obj.(*corev1.PersistentVolume)
	if !ok {
		return nil
	}
	// Only trigger for PVs with localdisk labels.
	if _, hasLabel := pv.Labels["localdisk-operator.io/disk"]; !hasLabel {
		return nil
	}

	// List all LeilFSCluster resources.
	clusterList := &saunafsv1alpha1.LeilFSClusterList{}
	if err := r.List(ctx, clusterList); err != nil {
		return nil
	}

	var requests []ctrl.Request
	for _, cluster := range clusterList.Items {
		if cluster.Spec.Chunk.AutoDiscover != nil && cluster.Spec.Chunk.AutoDiscover.Enabled {
			requests = append(requests, ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      cluster.Name,
					Namespace: cluster.Namespace,
				},
			})
		}
	}
	return requests
}
