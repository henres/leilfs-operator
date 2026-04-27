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
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	saunafsv1alpha1 "github.com/henres/saunafs-operator/api/v1alpha1"
	"github.com/henres/saunafs-operator/internal/metrics"
)

// SaunaFSClusterReconciler reconciles a SaunaFSCluster object
type SaunaFSClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// HA labels and lease constants.
const (
	// labelActiveMaster is set to "true" on whichever master/shadow pod
	// currently holds the active-master role. The master Service selector
	// matches this label so traffic always reaches the active pod.
	labelActiveMaster = "saunafs.io/active-master"

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
)

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
// for a saunafs-master / shadow pod.
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

// chunkDefaultResources returns defaults for a saunafs-chunkserver pod.
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

// metaloggerDefaultResources returns defaults for a saunafs-metalogger pod.
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

// webUIDefaultResources returns defaults for the saunafs-cgiserver pod.
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
								"app.kubernetes.io/name": "saunafs-master",
							},
						},
						TopologyKey: "kubernetes.io/hostname",
					},
				},
			},
		},
	}
}

//+kubebuilder:rbac:groups=saunafs.saunafs-operator.io,resources=saunafsclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=saunafs.saunafs-operator.io,resources=saunafsclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=saunafs.saunafs-operator.io,resources=saunafsclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=daemonsets;statefulsets;deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims;configmaps;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles;rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch

func (r *SaunaFSClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cluster := &saunafsv1alpha1.SaunaFSCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		if errors.IsNotFound(err) {
			// Cluster was deleted: drop all of its metrics so Prometheus
			// doesn't keep stale series for non-existent objects.
			metrics.DeleteCluster(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling SaunaFSCluster", "name", cluster.Name)

	// Snapshot the cluster object BEFORE any reconcile step mutates its status,
	// so the MergePatch below captures all status changes made during reconcile.
	statusPatchBase := client.MergeFrom(cluster.DeepCopy())

	// Run all sub-reconcilers; on first error record a Failed condition.
	var reconcileErr error
	steps := []struct {
		name string
		fn   func(context.Context, *saunafsv1alpha1.SaunaFSCluster) error
	}{
		{"goals configmap", r.reconcileGoalsConfigMap},
		{"migrate legacy master objects", r.migrateMasterToStatefulSet},
		{"master statefulset", r.reconcileMasterStatefulSet},
		{"master service", r.reconcileMasterService},
		{"master ha rbac", r.reconcileMasterHARBAC},
		{"master ha", r.reconcileMasterHA},
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

	cluster.Status.TotalChunkServers = int32(len(cluster.Spec.Chunk.Servers))

	if reconcileErr != nil {
		apimeta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
			Type:               saunafsv1alpha1.ConditionReady,
			Status:             metav1.ConditionFalse,
			Reason:             saunafsv1alpha1.ReasonReconcileError,
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
		logger.Error(err, "Failed to update SaunaFSCluster status")
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

// publishMetrics updates Prometheus collectors from the freshly
// reconciled cluster state. It is best-effort: any errors while
// counting auto-discovered PVs are logged and don't fail the reconcile.
func (r *SaunaFSClusterReconciler) publishMetrics(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster, reconcileErr error) {
	logger := log.FromContext(ctx)
	ns := cluster.Namespace
	name := cluster.Name

	// Cluster info: tag with the master image so dashboards can group by
	// SaunaFS version. Empty string when the user didn't pin an image.
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
// whose desired replicas are all ready.
func (r *SaunaFSClusterReconciler) countReadyChunkServers(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) int32 {
	var ready int32
	for _, srv := range cluster.Spec.Chunk.Servers {
		name := fmt.Sprintf("%s-chunk-%s", cluster.Name, srv.Name)
		sts := &appsv1.StatefulSet{}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: cluster.Namespace}, sts); err != nil {
			continue
		}
		if sts.Status.ReadyReplicas >= *sts.Spec.Replicas {
			ready++
		}
	}
	return ready
}

// countReadyMetaloggers returns the number of ready metalogger replicas.
func (r *SaunaFSClusterReconciler) countReadyMetaloggers(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) int32 {
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
func (r *SaunaFSClusterReconciler) migrateMasterToStatefulSet(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
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
func (r *SaunaFSClusterReconciler) reconcileGoalsConfigMap(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
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
	sb.WriteString("# sfsgoals.cfg — generated by saunafs-operator (do not edit manually)\n")
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

func masterContainerPorts(cluster *saunafsv1alpha1.SaunaFSCluster) []corev1.ContainerPort {
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

// ── Master Service ──────────────────────────────────────────────────────────

func (r *SaunaFSClusterReconciler) reconcileMasterService(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
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
			"app.kubernetes.io/name":     "saunafs-master",
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

func (r *SaunaFSClusterReconciler) reconcileChunkServers(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
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
func (r *SaunaFSClusterReconciler) reconcileChunkHeadlessService(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster, srv *saunafsv1alpha1.ChunkServerSpec) error {
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
				"app.kubernetes.io/name":     "saunafs-chunkserver",
				"app.kubernetes.io/instance": cluster.Name,
				"saunafs.io/chunk-server":    srv.Name,
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
func (r *SaunaFSClusterReconciler) reconcileChunkHddConfigMap(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster, srv *saunafsv1alpha1.ChunkServerSpec) error {
	cmName := fmt.Sprintf("%s-chunk-%s-hdd", cluster.Name, srv.Name)

	var sb strings.Builder
	sb.WriteString("# sfshdd.cfg — generated by saunafs-operator (do not edit manually)\n")
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

func (r *SaunaFSClusterReconciler) reconcileChunkStatefulSet(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster, srv *saunafsv1alpha1.ChunkServerSpec) error {
	name := fmt.Sprintf("%s-chunk-%s", cluster.Name, srv.Name)

	image := srv.Image
	if image == "" {
		image = cluster.Spec.Chunk.Image
	}
	if image == "" {
		image = "ghcr.io/henres/saunafs-container/saunafs-chunkserver:5.9.0"
	}

	var replicas int32 = 1

	// Master service DNS for this cluster
	masterHost := fmt.Sprintf("%s-master.%s.svc.cluster.local", cluster.Name, cluster.Namespace)

	// emptyDir shared between init and main container for /etc/saunafs
	cfgVolume := corev1.Volume{
		Name:         "saunafs-cfg",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	cfgMount := corev1.VolumeMount{Name: "saunafs-cfg", MountPath: "/etc/saunafs"}

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
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &replicas,
			ServiceName: name,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "saunafs-chunkserver",
					"app.kubernetes.io/instance": cluster.Name,
					"saunafs.io/chunk-server":    srv.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":     "saunafs-chunkserver",
						"app.kubernetes.io/instance": cluster.Name,
						"saunafs.io/chunk-server":    srv.Name,
					},
				},
				Spec: corev1.PodSpec{
					NodeName:    srv.NodeName,
					Tolerations: srv.Tolerations,
					// Each chunk server is pinned to a dedicated node (nodeName).
					// hostNetwork=true makes the chunk server register with the
					// master using the node's routable IP so that external SaunaFS
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
							Name:            "saunafs-chunkserver",
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

func chunkContainerPorts(cluster *saunafsv1alpha1.SaunaFSCluster) []corev1.ContainerPort {
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
func (r *SaunaFSClusterReconciler) reconcileAutoDiscoverChunkServers(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
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
		mountPath := fmt.Sprintf("/mnt/%s", pv.Name)
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
func (r *SaunaFSClusterReconciler) ensureAutoDiscoverPVC(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster, pv *corev1.PersistentVolume, pvcName string) error {
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
				"app.kubernetes.io/name":       "saunafs-chunkserver",
				"app.kubernetes.io/instance":   cluster.Name,
				"app.kubernetes.io/managed-by": "saunafs-operator",
				"saunafs.io/auto-discover":     "true",
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

// saunafsLabel converts a string to a valid SaunaFS LABEL value.
// SaunaFS labels must be alphanumeric with underscores only (no hyphens).
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

func (r *SaunaFSClusterReconciler) reconcileInterface(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
	iface := &cluster.Spec.WebUI

	// Skip if not explicitly enabled.
	if iface.Enabled == nil || !*iface.Enabled {
		return nil
	}

	image := iface.Image
	if image == "" {
		image = "ghcr.io/henres/saunafs-container/saunafs-cgiserver:5.9.0"
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

	name := fmt.Sprintf("%s-interface", cluster.Name)
	labels := map[string]string{
		"app.kubernetes.io/name":     "saunafs-interface",
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
							Name:            "saunafs-cgiserver",
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
// external SaunaFS clients connect to the master and mount the filesystem.
// The service targets the master pod on the standard SaunaFS client port (9421)
// and, optionally, the admin port (9419).
//
// Usage from a SaunaFS client node (replace <node-ip> and <node-port>):
//
//	saunafs-mount -H <node-ip> -P <node-port> /mnt/saunafs
func (r *SaunaFSClusterReconciler) reconcileExposeService(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
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
		"app.kubernetes.io/name":     "saunafs-master",
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
func exposeEnabled(cluster *saunafsv1alpha1.SaunaFSCluster) bool {
	return cluster.Spec.Expose.Enabled != nil && *cluster.Spec.Expose.Enabled
}

// ── NFS-Ganesha Gateway ──────────────────────────────────────────────────────

// reconcileNFS creates (or cleans up) a NFS-Ganesha Deployment and its
// NodePort Service.
//
// Architecture:
//
//	NFS client  ──►  NodePort:2049  ──►  NFS-Ganesha pod
//	                                       ├─ saunafs-client sidecar (FUSE)
//	                                       │    mounts SaunaFS → /exports
//	                                       └─ izdock/nfs-ganesha
//	                                            re-exports /exports via VFS FSAL
//
// The saunafs-client sidecar mounts the filesystem via FUSE into a shared
// emptyDir volume (/exports). NFS-Ganesha then exports that local path.
// Both containers are privileged so the FUSE mount is visible across them
// via mountPropagation:Bidirectional.
func (r *SaunaFSClusterReconciler) reconcileNFS(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
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
		ganeshaImage = "ghcr.io/henres/saunafs-operator/nfs-ganesha:latest"
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
	// Format follows the official SaunaFS documentation exactly.
	// Ganesha connects directly to the SaunaFS master via the SaunaFS FSAL —
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
		"app.kubernetes.io/name":     "saunafs-nfs",
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
					// Wait for the SaunaFS master to accept TCP connections on
					// port 9421 before starting ganesha. Without this guard,
					// ganesha tries to mount the SaunaFS filesystem during its
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
									`until nc -z %s 9421 2>/dev/null; do echo "waiting for saunafs-master:9421..."; sleep 2; done; echo "saunafs-master ready"`,
									masterHost,
								),
							},
						},
					},
					Containers: []corev1.Container{
						{
							// Single container: ganesha.nfsd with the SaunaFS
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
							Command: []string{"sh", "-c",
								"rpcbind -f & sleep 2; ganesha.nfsd -F -f /etc/ganesha/ganesha.conf -N NIV_WARN -L /dev/stdout",
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
							// ganesha.conf with SaunaFS FSAL settings,
							// generated from the SaunaFSCluster spec.
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
func chunkDNSPolicy(cluster *saunafsv1alpha1.SaunaFSCluster) corev1.DNSPolicy {
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
func (r *SaunaFSClusterReconciler) reconcileMetaloggers(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
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
		image = "ghcr.io/henres/saunafs-container/saunafs-metalogger:5.9.0"
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
		"app.kubernetes.io/name":      "saunafs-metalogger",
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
		Name:         "saunafs-cfg",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	cfgMount := corev1.VolumeMount{Name: "saunafs-cfg", MountPath: "/etc/saunafs"}

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
							Name:            "saunafs-metalogger",
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

func (r *SaunaFSClusterReconciler) createOrUpdateStatefulSet(ctx context.Context, desired *appsv1.StatefulSet) error {
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
	return r.Update(ctx, existing)
}

func (r *SaunaFSClusterReconciler) createOrUpdateDeployment(ctx context.Context, desired *appsv1.Deployment) error {
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
func (r *SaunaFSClusterReconciler) reconcileMasterStatefulSet(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
	stsName := fmt.Sprintf("%s-master", cluster.Name)
	hlSvcName := fmt.Sprintf("%s-master-hl", cluster.Name)

	image := cluster.Spec.Master.Image
	if image == "" {
		image = "ghcr.io/henres/saunafs-container/saunafs-master:5.9.0"
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
		"app.kubernetes.io/name":     "saunafs-master",
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
			Name: "saunafs-goals",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
				},
			},
		})
		goalsMounts = append(goalsMounts, corev1.VolumeMount{
			Name:      "saunafs-goals",
			MountPath: "/etc/saunafs-goals",
			ReadOnly:  true,
		})
		goalsCmd = `[ -f /etc/saunafs-goals/sfsgoals.cfg ] && cp /etc/saunafs-goals/sfsgoals.cfg /etc/saunafs/sfsgoals.cfg
`
	}

	leaseName := fmt.Sprintf("%s-master-ha", cluster.Name)
	saName := fmt.Sprintf("%s-master", cluster.Name)

	const defaultStartupGrace = 30 * time.Second
	startupGrace := defaultStartupGrace
	if cluster.Spec.Master.StartupGracePeriod != nil {
		startupGrace = cluster.Spec.Master.StartupGracePeriod.Duration
	}

	// init-config: read Lease → decide master or shadow personality.
	// Uses wget (available in the saunafs-master image) to call the kube API.
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
fi`,
		goalsCmd, leaseName, masterHost, masterHost)

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
exec /saunafs-master.start.sh
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

# Wait for sfsmaster to load metadata before we start health-checking it.
# This covers large filesystems where metadata load takes tens of seconds.
sleep ${STARTUP_GRACE}
echo "[ha-sidecar] startup grace period elapsed, entering main loop"

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
    if ! pgrep -x sfsmaster > /dev/null 2>&1; then
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
		Name:         "saunafs-cfg",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	cfgMount := corev1.VolumeMount{Name: "saunafs-cfg", MountPath: "/etc/saunafs"}
	dataMount := corev1.VolumeMount{Name: "master-data", MountPath: "/var/lib/saunafs"}

	podLabels := map[string]string{
		"app.kubernetes.io/name":     "saunafs-master",
		"app.kubernetes.io/instance": cluster.Name,
	}

	initConfigVolMounts := append([]corev1.VolumeMount{cfgMount, dataMount}, goalsMounts...)
	volumes := append([]corev1.Volume{cfgVolume}, goalsVolume...)

	ports := masterContainerPorts(cluster)

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
					Volumes:  volumes,
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
							Name:            "saunafs-master",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", masterRunCmd},
							Ports:           ports,
							Resources:       resources,
							VolumeMounts:    []corev1.VolumeMount{cfgMount, dataMount},
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
func (r *SaunaFSClusterReconciler) reconcileMasterHA(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
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
func (r *SaunaFSClusterReconciler) reconcileMasterHARBAC(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
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
func (r *SaunaFSClusterReconciler) setMasterServiceSelector(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster, activePodName string) error {
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

// setPodActiveMasterLabel adds or removes the "saunafs.io/active-master=true"
// label on a pod. Kubernetes does not allow editing pod spec labels via the
// standard Update — we must use a Patch.
func (r *SaunaFSClusterReconciler) setPodActiveMasterLabel(ctx context.Context, pod *corev1.Pod, active bool) error {
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
func (r *SaunaFSClusterReconciler) listMasterPods(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) ([]corev1.Pod, error) {
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
func (r *SaunaFSClusterReconciler) listShadowPods(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) ([]corev1.Pod, error) {
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
func (r *SaunaFSClusterReconciler) listAllMasterStatefulSetPods(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	err := r.List(ctx, podList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{
			"app.kubernetes.io/name":     "saunafs-master",
			"app.kubernetes.io/instance": cluster.Name,
		},
	)
	return podList.Items, err
}

// countReadyShadows counts ready shadow pods (all master StatefulSet pods except the active master).
func (r *SaunaFSClusterReconciler) countReadyShadows(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) int32 {
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
func (r *SaunaFSClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&saunafsv1alpha1.SaunaFSCluster{}).
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
// SaunaFSCluster resources that have autoDiscover enabled.
func (r *SaunaFSClusterReconciler) pvToClusterRequests(ctx context.Context, obj client.Object) []ctrl.Request {
	pv, ok := obj.(*corev1.PersistentVolume)
	if !ok {
		return nil
	}
	// Only trigger for PVs with localdisk labels.
	if _, hasLabel := pv.Labels["localdisk-operator.io/disk"]; !hasLabel {
		return nil
	}

	// List all SaunaFSCluster resources.
	clusterList := &saunafsv1alpha1.SaunaFSClusterList{}
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
