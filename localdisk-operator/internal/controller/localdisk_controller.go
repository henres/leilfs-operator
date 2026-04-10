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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	diskv1alpha1 "github.com/henres/localdisk-operator/api/v1alpha1"
)

// LocalDiskReconciler reconciles a LocalDisk object.
// Its sole responsibility is to create/delete the PersistentVolume when a
// disk reaches Ready state. All disk discovery and formatting is handled by
// the disk-agent DaemonSet which updates the LocalDisk status directly.
type LocalDiskReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// MountBaseDir is the base directory where the agent bind-mounts disks,
	// and where local-static-provisioner looks for volumes.
	// Default: /mnt/localdisk
	MountBaseDir string
}

//+kubebuilder:rbac:groups=disk.localdisk-operator.io,resources=localdisks,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=disk.localdisk-operator.io,resources=localdisks/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=disk.localdisk-operator.io,resources=localdisks/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=persistentvolumes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch

// Reconcile ensures a PersistentVolume exists for every Ready LocalDisk,
// and removes it if the disk is no longer Ready.
func (r *LocalDiskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	disk := &diskv1alpha1.LocalDisk{}
	if err := r.Get(ctx, req.NamespacedName, disk); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	pvName := disk.Status.UUID
	if pvName == "" {
		// No UUID yet — disk not formatted, nothing to do.
		return ctrl.Result{}, nil
	}

	if disk.Status.State == diskv1alpha1.LocalDiskStateReady {
		return r.reconcileReadyDisk(ctx, disk, pvName)
	}

	// Disk is no longer Ready — clean up the PV only if NO other LocalDisk
	// with the same UUID is in Ready state. This prevents a non-Ready CR on
	// one node from deleting the PV that was legitimately created by the
	// Ready CR on another node (common in Kind where nodes share /dev).
	return r.deleteOrphanPV(ctx, logger, disk.Name, pvName)
}

func (r *LocalDiskReconciler) reconcileReadyDisk(ctx context.Context, disk *diskv1alpha1.LocalDisk, pvName string) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	pv := &corev1.PersistentVolume{}
	err := r.Get(ctx, client.ObjectKey{Name: pvName}, pv)
	if err == nil {
		// PV already exists — nothing to do.
		return ctrl.Result{}, nil
	}
	if !errors.IsNotFound(err) {
		return ctrl.Result{}, err
	}

	// PV does not exist — create it.
	mountPath := disk.Status.MountPath
	if mountPath == "" {
		mountPath = fmt.Sprintf("%s/%s", r.mountBaseDir(), pvName)
	}

	storageSize := resource.NewQuantity(disk.Status.SizeBytes, resource.BinarySI)
	hostPathType := corev1.HostPathDirectory
	volumeMode := corev1.PersistentVolumeFilesystem
	reclaimPolicy := corev1.PersistentVolumeReclaimRetain

	newPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"localdisk-operator.io/disk": disk.Name,
				"localdisk-operator.io/node": disk.Status.Node,
			},
			Annotations: map[string]string{
				"localdisk-operator.io/device": disk.Status.Device,
				"localdisk-operator.io/serial": disk.Status.Serial,
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: *storageSize},
			StorageClassName:              "",
			VolumeMode:                    &volumeMode,
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeReclaimPolicy: reclaimPolicy,
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{
					Path: mountPath,
					Type: &hostPathType,
				},
			},
			NodeAffinity: &corev1.VolumeNodeAffinity{
				Required: &corev1.NodeSelector{
					NodeSelectorTerms: []corev1.NodeSelectorTerm{
						{
							MatchExpressions: []corev1.NodeSelectorRequirement{
								{
									Key:      "kubernetes.io/hostname",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{disk.Status.Node},
								},
							},
						},
					},
				},
			},
		},
	}

	if err := r.Create(ctx, newPV); err != nil {
		return ctrl.Result{}, fmt.Errorf("creating PV %s: %w", pvName, err)
	}
	logger.Info("Created PersistentVolume", "pv", pvName, "node", disk.Status.Node, "disk", disk.Name)
	return ctrl.Result{}, nil
}

func (r *LocalDiskReconciler) deleteOrphanPV(ctx context.Context, logger interface{ Info(string, ...any) }, diskName string, pvName string) (ctrl.Result, error) {
	pv := &corev1.PersistentVolume{}
	if err := r.Get(ctx, client.ObjectKey{Name: pvName}, pv); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Before deleting, check whether another LocalDisk CR with the same
	// UUID is in Ready state. If so, the PV is still valid and must not
	// be deleted. This handles the Kind scenario where multiple nodes
	// see the same physical device and share a UUID.
	allDisks := &diskv1alpha1.LocalDiskList{}
	if err := r.List(ctx, allDisks); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing LocalDisks: %w", err)
	}
	for i := range allDisks.Items {
		d := &allDisks.Items[i]
		if d.Name == diskName {
			continue // skip the current (non-Ready) disk
		}
		if d.Status.UUID == pvName && d.Status.State == diskv1alpha1.LocalDiskStateReady {
			logger.Info("Skipping PV deletion: another LocalDisk with same UUID is Ready",
				"pv", pvName, "readyDisk", d.Name, "triggeringDisk", diskName)
			return ctrl.Result{}, nil
		}
	}

	if err := r.Delete(ctx, pv); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("deleting PV %s: %w", pvName, err)
	}
	logger.Info("Deleted PersistentVolume for non-Ready disk", "pv", pvName, "disk", diskName)
	return ctrl.Result{}, nil
}

func (r *LocalDiskReconciler) mountBaseDir() string {
	if r.MountBaseDir != "" {
		return r.MountBaseDir
	}
	return "/mnt/localdisk"
}

// SetupWithManager sets up the controller with the Manager.
func (r *LocalDiskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&diskv1alpha1.LocalDisk{}).
		Complete(r)
}
