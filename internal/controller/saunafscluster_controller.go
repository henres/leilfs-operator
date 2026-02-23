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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
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

	if err := r.reconcileMasterDaemonSet(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile master daemonset: %w", err)
	}
	if err := r.reconcileMasterService(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile master service: %w", err)
	}
	if err := r.reconcileChunkServers(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile chunk servers: %w", err)
	}
	if err := r.reconcileInterface(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile interface: %w", err)
	}
	if err := r.reconcileExposeService(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile expose service: %w", err)
	}
	if err := r.reconcileNFS(ctx, cluster); err != nil {
		return ctrl.Result{}, fmt.Errorf("reconcile nfs: %w", err)
	}

	return ctrl.Result{}, nil
}

// ── Master DaemonSet ────────────────────────────────────────────────────────

func (r *SaunaFSClusterReconciler) reconcileMasterDaemonSet(ctx context.Context, cluster *saunafsv1alpha1.SaunaFSCluster) error {
	name := fmt.Sprintf("%s-master", cluster.Name)
	image := cluster.Spec.Master.Image
	if image == "" {
		image = "saunafs-master:latest"
	}

	ports := masterContainerPorts(cluster)

	// emptyDir shared between init and main container for /var/lib/saunafs
	dataVolume := corev1.Volume{
		Name:         "saunafs-data",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}
	dataMount := corev1.VolumeMount{
		Name:      "saunafs-data",
		MountPath: "/var/lib/saunafs",
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
					Volumes:      []corev1.Volume{dataVolume},
					// Init container pre-populates metadata.sfs so the start
					// script does not skip initialisation because the directory
					// already contains metadata.sfs.empty from the image layer.
					InitContainers: []corev1.Container{
						{
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
							VolumeMounts:    []corev1.VolumeMount{dataMount},
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
		ganeshaImage = "izdock/nfs-ganesha:latest"
	}
	// saunafs-client image: reuse the same image as chunk servers if set,
	// otherwise fall back to saunafs-client:latest.
	clientImage := cluster.Spec.Chunk.Image
	if clientImage == "" {
		clientImage = "saunafs-client:latest"
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

	privileged := true
	bidir := corev1.MountPropagationBidirectional
	hostToContainer := corev1.MountPropagationHostToContainer

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
					// FUSE mounts need to cross container boundaries:
					// saunafs-client mounts /exports (Bidirectional) so the
					// kernel propagates it to the host and back into the
					// nfs-ganesha container (HostToContainer).
					InitContainers: []corev1.Container{
						{
							// Ensures /exports exists before saunafs-mount runs.
							Name:            "init-exports",
							Image:           "busybox:latest",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"sh", "-c", "mkdir -p /exports"},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "exports", MountPath: "/exports", MountPropagation: &bidir},
							},
							SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
						},
					},
					Containers: []corev1.Container{
						{
							// saunafs-client mounts the SaunaFS filesystem via
							// FUSE into the shared /exports volume.
							Name:            "saunafs-client",
							Image:           clientImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							// Keep the container alive after mounting.
							// saunafs-mount forks into the background then the
							// sidecar sleeps indefinitely to hold the process.
							Command: []string{
								"sh", "-c",
								fmt.Sprintf(
									"saunafs-mount -H %s -P 9421 /exports -o big_writes,nosuid,nodev && sleep infinity",
									masterHost,
								),
							},
							SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "exports", MountPath: "/exports", MountPropagation: &bidir},
							},
						},
						{
							// NFS-Ganesha re-exports /exports over NFS using
							// the VFS FSAL (no SaunaFS-specific build needed).
							// Configuration is done entirely via env vars as
							// documented by izdock/nfs-ganesha.
							Name:            "nfs-ganesha",
							Image:           ganeshaImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Resources:       nfs.Resources,
							Env: []corev1.EnvVar{
								{Name: "EXPORT_PATH", Value: "/exports"},
								{Name: "PSEUDO_PATH", Value: "/"},
								{Name: "SQUASH_MODE", Value: squash},
								{Name: "PROTOCOLS", Value: "3,4"},
								{Name: "TRANSPORTS", Value: "TCP"},
								{Name: "ACCESS_TYPE", Value: "RW"},
								// Allow all private ranges by default; expose
								// can still be restricted at the firewall level.
								{Name: "CLIENT_LIST", Value: "0.0.0.0/0"},
								{Name: "LOG_LEVEL", Value: "WARN"},
							},
							SecurityContext: &corev1.SecurityContext{Privileged: &privileged},
							Ports: []corev1.ContainerPort{
								{Name: "nfs", ContainerPort: 2049, Protocol: corev1.ProtocolTCP},
								{Name: "rpcbind-tcp", ContainerPort: 111, Protocol: corev1.ProtocolTCP},
								{Name: "rpcbind-udp", ContainerPort: 111, Protocol: corev1.ProtocolUDP},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "exports", MountPath: "/exports", MountPropagation: &hostToContainer},
								{Name: "run", MountPath: "/run"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							// Shared FUSE mount point between saunafs-client
							// and nfs-ganesha.
							Name:         "exports",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
						},
						{
							Name:         "run",
							VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
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
			Ports:    []corev1.ServicePort{nfsPort},
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
