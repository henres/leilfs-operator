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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	saunafsv1alpha1 "github.com/henres/saunafs-operator/api/v1alpha1"
)

// SaunaFSClusterReconciler reconciles a SaunaFSCluster object
type SaunaFSClusterReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=saunafs.saunafs-operator.io,resources=saunafsclusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=saunafs.saunafs-operator.io,resources=saunafsclusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=saunafs.saunafs-operator.io,resources=saunafsclusters/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=daemonsets;statefulsets;deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims;configmaps,verbs=get;list;watch;create;update;patch;delete

func (r *SaunaFSClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	cluster := &saunafsv1alpha1.SaunaFSCluster{}
	if err := r.Get(ctx, req.NamespacedName, cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling SaunaFSCluster", "name", cluster.Name)

	// Run all sub-reconcilers; on first error record a Failed condition.
	var reconcileErr error
	steps := []struct {
		name string
		fn   func(context.Context, *saunafsv1alpha1.SaunaFSCluster) error
	}{
		{"goals configmap", r.reconcileGoalsConfigMap},
		{"master metadata pvc", r.reconcileMasterPVC},
		{"master daemonset", r.reconcileMasterDaemonSet},
		{"master service", r.reconcileMasterService},
		{"chunk servers", r.reconcileChunkServers},
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

	// Refresh cluster object before patching status to avoid conflicts.
	patch := client.MergeFrom(cluster.DeepCopy())

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

	if err := r.Status().Patch(ctx, cluster, patch); err != nil {
		logger.Error(err, "Failed to update SaunaFSCluster status")
		return ctrl.Result{}, err
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

// ── Master DaemonSet ────────────────────────────────────────────────────────

// ── Goals ConfigMap ──────────────────────────────────────────────────────────

// reconcileGoalsConfigMap creates or updates a ConfigMap that holds the
// generated sfsgoals.cfg file. The file is mounted into the master DaemonSet
// as a SubPath volume so only /etc/saunafs/sfsgoals.cfg is overridden.
//
// The ConfigMap carries two keys:
//   - sfsgoals.cfg  — mounted automatically by the master DaemonSet.
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
//   - Replication goal: pattern is N space-separated underscores.
//   - EC goal:          pattern is $ec(<dataParts>,<parityParts>).
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
		case g.Replication != nil:
			copies := make([]string, *g.Replication)
			for i := range copies {
				copies[i] = "_"
			}
			pattern = strings.Join(copies, " ")
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

// reconcileMasterPVC ensures the PersistentVolumeClaim that backs the master
// metadata directory (/var/lib/saunafs) exists.
//
// IMPORTANT: the PVC is deliberately NOT given an ownerReference to the
// SaunaFSCluster CR.  If it were, Kubernetes garbage collection would delete
// the PVC (and all metadata) whenever the CR is deleted — e.g. during a
// kind-undeploy / kind-deploy cycle.  The PVC must outlive the CR so that a
// fresh deployment of the same cluster name re-attaches to the existing
// metadata.  Manual deletion is required for a full reset (make kind-reset).
func (r *SaunaFSClusterReconciler) reconcileMasterPVC(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
	pvcName := fmt.Sprintf("%s-master-metadata", cluster.Name)

	// Determine storage size and class from spec (or use defaults).
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

	desired := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: cluster.Namespace,
			// Label so the PVC is identifiable as managed by this operator,
			// but no ownerReference — see function comment above.
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "saunafs-operator",
				"app.kubernetes.io/instance":   cluster.Name,
				"app.kubernetes.io/component":  "master-metadata",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			StorageClassName: storageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storageSize,
				},
			},
		},
	}
	// No SetControllerReference — intentional, see function comment.

	existing := &corev1.PersistentVolumeClaim{}
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: cluster.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	// PVC already exists — do not update (resizing requires special handling
	// and storage class changes are not supported in-place).
	return err
}

func (r *SaunaFSClusterReconciler) reconcileMasterDaemonSet(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
	name := fmt.Sprintf("%s-master", cluster.Name)
	image := cluster.Spec.Master.Image
	if image == "" {
		image = "saunafs-master:latest"
	}

	ports := masterContainerPorts(cluster)

	// PVC-backed volume for /var/lib/saunafs (metadata, shared with init-metadata).
	// The PVC is reconciled by reconcileMasterPVC before this function runs.
	pvcName := fmt.Sprintf("%s-master-metadata", cluster.Name)
	dataVolume := corev1.Volume{
		Name: "saunafs-data",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvcName,
			},
		},
	}
	dataMount := corev1.VolumeMount{
		Name:      "saunafs-data",
		MountPath: "/var/lib/saunafs",
	}

	// emptyDir for /etc/saunafs — pre-populated by init-config so that the
	// master startup script finds sfsmaster.cfg already present and does NOT
	// attempt to copy defaults (which would fail when overlaying read-only
	// files such as our goals ConfigMap SubPath mount).
	cfgVolume := corev1.Volume{
		Name:         "saunafs-cfg",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	cfgMount := corev1.VolumeMount{
		Name:      "saunafs-cfg",
		MountPath: "/etc/saunafs",
	}

	// Volumes and init-config command shared by both code paths.
	volumes := []corev1.Volume{dataVolume, cfgVolume}

	// init-config: seed /etc/saunafs with packaged defaults, then overlay
	// the operator-generated sfsgoals.cfg when the goals ConfigMap exists.
	// The ConfigMap is mounted at /etc/saunafs-goals/ (read-only directory)
	// and is copied into the writable emptyDir — avoiding any read-only
	// filesystem error during the main container startup script.
	initConfigCmd := "cp -r /usr/share/doc/saunafs-master/examples/. /etc/saunafs/"
	initConfigMounts := []corev1.VolumeMount{cfgMount}

	if len(cluster.Spec.Goals) > 0 {
		cmName := fmt.Sprintf("%s-master-goals", cluster.Name)
		volumes = append(volumes, corev1.Volume{
			Name: "saunafs-goals",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
				},
			},
		})
		// Staging mount: ConfigMap directory → /etc/saunafs-goals/
		// (read-only is fine here; init-config copies the file out).
		initConfigMounts = append(initConfigMounts, corev1.VolumeMount{
			Name:      "saunafs-goals",
			MountPath: "/etc/saunafs-goals",
			ReadOnly:  true,
		})
		initConfigCmd += " && cp /etc/saunafs-goals/sfsgoals.cfg /etc/saunafs/sfsgoals.cfg"
	}

	desired := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name":     "saunafs-master",
					"app.kubernetes.io/instance": cluster.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":     "saunafs-master",
						"app.kubernetes.io/instance": cluster.Name,
					},
				},
				Spec: corev1.PodSpec{
					NodeSelector: cluster.Spec.Master.NodeSelector,
					Tolerations:  cluster.Spec.Master.Tolerations,
					Volumes:      volumes,
					InitContainers: []corev1.Container{
						{
							// Seed /etc/saunafs/ with packaged example configs
							// (includes sfsmaster.cfg, sfsexports.cfg, sfsgoals.cfg, …)
							// so the main container startup script skips the copy
							// step entirely, preventing read-only filesystem errors.
							// When custom goals are defined the operator-generated
							// sfsgoals.cfg is overlaid from the staging mount.
							Name:            "init-config",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", initConfigCmd},
							VolumeMounts:    initConfigMounts,
						},
						{
							// Pre-populate metadata.sfs so the master start
							// script does not skip initialisation.
							Name:            "init-metadata",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"sh", "-c",
								"cp -n /opt/saunafs/templates/metadata.sfs.empty /var/lib/saunafs/metadata.sfs || true",
							},
							VolumeMounts: []corev1.VolumeMount{dataMount},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "saunafs-master",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Ports:           ports,
							Resources:       cluster.Spec.Master.Resources,
							VolumeMounts:    []corev1.VolumeMount{dataMount, cfgMount},
						},
					},
				},
			},
		},
	}
	if err := ctrl.SetControllerReference(cluster, desired, r.Scheme); err != nil {
		return err
	}
	return r.createOrUpdateDaemonSet(ctx, desired)
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

	desired := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: cluster.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type: svcType,
			Selector: map[string]string{
				"app.kubernetes.io/name":     "saunafs-master",
				"app.kubernetes.io/instance": cluster.Name,
			},
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
		if err := r.reconcileChunkStatefulSet(ctx, cluster, srv); err != nil {
			return fmt.Errorf("chunk server %s: %w", srv.Name, err)
		}
	}
	return nil
}

func (r *SaunaFSClusterReconciler) reconcileChunkStatefulSet(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster, srv *saunafsv1alpha1.ChunkServerSpec) error {
	name := fmt.Sprintf("%s-chunk-%s", cluster.Name, srv.Name)

	image := srv.Image
	if image == "" {
		image = cluster.Spec.Chunk.Image
	}
	if image == "" {
		image = "saunafs-chunkserver:latest"
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
	allMounts := append(dataVolumeMounts, cfgMount)

	chunkPorts := chunkContainerPorts(cluster)

	// Pre-create sfschunkserver.cfg with the correct MASTER_HOST.
	// The start script only rewrites the *commented* line
	// ("# MASTER_HOST = sfsmaster") so our already-uncommented value survives.
	initCmd := fmt.Sprintf(
		`mkdir -p /etc/saunafs && `+
			`cp /usr/share/doc/saunafs-chunkserver/examples/sfschunkserver.cfg /etc/saunafs/sfschunkserver.cfg && `+
			`sed -i 's/^# *MASTER_HOST *= *sfsmaster/MASTER_HOST = %s/' /etc/saunafs/sfschunkserver.cfg`,
		masterHost,
	)

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
		image = "saunafs-cgiserver:latest"
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
		ganeshaImage = "nfs-ganesha:latest"
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

// ── helpers ─────────────────────────────────────────────────────────────────

func (r *SaunaFSClusterReconciler) createOrUpdateDaemonSet(ctx context.Context, desired *appsv1.DaemonSet) error {
	existing := &appsv1.DaemonSet{}
	err := r.Get(ctx, types.NamespacedName{Name: desired.Name, Namespace: desired.Namespace}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec = desired.Spec
	return r.Update(ctx, existing)
}

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

// SetupWithManager sets up the controller with the Manager.
func (r *SaunaFSClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&saunafsv1alpha1.SaunaFSCluster{}).
		Owns(&appsv1.DaemonSet{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
