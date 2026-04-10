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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	diskv1alpha1 "github.com/henres/localdisk-operator/api/v1alpha1"
)

const (
	// daemonSetName is the name of the managed agent DaemonSet.
	daemonSetName = "localdisk-agent"

	// configSingletonName is the expected name for the singleton config resource.
	configSingletonName = "default"
)

// LocalDiskConfigReconciler reconciles a LocalDiskConfig object.
// It manages the disk-agent DaemonSet, applying nodeSelector and
// tolerations from the config spec.
type LocalDiskConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// AgentImage is the default container image for the agent.
	// Can be overridden by spec.agent.image in the config.
	AgentImage string

	// Namespace is the namespace where the agent DaemonSet is deployed.
	Namespace string
}

//+kubebuilder:rbac:groups=disk.localdisk-operator.io,resources=localdiskconfigs,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=disk.localdisk-operator.io,resources=localdiskconfigs/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=disk.localdisk-operator.io,resources=localdiskconfigs/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=daemonsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch

// Reconcile ensures the agent DaemonSet matches the desired state from
// the LocalDiskConfig singleton.
func (r *LocalDiskConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// We only care about the singleton named "default".
	if req.Name != configSingletonName {
		logger.Info("Ignoring non-default LocalDiskConfig", "name", req.Name)
		return ctrl.Result{}, nil
	}

	config := &diskv1alpha1.LocalDiskConfig{}
	if err := r.Get(ctx, req.NamespacedName, config); err != nil {
		if errors.IsNotFound(err) {
			// Config deleted — we could delete the DaemonSet, but it's
			// safer to leave it running. The user can delete it manually.
			logger.Info("LocalDiskConfig deleted, leaving DaemonSet in place")
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Ensure the ServiceAccount exists.
	if err := r.ensureServiceAccount(ctx, config); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring ServiceAccount: %w", err)
	}

	// Build and apply the desired DaemonSet.
	desired := r.buildDaemonSet(config)

	// Set the config as the owner so the DaemonSet is garbage-collected
	// if the config is deleted.
	if err := controllerutil.SetControllerReference(config, desired, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("setting owner reference: %w", err)
	}

	existing := &appsv1.DaemonSet{}
	err := r.Get(ctx, types.NamespacedName{Name: daemonSetName, Namespace: r.Namespace}, existing)

	if errors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil {
			return ctrl.Result{}, fmt.Errorf("creating DaemonSet: %w", err)
		}
		logger.Info("Created agent DaemonSet", "name", daemonSetName)
	} else if err != nil {
		return ctrl.Result{}, err
	} else {
		// Update the existing DaemonSet spec.
		existing.Spec = desired.Spec
		existing.Labels = desired.Labels
		if err := r.Update(ctx, existing); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating DaemonSet: %w", err)
		}
		logger.Info("Updated agent DaemonSet", "name", daemonSetName)
	}

	// Update status.
	return r.updateStatus(ctx, config)
}

// ensureServiceAccount creates the agent ServiceAccount if it doesn't exist.
func (r *LocalDiskConfigReconciler) ensureServiceAccount(ctx context.Context, config *diskv1alpha1.LocalDiskConfig) error {
	sa := &corev1.ServiceAccount{}
	err := r.Get(ctx, types.NamespacedName{Name: daemonSetName, Namespace: r.Namespace}, sa)
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	sa = &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      daemonSetName,
			Namespace: r.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "localdisk-agent",
				"app.kubernetes.io/component":  "agent",
				"app.kubernetes.io/managed-by": "localdisk-operator",
			},
		},
	}
	if err := controllerutil.SetControllerReference(config, sa, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, sa)
}

// buildDaemonSet constructs the desired DaemonSet from the config.
func (r *LocalDiskConfigReconciler) buildDaemonSet(config *diskv1alpha1.LocalDiskConfig) *appsv1.DaemonSet {
	agentImage := r.AgentImage
	if config.Spec.Agent.Image != "" {
		agentImage = config.Spec.Agent.Image
	}

	// Build tolerations — default to tolerate everything if not specified.
	tolerations := r.buildTolerations(config)

	hostPathDirectory := corev1.HostPathDirectory
	hostPathDirectoryOrCreate := corev1.HostPathDirectoryOrCreate
	privileged := true

	// Build agent args from config.
	agentArgs := []string{"--mount-base-dir=/mnt/localdisk"}
	if config.Spec.Agent.IncludeLoopDevices {
		agentArgs = append(agentArgs, "--include-loop-devices")
	}
	if config.Spec.Agent.ScanInterval != "" {
		agentArgs = append(agentArgs, "--scan-interval="+config.Spec.Agent.ScanInterval)
	}

	ds := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      daemonSetName,
			Namespace: r.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "localdisk-agent",
				"app.kubernetes.io/component":  "agent",
				"app.kubernetes.io/managed-by": "localdisk-operator",
			},
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app.kubernetes.io/name": "localdisk-agent",
				},
			},
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type: appsv1.RollingUpdateDaemonSetStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":      "localdisk-agent",
						"app.kubernetes.io/component": "agent",
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: daemonSetName,
					NodeSelector:       config.Spec.Agent.NodeSelector,
					Tolerations:        tolerations,
					Containers: []corev1.Container{
						{
							Name:            "agent",
							Image:           agentImage,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/agent"},
							Args:            agentArgs,
							Env: []corev1.EnvVar{
								{
									Name: "NODE_NAME",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "spec.nodeName",
										},
									},
								},
							},
							SecurityContext: &corev1.SecurityContext{
								Privileged: &privileged,
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("10m"),
									corev1.ResourceMemory: resource.MustParse("32Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("64Mi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "dev",
									MountPath: "/dev",
								},
								{
									Name:      "sys",
									MountPath: "/sys",
									ReadOnly:  true,
								},
								{
									Name:             "localdisk-mount-base",
									MountPath:        "/mnt/localdisk",
									MountPropagation: mountPropagationPtr(corev1.MountPropagationBidirectional),
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "dev",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/dev",
									Type: &hostPathDirectory,
								},
							},
						},
						{
							Name: "sys",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/sys",
									Type: &hostPathDirectory,
								},
							},
						},
						{
							Name: "localdisk-mount-base",
							VolumeSource: corev1.VolumeSource{
								HostPath: &corev1.HostPathVolumeSource{
									Path: "/mnt/localdisk",
									Type: &hostPathDirectoryOrCreate,
								},
							},
						},
					},
				},
			},
		},
	}

	return ds
}

// buildTolerations converts the config tolerations to corev1.Toleration.
// If no tolerations are specified, defaults to tolerating all taints.
func (r *LocalDiskConfigReconciler) buildTolerations(config *diskv1alpha1.LocalDiskConfig) []corev1.Toleration {
	if len(config.Spec.Agent.Tolerations) > 0 {
		var tolerations []corev1.Toleration
		for _, t := range config.Spec.Agent.Tolerations {
			tolerations = append(tolerations, corev1.Toleration{
				Key:      t.Key,
				Operator: corev1.TolerationOperator(t.Operator),
				Value:    t.Value,
				Effect:   corev1.TaintEffect(t.Effect),
			})
		}
		return tolerations
	}

	// Default: tolerate all taints so the agent runs on every node.
	return []corev1.Toleration{
		{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
		{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	}
}

// updateStatus patches the LocalDiskConfig status with DaemonSet information.
func (r *LocalDiskConfigReconciler) updateStatus(ctx context.Context, config *diskv1alpha1.LocalDiskConfig) (ctrl.Result, error) {
	ds := &appsv1.DaemonSet{}
	err := r.Get(ctx, types.NamespacedName{Name: daemonSetName, Namespace: r.Namespace}, ds)

	original := config.DeepCopy()
	config.Status.AgentDaemonSetName = daemonSetName
	config.Status.ObservedGeneration = config.Generation

	if err == nil {
		config.Status.AgentDaemonSetReady = ds.Status.DesiredNumberScheduled > 0 &&
			ds.Status.NumberReady == ds.Status.DesiredNumberScheduled
	} else {
		config.Status.AgentDaemonSetReady = false
	}

	if patchErr := r.Status().Patch(ctx, config, client.MergeFrom(original)); patchErr != nil {
		return ctrl.Result{}, fmt.Errorf("patching status: %w", patchErr)
	}
	return ctrl.Result{}, nil
}

func mountPropagationPtr(m corev1.MountPropagationMode) *corev1.MountPropagationMode {
	return &m
}

// SetupWithManager sets up the controller with the Manager.
func (r *LocalDiskConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&diskv1alpha1.LocalDiskConfig{}).
		Owns(&appsv1.DaemonSet{}).
		Complete(r)
}
