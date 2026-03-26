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
	"bytes"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	saunafsv1alpha1 "github.com/henres/saunafs-operator/api/v1alpha1"
)

// SaunaFSClusterReconciler reconciles a SaunaFSCluster object
type SaunaFSClusterReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	RestConfig *rest.Config // required for pod exec (HA promotion)
}

// HA labels and lease constants.
const (
	// labelActiveMaster is set to "true" on whichever master/shadow pod
	// currently holds the active-master role. The master Service selector
	// matches this label so traffic always reaches the active pod.
	labelActiveMaster = "saunafs.io/active-master"

	// labelMasterRole distinguishes primary ("primary") from shadow ("shadow") pods.
	labelMasterRole = "saunafs.io/master-role"

	// leaseDuration is how long the operator considers a Lease valid without renewal.
	leaseDuration = 15 * time.Second

	// leaseRenewInterval is how often the operator renews the Lease for a healthy active master.
	leaseRenewInterval = 5 * time.Second
)

//+kubebuilder:rbac:groups=saunafs.saunafs-operator.io,resources=saunafsclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=saunafs.saunafs-operator.io,resources=saunafsclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=saunafs.saunafs-operator.io,resources=saunafsclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=daemonsets;statefulsets;deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims;configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch
//+kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

func (r *SaunaFSClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cluster := &saunafsv1alpha1.SaunaFSCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
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
		{"master ha", r.reconcileMasterHA},
		{"chunk servers", r.reconcileChunkServers},
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

	// When shadow HA is configured, requeue regularly so the operator can renew
	// the Lease and detect master failures without waiting for a watch event.
	if cluster.Spec.Shadow != nil && reconcileErr == nil {
		return ctrl.Result{RequeueAfter: leaseRenewInterval}, nil
	}

	return ctrl.Result{}, reconcileErr
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
		image = "ghcr.io/henres/saunafs-container/saunafs-chunkserver:latest"
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
							Resources:       srv.Resources,
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

// ── CGI Interface Deployment ─────────────────────────────────────────────────

func (r *SaunaFSClusterReconciler) reconcileInterface(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
	iface := &cluster.Spec.WebUI

	// Skip if not explicitly enabled.
	if iface.Enabled == nil || !*iface.Enabled {
		return nil
	}

	image := iface.Image
	if image == "" {
		image = "ghcr.io/henres/saunafs-container/saunafs-cgiserver:latest"
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
							Resources: iface.Resources,
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

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeNodePort,
			Selector: map[string]string{
				"app.kubernetes.io/name":     "saunafs-master",
				"app.kubernetes.io/instance": cluster.Name,
			},
			Ports: servicePorts,
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
							Resources:       nfs.Resources,
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
		image = "ghcr.io/henres/saunafs-container/saunafs-metalogger:latest"
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
							Resources:       ml.Resources,
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
		image = "ghcr.io/henres/saunafs-container/saunafs-master:latest"
	}

	// Total replicas: 1 primary + N shadows.
	var shadowReplicas int32
	if cluster.Spec.Shadow != nil && cluster.Spec.Shadow.Replicas != nil {
		shadowReplicas = *cluster.Spec.Shadow.Replicas
	}
	totalReplicas := int32(1) + shadowReplicas

	// Storage size & class: use shadow spec if present, else master spec.
	storageSize := resource.MustParse("1Gi")
	var storageClassName *string
	if ms := cluster.Spec.Master.MetadataStorage; ms != nil {
		if !ms.Size.IsZero() {
			storageSize = ms.Size
		}
		if ms.StorageClassName != "" {
			sc := ms.StorageClassName
			storageClassName = &sc
		}
	}
	if sh := cluster.Spec.Shadow; sh != nil && sh.MetadataStorage != nil {
		if !sh.MetadataStorage.Size.IsZero() {
			storageSize = sh.MetadataStorage.Size
		}
		if sh.MetadataStorage.StorageClassName != "" {
			sc := sh.MetadataStorage.StorageClassName
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

	// ── init-config command (ordinal-aware) ──────────────────────────────────
	// HOSTNAME = "<stsName>-<ordinal>" → ORDINAL="${HOSTNAME##*-}"
	// pod-0  → primary master: just copy examples (+ optional goals overlay)
	// pod-1+ → shadow: copy examples, then patch PERSONALITY and MASTER_HOST
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
		goalsCmd = `  [ -f /etc/saunafs-goals/sfsgoals.cfg ] && cp /etc/saunafs-goals/sfsgoals.cfg /etc/saunafs/sfsgoals.cfg` + "\n"
	}

	initConfigCmd := fmt.Sprintf(`ORDINAL="${HOSTNAME##*-}"
mkdir -p /etc/saunafs /var/lib/saunafs
cp -r /usr/share/doc/saunafs-master/examples/. /etc/saunafs/
if [ "$ORDINAL" = "0" ]; then
%s  : # primary master – no personality patch needed
else
  # Clear any stale promotion sentinel so a restarted shadow always starts as shadow.
  rm -f /var/lib/saunafs/.promote
  sed -i 's/^# *PERSONALITY *= *.*/PERSONALITY = shadow/' /etc/saunafs/sfsmaster.cfg
  grep -q "^PERSONALITY" /etc/saunafs/sfsmaster.cfg || echo "PERSONALITY = shadow" >> /etc/saunafs/sfsmaster.cfg
  sed -i 's/^# *MASTER_HOST *= *sfsmaster/MASTER_HOST = %s/' /etc/saunafs/sfsmaster.cfg
  grep -q "^MASTER_HOST" /etc/saunafs/sfsmaster.cfg || echo "MASTER_HOST = %s" >> /etc/saunafs/sfsmaster.cfg
  grep -q "^MASTER_PORT" /etc/saunafs/sfsmaster.cfg || echo "MASTER_PORT = 9419" >> /etc/saunafs/sfsmaster.cfg
  grep -q "^MASTER_RECONNECTION_DELAY" /etc/saunafs/sfsmaster.cfg || echo "MASTER_RECONNECTION_DELAY = 5" >> /etc/saunafs/sfsmaster.cfg
  grep -q "^MASTER_TIMEOUT" /etc/saunafs/sfsmaster.cfg || echo "MASTER_TIMEOUT = 60" >> /etc/saunafs/sfsmaster.cfg
fi`, goalsCmd, masterHost, masterHost)

	initMetaCmd := "cp -n /opt/saunafs/templates/metadata.sfs.empty /var/lib/saunafs/metadata.sfs 2>/dev/null || true"

	// ── Wrapper script (all pods) ─────────────────────────────────────────────
	// Checks for a .promote sentinel on the PVC (written by the operator during
	// failover). If found: switches PERSONALITY to master and removes shadow
	// config lines, then runs the standard start script.
	// pod-0 never needs this under normal operation, but it works correctly for
	// the failback case where a promoted shadow is reset to shadow and pod-0
	// takes back the primary role.
	masterRunCmd := `
if [ -f /var/lib/saunafs/.promote ]; then
  echo "[ha] promotion sentinel found – starting as master"
  rm -f /var/lib/saunafs/.promote
  sed -i 's/^PERSONALITY = shadow/PERSONALITY = master/' /etc/saunafs/sfsmaster.cfg
  sed -i '/^MASTER_HOST/d' /etc/saunafs/sfsmaster.cfg
  sed -i '/^MASTER_PORT/d' /etc/saunafs/sfsmaster.cfg
fi
exec /saunafs-master.start.sh
`

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

	// Resources: use master resources for pod-0, shadow resources for pod-1+.
	// Since StatefulSet has a single container spec, we use master resources
	// for all pods. Shadow resources (if different) can be added later via
	// per-ordinal resource overrides (requires a future operator enhancement).
	resources := cluster.Spec.Master.Resources

	desired := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: stsName, Namespace: cluster.Namespace},
		Spec: appsv1.StatefulSetSpec{
			Replicas:    &totalReplicas,
			ServiceName: hlSvcName,
			Selector:    &metav1.LabelSelector{MatchLabels: masterLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					NodeSelector: cluster.Spec.Master.NodeSelector,
					Tolerations:  cluster.Spec.Master.Tolerations,
					Volumes:      volumes,
					InitContainers: []corev1.Container{
						{
							Name:            "init-config",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", initConfigCmd},
							VolumeMounts:    initConfigVolMounts,
						},
						{
							Name:            "init-metadata",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", initMetaCmd},
							VolumeMounts:    []corev1.VolumeMount{dataMount},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "saunafs-master",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", masterRunCmd},
							Ports:           ports,
							Resources:       resources,
							VolumeMounts:    []corev1.VolumeMount{cfgMount, dataMount},
						},
					},
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

// ── Master HA (Lease + failover) ─────────────────────────────────────────────

// reconcileMasterHA implements the leader-election loop for the master role:
//
//  1. Read or create the Lease "<cluster>-master-ha".
//  2. If the current active pod (status.ActiveMaster) is Running+Ready, renew
//     the Lease and return.
//  3. Otherwise (no active master known, or the recorded pod is gone/not-ready):
//     a. If pod-0 (primary) is Running+Ready → make it active.
//     b. Else if any shadow pods (ordinal > 0) are Running+Ready → pick the
//     lowest ordinal, write the .promote sentinel, make it active.
//  4. Update the "active-master=true" label on pods and the master Service
//     selector accordingly.
//  5. Clear the label from all other master/shadow pods.
//  6. Update status.ActiveMaster and status.ReadyShadows.
//
// The operator requeues every leaseRenewInterval seconds so the Lease is kept
// fresh.  A stale Lease (> leaseDuration) is treated the same as a missing one
// and triggers a new election.
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

	// ── Gather all master pods (pod-0 = primary, pod-N = shadow) ─────────────
	masterPods, err := r.listMasterPods(ctx, cluster)
	if err != nil {
		return err
	}
	shadowPods, err := r.listShadowPods(ctx, cluster)
	if err != nil {
		return err
	}

	allCandidates := append(masterPods, shadowPods...)

	// ── Determine current active pod ─────────────────────────────────────────
	// Check both the persisted status AND the live active-master label on pods.
	// The label is applied immediately upon election (before the status patch is
	// persisted), so it provides faster convergence across reconcile cycles.
	currentActive := cluster.Status.ActiveMaster
	activeRunning := false
	for _, p := range allCandidates {
		isActive := (p.Name == currentActive) || (p.Labels[labelActiveMaster] == "true")
		if isActive && isPodRunningReady(&p) {
			activeRunning = true
			// Sync the in-memory status so the Lease and subsequent logic agree.
			if p.Name != currentActive {
				cluster.Status.ActiveMaster = p.Name
				currentActive = p.Name
			}
			break
		}
	}

	// ── Lease management ─────────────────────────────────────────────────────
	holderIdentity := currentActive
	now := metav1.NewMicroTime(time.Now())
	durationSec := int32(leaseDuration.Seconds())

	lease := &coordinationv1.Lease{}
	leaseNN := types.NamespacedName{Name: leaseName, Namespace: cluster.Namespace}
	leaseExists := true
	if err := r.Get(ctx, leaseNN, lease); err != nil {
		if !errors.IsNotFound(err) {
			return err
		}
		leaseExists = false
	}

	// ── Failback: prefer pod-0 over a promoted shadow ────────────────────────
	// If the current active is a shadow pod (ordinal > 0) and pod-0 is now
	// Running+Ready, trigger a re-election so traffic moves back to the primary.
	// This handles the failback case without disrupting a healthy shadow-as-master.
	if activeRunning && currentActive != "" {
		isPrimary := strings.HasSuffix(currentActive, "-master-0")
		if !isPrimary {
			// Current active is a shadow. Check if pod-0 is now ready.
			for i := range masterPods {
				if isPodRunningReady(&masterPods[i]) {
					logger.Info("Primary pod-0 is ready, triggering failback election",
						"current", currentActive, "primary", masterPods[i].Name)
					activeRunning = false
					break
				}
			}
		}
	}

	if activeRunning {
		// Just renew the Lease.
		if !leaseExists {
			lease = &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: cluster.Namespace},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       &holderIdentity,
					LeaseDurationSeconds: &durationSec,
					RenewTime:            &now,
					AcquireTime:          &now,
					LeaseTransitions:     new(int32),
				},
			}
			if err := ctrl.SetControllerReference(cluster, lease, r.Scheme); err != nil {
				return err
			}
			if err := r.Create(ctx, lease); err != nil && !errors.IsAlreadyExists(err) {
				return err
			}
		} else {
			lease.Spec.HolderIdentity = &holderIdentity
			lease.Spec.RenewTime = &now
			lease.Spec.LeaseDurationSeconds = &durationSec
			if err := r.Update(ctx, lease); err != nil {
				return err
			}
		}
		// Update ReadyShadows count.
		cluster.Status.ReadyShadows = r.countReadyShadows(ctx, cluster)
		return nil
	}

	// ── Election: active pod is gone or unknown ───────────────────────────────
	logger.Info("Active master not ready, running HA election",
		"cluster", cluster.Name, "previous", currentActive)

	var elected *corev1.Pod

	// Prefer primary master Deployment pod if it is ready.
	for i := range masterPods {
		if isPodRunningReady(&masterPods[i]) {
			elected = &masterPods[i]
			break
		}
	}

	// Only consider shadow promotion if no primary master pod is in Running phase.
	// A pod that is Running but not yet Ready (e.g. still initialising) will be
	// ready within seconds — we wait rather than disrupting the shadow.
	// A pod that is Pending (e.g. unschedulable due to node failure) warrants
	// shadow promotion since the primary cannot serve traffic.
	primaryRunning := false
	for i := range masterPods {
		if masterPods[i].Status.Phase == corev1.PodRunning {
			primaryRunning = true
			break
		}
	}
	if elected == nil && !primaryRunning {
		readyShadows := []corev1.Pod{}
		for i := range shadowPods {
			if isPodRunningReady(&shadowPods[i]) {
				readyShadows = append(readyShadows, shadowPods[i])
			}
		}
		sort.Slice(readyShadows, func(i, j int) bool {
			return readyShadows[i].Name < readyShadows[j].Name
		})
		if len(readyShadows) > 0 {
			elected = &readyShadows[0]
		}
	}

	if elected == nil {
		logger.Info("No ready master or shadow pod found, waiting")
		cluster.Status.ReadyShadows = r.countReadyShadows(ctx, cluster)
		return nil // will be re-triggered by pod watch
	}

	// If elected is a shadow, promote it.
	if elected.Labels[labelMasterRole] == "shadow" {
		logger.Info("Promoting shadow to master", "pod", elected.Name)
		// Strategy: write a promotion sentinel file to the PVC, then send SIGTERM
		// to the running sfsmaster process so it exits cleanly.  The container's
		// wrapper script (masterRunCmd) will detect the sentinel on the next start
		// and launch sfsmaster with PERSONALITY=master instead of shadow.
		promoteCmd := []string{"sh", "-c",
			"touch /var/lib/saunafs/.promote && kill -TERM $(cat /var/run/sfsmaster.pid 2>/dev/null || pgrep sfsmaster) 2>/dev/null || true",
		}
		if err := r.execInPod(ctx, cluster.Namespace, elected.Name, "saunafs-master", promoteCmd); err != nil {
			// Transient errors (container restarting, not yet ready) should not
			// cause an immediate requeue storm. Log and wait for the next cycle.
			logger.Info("Shadow promotion exec failed (will retry)", "pod", elected.Name, "err", err.Error())
			cluster.Status.ReadyShadows = r.countReadyShadows(ctx, cluster)
			return nil
		}
	}

	// ── Flip labels ───────────────────────────────────────────────────────────
	newActive := elected.Name
	for _, p := range allCandidates {
		wantActive := p.Name == newActive
		if err := r.setPodActiveMasterLabel(ctx, &p, wantActive); err != nil {
			logger.Error(err, "Failed to set active-master label", "pod", p.Name)
			// non-fatal: continue to update the Service
		}
	}

	// ── Demote any previously-promoted shadow back to shadow mode ─────────────
	// When pod-0 is re-elected after a failback, shadow pods that were promoted
	// (running sfsmaster in master mode) must be restarted so their init-container
	// re-applies the shadow config (PERSONALITY=shadow + MASTER_HOST).
	// We delete those pods; the StatefulSet controller will recreate them.
	for _, p := range allCandidates {
		if p.Name == newActive {
			continue // don't touch the newly elected pod
		}
		// A shadow pod (ordinal > 0) that was previously the active master
		// needs to be restarted as shadow.
		isShadowOrdinal := !strings.HasSuffix(p.Name, "-master-0")
		if isShadowOrdinal {
			logger.Info("Deleting promoted shadow pod to re-init as shadow", "pod", p.Name)
			if err := r.Delete(ctx, &p); err != nil && !errors.IsNotFound(err) {
				logger.Error(err, "Failed to delete shadow pod for demotion", "pod", p.Name)
				// non-fatal: StatefulSet will eventually reconcile
			}
		}
	}

	// ── Update master Service selector ───────────────────────────────────────
	if err := r.setMasterServiceSelector(ctx, cluster, newActive); err != nil {
		return err
	}

	// ── Update Lease ─────────────────────────────────────────────────────────
	transitions := int32(1)
	if leaseExists && lease.Spec.LeaseTransitions != nil {
		transitions = *lease.Spec.LeaseTransitions + 1
	}
	if !leaseExists {
		lease = &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: cluster.Namespace},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &newActive,
				LeaseDurationSeconds: &durationSec,
				RenewTime:            &now,
				AcquireTime:          &now,
				LeaseTransitions:     &transitions,
			},
		}
		if err := ctrl.SetControllerReference(cluster, lease, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, lease); err != nil && !errors.IsAlreadyExists(err) {
			return err
		}
	} else {
		lease.Spec.HolderIdentity = &newActive
		lease.Spec.RenewTime = &now
		lease.Spec.AcquireTime = &now
		lease.Spec.LeaseDurationSeconds = &durationSec
		lease.Spec.LeaseTransitions = &transitions
		if err := r.Update(ctx, lease); err != nil {
			return err
		}
	}

	cluster.Status.ActiveMaster = newActive
	cluster.Status.ReadyShadows = r.countReadyShadows(ctx, cluster)
	logger.Info("HA election complete", "activeMaster", newActive)
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

// listMasterPods returns all pods of the unified master StatefulSet
// that have ordinal 0 (the primary master pod).
// In the unified StatefulSet, pod-0 always starts as the primary master.
func (r *SaunaFSClusterReconciler) listMasterPods(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) ([]corev1.Pod, error) {
	allPods, err := r.listAllMasterStatefulSetPods(ctx, cluster)
	if err != nil {
		return nil, err
	}
	// pod-0 is the primary master: its name ends with "-master-0"
	primaryName := fmt.Sprintf("%s-master-0", cluster.Name)
	var primary []corev1.Pod
	for _, p := range allPods {
		if p.Name == primaryName {
			// Ensure role label is set correctly
			p.Labels[labelMasterRole] = "primary"
			primary = append(primary, p)
		}
	}
	return primary, nil
}

// listShadowPods returns all pods of the unified master StatefulSet
// that have ordinal > 0 (shadow pods).
func (r *SaunaFSClusterReconciler) listShadowPods(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) ([]corev1.Pod, error) {
	allPods, err := r.listAllMasterStatefulSetPods(ctx, cluster)
	if err != nil {
		return nil, err
	}
	primaryName := fmt.Sprintf("%s-master-0", cluster.Name)
	var shadows []corev1.Pod
	for _, p := range allPods {
		if p.Name != primaryName {
			// Ensure role label is set correctly
			p.Labels[labelMasterRole] = "shadow"
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

// countReadyShadows counts ready shadow master pods (ordinal > 0 in the unified StatefulSet).
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

// execInPod runs a command inside a running pod container via the Kubernetes
// exec subresource. stdout/stderr are captured and returned in the error when
// the command exits non-zero.
func (r *SaunaFSClusterReconciler) execInPod(ctx context.Context, namespace, podName, containerName string, cmd []string) error {
	if r.RestConfig == nil {
		return fmt.Errorf("RestConfig not set on reconciler, cannot exec into pod %s", podName)
	}

	clientset, err := kubernetes.NewForConfig(r.RestConfig)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}

	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, clientgoscheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(r.RestConfig, http.MethodPost, req.URL())
	if err != nil {
		return fmt.Errorf("create SPDY executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return fmt.Errorf("exec %v in %s: %w\nstdout: %s\nstderr: %s",
			cmd, podName, err, stdout.String(), stderr.String())
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *SaunaFSClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&saunafsv1alpha1.SaunaFSCluster{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		// Note: we intentionally do NOT watch the HA Lease here.
		// The operator renews the Lease every leaseRenewInterval via RequeueAfter.
		// Watching Lease changes would create a feedback loop: each renewal would
		// trigger an immediate reconcile, doubling the reconcile rate.
		Complete(r)
}
