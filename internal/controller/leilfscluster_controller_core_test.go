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

// This file covers reconcileGoalsConfigMap, reconcileMasterService,
// reconcileMetaloggers, reconcileInterface, reconcileExposeService and
// reconcileNFS in isolation, calling each sub-reconciler method directly
// rather than the full Reconcile() loop. This keeps these specs independent
// from the other eight sub-reconcilers (master StatefulSet/HA, chunk
// servers, …) which are covered elsewhere.

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	saunafsv1alpha1 "github.com/henres/leilfs-operator/api/v1alpha1"
)

var _ = Describe("LeilFSCluster core sub-reconcilers", func() {
	ctx := context.Background()

	newReconciler := func() *LeilFSClusterReconciler {
		return &LeilFSClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	int32Ptr := func(v int32) *int32 { return &v }
	boolPtr := func(v bool) *bool { return &v }

	// createCluster persists a minimal LeilFSCluster so that owner references
	// (which require a UID) can be set by the reconciler methods under test.
	createCluster := func(name string, mutate func(*saunafsv1alpha1.LeilFSCluster)) *saunafsv1alpha1.LeilFSCluster {
		cluster := &saunafsv1alpha1.LeilFSCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		}
		if mutate != nil {
			mutate(cluster)
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		return cluster
	}

	nn := func(name string) types.NamespacedName {
		return types.NamespacedName{Name: name, Namespace: "default"}
	}

	expectEventuallyExists := func(obj client.Object, key types.NamespacedName) {
		Eventually(func() error { return k8sClient.Get(ctx, key, obj) }).Should(Succeed())
	}

	expectEventuallyGone := func(newObj func() client.Object, key types.NamespacedName) {
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, newObj()))
		}).Should(BeTrue())
	}

	// findPort locates a ServicePort by name. NodePort Services get an
	// API-server-assigned NodePort whenever the spec doesn't request one
	// explicitly, so callers compare the other fields and only assert on
	// NodePort when the test itself requested a specific value.
	findPort := func(ports []corev1.ServicePort, name string) corev1.ServicePort {
		for _, p := range ports {
			if p.Name == name {
				return p
			}
		}
		Fail(fmt.Sprintf("service port %q not found in %#v", name, ports))
		return corev1.ServicePort{}
	}

	// ── reconcileGoalsConfigMap ──────────────────────────────────────────────

	Describe("reconcileGoalsConfigMap", func() {
		It("does nothing when spec.goals is empty", func() {
			cluster := createCluster("core-goals-empty", nil)
			Expect(newReconciler().reconcileGoalsConfigMap(ctx, cluster)).To(Succeed())

			cm := &corev1.ConfigMap{}
			err := k8sClient.Get(ctx, nn("core-goals-empty-master-goals"), cm)
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("creates a ConfigMap rendering replication, node-pinned and EC goals", func() {
			cluster := createCluster("core-goals-create", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Goals = []saunafsv1alpha1.GoalSpec{
					{ID: 2, Name: "two_copies", Replication: int32Ptr(2)},
					{ID: 11, Name: "node_spread", Replication: int32Ptr(3), NodeLabels: []string{"node1", "node2", "node3"}},
					{ID: 10, Name: "ec_4_2", EC: &saunafsv1alpha1.ECSpec{DataParts: 4, ParityParts: 2}, Default: true},
				}
			})

			Expect(newReconciler().reconcileGoalsConfigMap(ctx, cluster)).To(Succeed())

			cm := &corev1.ConfigMap{}
			expectEventuallyExists(cm, nn("core-goals-create-master-goals"))

			content := cm.Data["sfsgoals.cfg"]
			Expect(content).To(ContainSubstring("two_copies"))
			Expect(content).To(ContainSubstring("_ _")) // anonymous replication pattern
			Expect(content).To(ContainSubstring("node1 node2 node3"))
			Expect(content).To(ContainSubstring("$ec(4,2)"))

			Expect(cm.Data).To(HaveKey("sfsmaster-default-goal.txt"))
			Expect(cm.Data["sfsmaster-default-goal.txt"]).To(ContainSubstring("SFSMASTER_DEFAULT_GOAL = ec_4_2"))

			Expect(cm.OwnerReferences).To(HaveLen(1))
			Expect(cm.OwnerReferences[0].Name).To(Equal(cluster.Name))
		})

		It("is idempotent across repeated reconciles", func() {
			cluster := createCluster("core-goals-idempotent", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Goals = []saunafsv1alpha1.GoalSpec{{ID: 2, Name: "two_copies", Replication: int32Ptr(2)}}
			})
			r := newReconciler()
			Expect(r.reconcileGoalsConfigMap(ctx, cluster)).To(Succeed())

			first := &corev1.ConfigMap{}
			expectEventuallyExists(first, nn("core-goals-idempotent-master-goals"))

			Expect(r.reconcileGoalsConfigMap(ctx, cluster)).To(Succeed())

			second := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("core-goals-idempotent-master-goals"), second)).To(Succeed())
			Expect(second.UID).To(Equal(first.UID))
			Expect(second.Data).To(Equal(first.Data))
		})

		It("deletes the ConfigMap once spec.goals is cleared", func() {
			cluster := createCluster("core-goals-clear", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Goals = []saunafsv1alpha1.GoalSpec{{ID: 2, Name: "two_copies", Replication: int32Ptr(2)}}
			})
			r := newReconciler()
			Expect(r.reconcileGoalsConfigMap(ctx, cluster)).To(Succeed())
			expectEventuallyExists(&corev1.ConfigMap{}, nn("core-goals-clear-master-goals"))

			cluster.Spec.Goals = nil
			Expect(r.reconcileGoalsConfigMap(ctx, cluster)).To(Succeed())

			expectEventuallyGone(func() client.Object { return &corev1.ConfigMap{} }, nn("core-goals-clear-master-goals"))
		})
	})

	// ── reconcileMasterService ───────────────────────────────────────────────

	Describe("reconcileMasterService", func() {
		It("creates a ClusterIP Service selecting master pods by name when HA is not configured", func() {
			cluster := createCluster("core-msvc-create", nil)
			Expect(newReconciler().reconcileMasterService(ctx, cluster)).To(Succeed())

			svc := &corev1.Service{}
			expectEventuallyExists(svc, nn("core-msvc-create-master"))

			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(svc.Spec.Selector).To(Equal(map[string]string{
				"app.kubernetes.io/name":     "leilfs-master",
				"app.kubernetes.io/instance": cluster.Name,
			}))
			Expect(svc.Spec.Ports).To(ConsistOf(
				corev1.ServicePort{Name: "admin", Port: 9419, TargetPort: intstr.FromInt(9419), Protocol: corev1.ProtocolTCP},
				corev1.ServicePort{Name: "cs", Port: 9420, TargetPort: intstr.FromInt(9420), Protocol: corev1.ProtocolTCP},
				corev1.ServicePort{Name: "client", Port: 9421, TargetPort: intstr.FromInt(9421), Protocol: corev1.ProtocolTCP},
			))
			Expect(svc.OwnerReferences).To(HaveLen(1))
		})

		It("is idempotent and preserves the assigned ClusterIP across reconciles", func() {
			cluster := createCluster("core-msvc-idempotent", nil)
			r := newReconciler()
			Expect(r.reconcileMasterService(ctx, cluster)).To(Succeed())

			first := &corev1.Service{}
			expectEventuallyExists(first, nn("core-msvc-idempotent-master"))
			Expect(first.Spec.ClusterIP).NotTo(BeEmpty())

			Expect(r.reconcileMasterService(ctx, cluster)).To(Succeed())

			second := &corev1.Service{}
			Expect(k8sClient.Get(ctx, nn("core-msvc-idempotent-master"), second)).To(Succeed())
			Expect(second.UID).To(Equal(first.UID))
			Expect(second.Spec.ClusterIP).To(Equal(first.Spec.ClusterIP))
		})

		It("switches the selector to the active-master label once Shadow is configured", func() {
			cluster := createCluster("core-msvc-ha", nil)
			r := newReconciler()
			Expect(r.reconcileMasterService(ctx, cluster)).To(Succeed())
			expectEventuallyExists(&corev1.Service{}, nn("core-msvc-ha-master"))

			cluster.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(1)}
			Expect(r.reconcileMasterService(ctx, cluster)).To(Succeed())

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, nn("core-msvc-ha-master"), svc)).To(Succeed())
			Expect(svc.Spec.Selector).To(Equal(map[string]string{
				"app.kubernetes.io/instance": cluster.Name,
				labelActiveMaster:            "true",
			}))
		})
	})

	// ── reconcileMetaloggers ─────────────────────────────────────────────────

	Describe("reconcileMetaloggers", func() {
		It("does nothing when spec.metalogger is nil", func() {
			cluster := createCluster("core-ml-nil", nil)
			Expect(newReconciler().reconcileMetaloggers(ctx, cluster)).To(Succeed())

			err := k8sClient.Get(ctx, nn("core-ml-nil-metalogger"), &appsv1.StatefulSet{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("creates the metalogger StatefulSet and headless Service", func() {
			cluster := createCluster("core-ml-create", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Metalogger = &saunafsv1alpha1.MetaloggerSpec{Replicas: int32Ptr(2)}
			})
			Expect(newReconciler().reconcileMetaloggers(ctx, cluster)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			expectEventuallyExists(sts, nn("core-ml-create-metalogger"))
			Expect(*sts.Spec.Replicas).To(Equal(int32(2)))
			Expect(sts.Spec.ServiceName).To(Equal("core-ml-create-metalogger"))
			Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))
			Expect(sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]).
				To(Equal(resource.MustParse("5Gi")))
			Expect(sts.OwnerReferences).To(HaveLen(1))

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, nn("core-ml-create-metalogger"), svc)).To(Succeed())
			Expect(svc.Spec.ClusterIP).To(Equal("None"))
			Expect(svc.Spec.Ports).To(ConsistOf(corev1.ServicePort{
				Name: "metalogger", Port: 9419, TargetPort: intstr.FromInt(9419), Protocol: corev1.ProtocolTCP,
			}))
		})

		It("is idempotent across repeated reconciles", func() {
			cluster := createCluster("core-ml-idempotent", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Metalogger = &saunafsv1alpha1.MetaloggerSpec{Replicas: int32Ptr(1)}
			})
			r := newReconciler()
			Expect(r.reconcileMetaloggers(ctx, cluster)).To(Succeed())

			first := &appsv1.StatefulSet{}
			expectEventuallyExists(first, nn("core-ml-idempotent-metalogger"))

			Expect(r.reconcileMetaloggers(ctx, cluster)).To(Succeed())

			second := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, nn("core-ml-idempotent-metalogger"), second)).To(Succeed())
			Expect(second.UID).To(Equal(first.UID))
		})

		It("honors a custom MetadataStorage size and StorageClassName", func() {
			cluster := createCluster("core-ml-storage", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Metalogger = &saunafsv1alpha1.MetaloggerSpec{
					Replicas: int32Ptr(1),
					MetadataStorage: &saunafsv1alpha1.MasterStorageSpec{
						Size:             resource.MustParse("10Gi"),
						StorageClassName: "fast-local",
					},
				}
			})
			Expect(newReconciler().reconcileMetaloggers(ctx, cluster)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			expectEventuallyExists(sts, nn("core-ml-storage-metalogger"))
			Expect(sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]).
				To(Equal(resource.MustParse("10Gi")))
			Expect(*sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName).To(Equal("fast-local"))
		})

		It("deletes the StatefulSet and Service once replicas is set back to 0", func() {
			cluster := createCluster("core-ml-disable", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Metalogger = &saunafsv1alpha1.MetaloggerSpec{Replicas: int32Ptr(1)}
			})
			r := newReconciler()
			Expect(r.reconcileMetaloggers(ctx, cluster)).To(Succeed())
			expectEventuallyExists(&appsv1.StatefulSet{}, nn("core-ml-disable-metalogger"))

			cluster.Spec.Metalogger.Replicas = int32Ptr(0)
			Expect(r.reconcileMetaloggers(ctx, cluster)).To(Succeed())

			expectEventuallyGone(func() client.Object { return &appsv1.StatefulSet{} }, nn("core-ml-disable-metalogger"))
			expectEventuallyGone(func() client.Object { return &corev1.Service{} }, nn("core-ml-disable-metalogger"))
		})
	})

	// ── reconcileInterface ───────────────────────────────────────────────────

	Describe("reconcileInterface", func() {
		It("does nothing when spec.interface.enabled is unset", func() {
			cluster := createCluster("core-iface-unset", nil)
			Expect(newReconciler().reconcileInterface(ctx, cluster)).To(Succeed())

			err := k8sClient.Get(ctx, nn("core-iface-unset-interface"), &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("creates a Deployment and Service with default image and port when enabled", func() {
			cluster := createCluster("core-iface-create", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.WebUI.Enabled = boolPtr(true)
			})
			Expect(newReconciler().reconcileInterface(ctx, cluster)).To(Succeed())

			dep := &appsv1.Deployment{}
			expectEventuallyExists(dep, nn("core-iface-create-interface"))
			Expect(*dep.Spec.Replicas).To(Equal(int32(1)))
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/henres/leilfs-container/leilfs-cgiserver:5.10.1"))
			Expect(dep.Spec.Template.Spec.Containers[0].Ports).To(ConsistOf(corev1.ContainerPort{Name: "http", ContainerPort: 9425, Protocol: corev1.ProtocolTCP}))
			Expect(dep.OwnerReferences).To(HaveLen(1))

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, nn("core-iface-create-interface"), svc)).To(Succeed())
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(svc.Spec.Ports).To(ConsistOf(corev1.ServicePort{
				Name: "http", Port: 9425, TargetPort: intstr.FromInt(9425), Protocol: corev1.ProtocolTCP,
			}))
		})

		It("honors custom image, port and replicas", func() {
			cluster := createCluster("core-iface-custom", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.WebUI = saunafsv1alpha1.WebUISpec{
					Enabled:  boolPtr(true),
					Image:    "example.com/custom-cgi:1.2.3",
					Port:     9000,
					Replicas: int32Ptr(3),
				}
			})
			Expect(newReconciler().reconcileInterface(ctx, cluster)).To(Succeed())

			dep := &appsv1.Deployment{}
			expectEventuallyExists(dep, nn("core-iface-custom-interface"))
			Expect(*dep.Spec.Replicas).To(Equal(int32(3)))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("example.com/custom-cgi:1.2.3"))
			Expect(dep.Spec.Template.Spec.Containers[0].Ports).To(ConsistOf(corev1.ContainerPort{Name: "http", ContainerPort: 9000, Protocol: corev1.ProtocolTCP}))
		})

		It("is idempotent across repeated reconciles", func() {
			cluster := createCluster("core-iface-idempotent", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.WebUI.Enabled = boolPtr(true)
			})
			r := newReconciler()
			Expect(r.reconcileInterface(ctx, cluster)).To(Succeed())

			first := &appsv1.Deployment{}
			expectEventuallyExists(first, nn("core-iface-idempotent-interface"))

			Expect(r.reconcileInterface(ctx, cluster)).To(Succeed())

			second := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, nn("core-iface-idempotent-interface"), second)).To(Succeed())
			Expect(second.UID).To(Equal(first.UID))
		})

		// Regression test: reconcileInterface must clean up its Deployment and
		// Service once spec.interface.enabled is toggled back to false, matching
		// the cleanup-on-disable behavior of every sibling toggle-based
		// sub-reconciler (reconcileExposeService, reconcileNFS,
		// reconcileMetaloggers). Before the fix, disabling the interface left
		// both objects running indefinitely.
		It("deletes the Deployment and Service once disabled again", func() {
			cluster := createCluster("core-iface-disable", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.WebUI.Enabled = boolPtr(true)
			})
			r := newReconciler()
			Expect(r.reconcileInterface(ctx, cluster)).To(Succeed())
			expectEventuallyExists(&appsv1.Deployment{}, nn("core-iface-disable-interface"))

			cluster.Spec.WebUI.Enabled = boolPtr(false)
			Expect(r.reconcileInterface(ctx, cluster)).To(Succeed())

			expectEventuallyGone(func() client.Object { return &appsv1.Deployment{} }, nn("core-iface-disable-interface"))
			expectEventuallyGone(func() client.Object { return &corev1.Service{} }, nn("core-iface-disable-interface"))
		})
	})

	// ── reconcileExposeService ───────────────────────────────────────────────

	Describe("reconcileExposeService", func() {
		It("does nothing when spec.expose.enabled is unset", func() {
			cluster := createCluster("core-expose-unset", nil)
			Expect(newReconciler().reconcileExposeService(ctx, cluster)).To(Succeed())

			err := k8sClient.Get(ctx, nn("core-expose-unset-client-expose"), &corev1.Service{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("creates a NodePort Service targeting the client port when enabled", func() {
			cluster := createCluster("core-expose-create", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Expose.Enabled = boolPtr(true)
			})
			Expect(newReconciler().reconcileExposeService(ctx, cluster)).To(Succeed())

			svc := &corev1.Service{}
			expectEventuallyExists(svc, nn("core-expose-create-client-expose"))
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
			Expect(svc.Spec.Selector).To(Equal(map[string]string{
				"app.kubernetes.io/name":     "leilfs-master",
				"app.kubernetes.io/instance": cluster.Name,
			}))
			Expect(svc.Spec.Ports).To(HaveLen(1))
			clientPort := findPort(svc.Spec.Ports, "client")
			Expect(clientPort.Port).To(Equal(int32(9421)))
			Expect(clientPort.TargetPort).To(Equal(intstr.FromInt(9421)))
			Expect(clientPort.Protocol).To(Equal(corev1.ProtocolTCP))
			Expect(clientPort.NodePort).To(BeNumerically(">", 0)) // auto-assigned by the API server
			Expect(svc.OwnerReferences).To(HaveLen(1))
		})

		It("is idempotent across repeated reconciles", func() {
			cluster := createCluster("core-expose-idempotent", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Expose.Enabled = boolPtr(true)
			})
			r := newReconciler()
			Expect(r.reconcileExposeService(ctx, cluster)).To(Succeed())

			first := &corev1.Service{}
			expectEventuallyExists(first, nn("core-expose-idempotent-client-expose"))

			Expect(r.reconcileExposeService(ctx, cluster)).To(Succeed())

			second := &corev1.Service{}
			Expect(k8sClient.Get(ctx, nn("core-expose-idempotent-client-expose"), second)).To(Succeed())
			Expect(second.UID).To(Equal(first.UID))
		})

		It("adds the admin NodePort when spec.expose.adminNodePort is set", func() {
			cluster := createCluster("core-expose-admin", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Expose.Enabled = boolPtr(true)
				c.Spec.Expose.AdminNodePort = 30500
			})
			Expect(newReconciler().reconcileExposeService(ctx, cluster)).To(Succeed())

			svc := &corev1.Service{}
			expectEventuallyExists(svc, nn("core-expose-admin-client-expose"))
			Expect(svc.Spec.Ports).To(HaveLen(2))
			adminPort := findPort(svc.Spec.Ports, "admin")
			Expect(adminPort.Port).To(Equal(int32(9419)))
			Expect(adminPort.TargetPort).To(Equal(intstr.FromInt(9419)))
			Expect(adminPort.Protocol).To(Equal(corev1.ProtocolTCP))
			Expect(adminPort.NodePort).To(Equal(int32(30500))) // explicitly requested
		})

		It("switches the selector to the active-master label once Shadow is configured", func() {
			cluster := createCluster("core-expose-ha", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Expose.Enabled = boolPtr(true)
				c.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(1)}
			})
			Expect(newReconciler().reconcileExposeService(ctx, cluster)).To(Succeed())

			svc := &corev1.Service{}
			expectEventuallyExists(svc, nn("core-expose-ha-client-expose"))
			Expect(svc.Spec.Selector).To(Equal(map[string]string{
				"app.kubernetes.io/instance": cluster.Name,
				labelActiveMaster:            "true",
			}))
		})

		It("deletes the Service once disabled again", func() {
			cluster := createCluster("core-expose-disable", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Expose.Enabled = boolPtr(true)
			})
			r := newReconciler()
			Expect(r.reconcileExposeService(ctx, cluster)).To(Succeed())
			expectEventuallyExists(&corev1.Service{}, nn("core-expose-disable-client-expose"))

			cluster.Spec.Expose.Enabled = boolPtr(false)
			Expect(r.reconcileExposeService(ctx, cluster)).To(Succeed())

			expectEventuallyGone(func() client.Object { return &corev1.Service{} }, nn("core-expose-disable-client-expose"))
		})
	})

	// ── reconcileNFS ─────────────────────────────────────────────────────────

	Describe("reconcileNFS", func() {
		It("does nothing when spec.nfs.enabled is unset", func() {
			cluster := createCluster("core-nfs-unset", nil)
			Expect(newReconciler().reconcileNFS(ctx, cluster)).To(Succeed())

			err := k8sClient.Get(ctx, nn("core-nfs-unset-nfs"), &appsv1.Deployment{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("creates the ConfigMap, Deployment and Service with defaults when enabled", func() {
			cluster := createCluster("core-nfs-create", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.NFS.Enabled = boolPtr(true)
			})
			Expect(newReconciler().reconcileNFS(ctx, cluster)).To(Succeed())

			cm := &corev1.ConfigMap{}
			expectEventuallyExists(cm, nn("core-nfs-create-nfs-ganesha-conf"))
			Expect(cm.Data["ganesha.conf"]).To(ContainSubstring(`Path                 = "/"`))
			Expect(cm.Data["ganesha.conf"]).To(ContainSubstring("Squash               = No_Root_Squash"))
			Expect(cm.Data["ganesha.conf"]).To(ContainSubstring(`hostname                 = "core-nfs-create-master.default.svc.cluster.local"`))

			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, nn("core-nfs-create-nfs"), dep)).To(Succeed())
			Expect(dep.Spec.Template.Spec.InitContainers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.InitContainers[0].Name).To(Equal("wait-for-master"))
			Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
			Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("ghcr.io/henres/leilfs-operator/nfs-ganesha:latest"))
			Expect(dep.OwnerReferences).To(HaveLen(1))

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, nn("core-nfs-create-nfs"), svc)).To(Succeed())
			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
			Expect(svc.Spec.Ports).To(HaveLen(3))
			nfsPort := findPort(svc.Spec.Ports, "nfs")
			Expect(nfsPort.Port).To(Equal(int32(2049)))
			Expect(nfsPort.TargetPort).To(Equal(intstr.FromInt(2049)))
			Expect(nfsPort.Protocol).To(Equal(corev1.ProtocolTCP))
			Expect(nfsPort.NodePort).To(BeNumerically(">", 0)) // auto-assigned by the API server
			rpcTCP := findPort(svc.Spec.Ports, "rpcbind-tcp")
			Expect(rpcTCP.Port).To(Equal(int32(111)))
			Expect(rpcTCP.Protocol).To(Equal(corev1.ProtocolTCP))
			rpcUDP := findPort(svc.Spec.Ports, "rpcbind-udp")
			Expect(rpcUDP.Port).To(Equal(int32(111)))
			Expect(rpcUDP.Protocol).To(Equal(corev1.ProtocolUDP))
		})

		It("is idempotent across repeated reconciles", func() {
			cluster := createCluster("core-nfs-idempotent", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.NFS.Enabled = boolPtr(true)
			})
			r := newReconciler()
			Expect(r.reconcileNFS(ctx, cluster)).To(Succeed())

			firstDep := &appsv1.Deployment{}
			expectEventuallyExists(firstDep, nn("core-nfs-idempotent-nfs"))
			firstCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("core-nfs-idempotent-nfs-ganesha-conf"), firstCM)).To(Succeed())

			Expect(r.reconcileNFS(ctx, cluster)).To(Succeed())

			secondDep := &appsv1.Deployment{}
			Expect(k8sClient.Get(ctx, nn("core-nfs-idempotent-nfs"), secondDep)).To(Succeed())
			Expect(secondDep.UID).To(Equal(firstDep.UID))

			secondCM := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, nn("core-nfs-idempotent-nfs-ganesha-conf"), secondCM)).To(Succeed())
			Expect(secondCM.UID).To(Equal(firstCM.UID))
		})

		It("honors custom exportPath, squash and nodePort", func() {
			cluster := createCluster("core-nfs-custom", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.NFS = saunafsv1alpha1.NFSSpec{
					Enabled:    boolPtr(true),
					ExportPath: "/exports/data",
					Squash:     "Root_Squash",
					NodePort:   30800,
				}
			})
			Expect(newReconciler().reconcileNFS(ctx, cluster)).To(Succeed())

			cm := &corev1.ConfigMap{}
			expectEventuallyExists(cm, nn("core-nfs-custom-nfs-ganesha-conf"))
			Expect(cm.Data["ganesha.conf"]).To(ContainSubstring(`Path                 = "/exports/data"`))
			Expect(cm.Data["ganesha.conf"]).To(ContainSubstring("Squash               = Root_Squash"))

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, nn("core-nfs-custom-nfs"), svc)).To(Succeed())
			var nfsPort *corev1.ServicePort
			for i := range svc.Spec.Ports {
				if svc.Spec.Ports[i].Name == "nfs" {
					nfsPort = &svc.Spec.Ports[i]
				}
			}
			Expect(nfsPort).NotTo(BeNil())
			Expect(nfsPort.NodePort).To(Equal(int32(30800)))
		})

		It("deletes the ConfigMap, Deployment and Service once disabled again", func() {
			cluster := createCluster("core-nfs-disable", func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.NFS.Enabled = boolPtr(true)
			})
			r := newReconciler()
			Expect(r.reconcileNFS(ctx, cluster)).To(Succeed())
			expectEventuallyExists(&appsv1.Deployment{}, nn("core-nfs-disable-nfs"))

			cluster.Spec.NFS.Enabled = boolPtr(false)
			Expect(r.reconcileNFS(ctx, cluster)).To(Succeed())

			expectEventuallyGone(func() client.Object { return &appsv1.Deployment{} }, nn("core-nfs-disable-nfs"))
			expectEventuallyGone(func() client.Object { return &corev1.Service{} }, nn("core-nfs-disable-nfs"))
			expectEventuallyGone(func() client.Object { return &corev1.ConfigMap{} }, nn("core-nfs-disable-nfs-ganesha-conf"))
		})
	})
})
