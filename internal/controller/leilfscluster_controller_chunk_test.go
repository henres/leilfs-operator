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

// This file covers the chunk-server-related reconcile methods:
//   - reconcileChunkServers            (dispatcher over spec.chunk.servers)
//   - reconcileChunkHeadlessService
//   - reconcileChunkHddConfigMap
//   - reconcileChunkStatefulSet
//   - reconcileAutoDiscoverChunkServers (PV auto-discovery from localdisk-operator)
//
// Every LeilFSCluster and PersistentVolume created here uses a name unique to
// its own It() block so that resources created by these specs never collide
// with specs in other files that share the same envtest namespace ("default").
// Note: envtest runs only the API server + etcd, not the garbage-collector
// controller, so objects with an ownerReference to a deleted LeilFSCluster are
// NOT cascade-deleted; uniqueness of generated names is what keeps specs
// independent, not GC.

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
	"sigs.k8s.io/controller-runtime/pkg/client"

	saunafsv1alpha1 "github.com/henres/leilfs-operator/api/v1alpha1"
)

var _ = Describe("Chunk server reconciliation", func() {
	ctx := context.Background()

	newReconciler := func() *LeilFSClusterReconciler {
		return &LeilFSClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	// createCluster creates a LeilFSCluster with the given name/spec and
	// registers its deletion. It does not wait for/rely on cascade deletion
	// of owned objects (see file-level comment).
	createCluster := func(name string, spec saunafsv1alpha1.LeilFSClusterSpec) *saunafsv1alpha1.LeilFSCluster {
		cluster := &saunafsv1alpha1.LeilFSCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec:       spec,
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
		return cluster
	}

	chunkKey := func(cluster *saunafsv1alpha1.LeilFSCluster, srvName string) types.NamespacedName {
		return types.NamespacedName{Name: fmt.Sprintf("%s-chunk-%s", cluster.Name, srvName), Namespace: cluster.Namespace}
	}

	// ── reconcileChunkHeadlessService ────────────────────────────────────────

	Describe("reconcileChunkHeadlessService", func() {
		It("creates a headless Service with the expected selector, port, and owner reference", func() {
			cluster := createCluster("chunk-svc-create", saunafsv1alpha1.LeilFSClusterSpec{})
			srv := &saunafsv1alpha1.ChunkServerSpec{Name: "c0", NodeName: "node-a"}

			Expect(newReconciler().reconcileChunkHeadlessService(ctx, cluster, srv)).To(Succeed())

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, "c0"), svc)).To(Succeed())

			Expect(svc.Spec.ClusterIP).To(Equal("None"))
			Expect(svc.Spec.Selector).To(Equal(map[string]string{
				"app.kubernetes.io/name":     "leilfs-chunkserver",
				"app.kubernetes.io/instance": cluster.Name,
				"leilfs.io/chunk-server":     "c0",
			}))
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Name).To(Equal("data"))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(9422)))
			Expect(svc.Spec.Ports[0].Protocol).To(Equal(corev1.ProtocolTCP))

			Expect(svc.OwnerReferences).To(HaveLen(1))
			Expect(svc.OwnerReferences[0].Name).To(Equal(cluster.Name))
			Expect(svc.OwnerReferences[0].Kind).To(Equal("LeilFSCluster"))
		})

		It("is idempotent: repairs a manually-modified selector on re-reconcile without touching the immutable ClusterIP", func() {
			cluster := createCluster("chunk-svc-repair", saunafsv1alpha1.LeilFSClusterSpec{})
			srv := &saunafsv1alpha1.ChunkServerSpec{Name: "c0", NodeName: "node-a"}
			r := newReconciler()

			Expect(r.reconcileChunkHeadlessService(ctx, cluster, srv)).To(Succeed())

			svc := &corev1.Service{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, "c0"), svc)).To(Succeed())
			originalClusterIP := svc.Spec.ClusterIP
			originalUID := svc.UID

			// Corrupt the selector out-of-band, then reconcile again.
			svc.Spec.Selector = map[string]string{"tampered": "true"}
			Expect(k8sClient.Update(ctx, svc)).To(Succeed())

			Expect(r.reconcileChunkHeadlessService(ctx, cluster, srv)).To(Succeed())

			repaired := &corev1.Service{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, "c0"), repaired)).To(Succeed())
			Expect(repaired.UID).To(Equal(originalUID), "expected an update, not a recreate")
			Expect(repaired.Spec.ClusterIP).To(Equal(originalClusterIP))
			Expect(repaired.Spec.Selector).To(Equal(map[string]string{
				"app.kubernetes.io/name":     "leilfs-chunkserver",
				"app.kubernetes.io/instance": cluster.Name,
				"leilfs.io/chunk-server":     "c0",
			}))

			// Still exactly one Service for this chunk server.
			svcList := &corev1.ServiceList{}
			Expect(k8sClient.List(ctx, svcList, client.InNamespace(cluster.Namespace), client.MatchingLabels{})).To(Succeed())
			count := 0
			for _, s := range svcList.Items {
				if s.Name == chunkKey(cluster, "c0").Name {
					count++
				}
			}
			Expect(count).To(Equal(1))
		})
	})

	// ── reconcileChunkHddConfigMap ───────────────────────────────────────────

	Describe("reconcileChunkHddConfigMap", func() {
		It("writes one line per mount path plus the generated header comment", func() {
			cluster := createCluster("chunk-cm-create", saunafsv1alpha1.LeilFSClusterSpec{})
			srv := &saunafsv1alpha1.ChunkServerSpec{
				Name:     "c0",
				NodeName: "node-a",
				MountPaths: []saunafsv1alpha1.MountPath{
					{Path: "/mnt/hdd0", HostPath: "/mnt/disks/hdd0"},
					{Path: "/mnt/hdd1", HostPath: "/mnt/disks/hdd1"},
				},
			}

			Expect(newReconciler().reconcileChunkHddConfigMap(ctx, cluster, srv)).To(Succeed())

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: fmt.Sprintf("%s-chunk-c0-hdd", cluster.Name), Namespace: cluster.Namespace,
			}, cm)).To(Succeed())

			content := cm.Data["sfshdd.cfg"]
			Expect(content).To(HavePrefix("# sfshdd.cfg"))
			Expect(content).To(ContainSubstring("/mnt/hdd0\n"))
			Expect(content).To(ContainSubstring("/mnt/hdd1\n"))

			Expect(cm.OwnerReferences).To(HaveLen(1))
			Expect(cm.OwnerReferences[0].Name).To(Equal(cluster.Name))
		})

		It("is idempotent: updates the existing ConfigMap in place when mount paths change", func() {
			cluster := createCluster("chunk-cm-update", saunafsv1alpha1.LeilFSClusterSpec{})
			srv := &saunafsv1alpha1.ChunkServerSpec{
				Name:       "c0",
				NodeName:   "node-a",
				MountPaths: []saunafsv1alpha1.MountPath{{Path: "/mnt/hdd0", HostPath: "/mnt/disks/hdd0"}},
			}
			r := newReconciler()
			Expect(r.reconcileChunkHddConfigMap(ctx, cluster, srv)).To(Succeed())

			cmKey := types.NamespacedName{Name: fmt.Sprintf("%s-chunk-c0-hdd", cluster.Name), Namespace: cluster.Namespace}
			first := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, cmKey, first)).To(Succeed())
			originalUID := first.UID

			srv.MountPaths = append(srv.MountPaths, saunafsv1alpha1.MountPath{Path: "/mnt/hdd1", HostPath: "/mnt/disks/hdd1"})
			Expect(r.reconcileChunkHddConfigMap(ctx, cluster, srv)).To(Succeed())

			updated := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, cmKey, updated)).To(Succeed())
			Expect(updated.UID).To(Equal(originalUID), "expected an update, not a recreate")
			Expect(updated.Data["sfshdd.cfg"]).To(ContainSubstring("/mnt/hdd0\n"))
			Expect(updated.Data["sfshdd.cfg"]).To(ContainSubstring("/mnt/hdd1\n"))
		})
	})

	// ── reconcileChunkStatefulSet ────────────────────────────────────────────

	Describe("reconcileChunkStatefulSet", func() {
		It("creates a single-replica StatefulSet with the built-in default image and hostPath volumes", func() {
			cluster := createCluster("chunk-sts-default", saunafsv1alpha1.LeilFSClusterSpec{})
			srv := &saunafsv1alpha1.ChunkServerSpec{
				Name:       "c0",
				NodeName:   "node-a",
				MountPaths: []saunafsv1alpha1.MountPath{{Path: "/mnt/hdd0", HostPath: "/mnt/disks/hdd0"}},
			}

			Expect(newReconciler().reconcileChunkStatefulSet(ctx, cluster, srv)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, "c0"), sts)).To(Succeed())

			Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
			Expect(sts.Spec.ServiceName).To(Equal(chunkKey(cluster, "c0").Name))
			Expect(sts.Spec.Template.Spec.NodeName).To(Equal("node-a"))

			Expect(sts.Spec.Template.Spec.Containers).To(HaveLen(1))
			container := sts.Spec.Template.Spec.Containers[0]
			Expect(container.Name).To(Equal("leilfs-chunkserver"))
			Expect(container.Image).To(Equal("ghcr.io/henres/leilfs-container/leilfs-chunkserver:5.10.1"))

			Expect(container.VolumeMounts).To(ContainElement(corev1.VolumeMount{Name: "data-0", MountPath: "/mnt/hdd0"}))

			var dataVolume *corev1.Volume
			for i := range sts.Spec.Template.Spec.Volumes {
				if sts.Spec.Template.Spec.Volumes[i].Name == "data-0" {
					dataVolume = &sts.Spec.Template.Spec.Volumes[i]
				}
			}
			Expect(dataVolume).NotTo(BeNil())
			Expect(dataVolume.HostPath).NotTo(BeNil())
			Expect(dataVolume.HostPath.Path).To(Equal("/mnt/disks/hdd0"))

			Expect(sts.OwnerReferences).To(HaveLen(1))
			Expect(sts.OwnerReferences[0].Name).To(Equal(cluster.Name))
		})

		It("prefers srv.Image over cluster.Spec.Chunk.Image and the built-in default", func() {
			cluster := createCluster("chunk-sts-image-srv", saunafsv1alpha1.LeilFSClusterSpec{
				Chunk: saunafsv1alpha1.ChunkSpec{Image: "example.com/cluster-default:1.0"},
			})
			srv := &saunafsv1alpha1.ChunkServerSpec{
				Name:     "c0",
				NodeName: "node-a",
				Image:    "example.com/per-server:2.0",
			}

			Expect(newReconciler().reconcileChunkStatefulSet(ctx, cluster, srv)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, "c0"), sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("example.com/per-server:2.0"))
			Expect(sts.Spec.Template.Spec.InitContainers[0].Image).To(Equal("example.com/per-server:2.0"))
		})

		It("falls back to cluster.Spec.Chunk.Image when srv.Image is empty", func() {
			cluster := createCluster("chunk-sts-image-cluster", saunafsv1alpha1.LeilFSClusterSpec{
				Chunk: saunafsv1alpha1.ChunkSpec{Image: "example.com/cluster-default:1.0"},
			})
			srv := &saunafsv1alpha1.ChunkServerSpec{Name: "c0", NodeName: "node-a"}

			Expect(newReconciler().reconcileChunkStatefulSet(ctx, cluster, srv)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, "c0"), sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.Containers[0].Image).To(Equal("example.com/cluster-default:1.0"))
		})

		It("appends a LABEL line to the init command when srv.Label is set", func() {
			cluster := createCluster("chunk-sts-label", saunafsv1alpha1.LeilFSClusterSpec{})
			srv := &saunafsv1alpha1.ChunkServerSpec{Name: "c0", NodeName: "node-a", Label: "rack1"}

			Expect(newReconciler().reconcileChunkStatefulSet(ctx, cluster, srv)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, "c0"), sts)).To(Succeed())
			initCmd := sts.Spec.Template.Spec.InitContainers[0].Command
			Expect(initCmd).To(HaveLen(3))
			Expect(initCmd[2]).To(ContainSubstring("LABEL = rack1"))
		})

		It("omits the LABEL line when srv.Label is empty", func() {
			cluster := createCluster("chunk-sts-nolabel", saunafsv1alpha1.LeilFSClusterSpec{})
			srv := &saunafsv1alpha1.ChunkServerSpec{Name: "c0", NodeName: "node-a"}

			Expect(newReconciler().reconcileChunkStatefulSet(ctx, cluster, srv)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, "c0"), sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.InitContainers[0].Command[2]).NotTo(ContainSubstring("LABEL"))
		})

		It("is idempotent: re-reconciling updates the existing StatefulSet in place", func() {
			cluster := createCluster("chunk-sts-idempotent", saunafsv1alpha1.LeilFSClusterSpec{})
			srv := &saunafsv1alpha1.ChunkServerSpec{Name: "c0", NodeName: "node-a"}
			r := newReconciler()

			Expect(r.reconcileChunkStatefulSet(ctx, cluster, srv)).To(Succeed())
			first := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, "c0"), first)).To(Succeed())
			originalUID := first.UID

			srv.Image = "example.com/updated:3.0"
			Expect(r.reconcileChunkStatefulSet(ctx, cluster, srv)).To(Succeed())

			updated := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, "c0"), updated)).To(Succeed())
			Expect(updated.UID).To(Equal(originalUID), "expected an update, not a recreate")
			Expect(updated.Spec.Template.Spec.Containers[0].Image).To(Equal("example.com/updated:3.0"))

			stsList := &appsv1.StatefulSetList{}
			Expect(k8sClient.List(ctx, stsList, client.InNamespace(cluster.Namespace))).To(Succeed())
			count := 0
			for _, s := range stsList.Items {
				if s.Name == chunkKey(cluster, "c0").Name {
					count++
				}
			}
			Expect(count).To(Equal(1))
		})
	})

	// ── reconcileChunkServers (dispatcher) ───────────────────────────────────

	Describe("reconcileChunkServers", func() {
		specFor := func() saunafsv1alpha1.LeilFSClusterSpec {
			return saunafsv1alpha1.LeilFSClusterSpec{
				Chunk: saunafsv1alpha1.ChunkSpec{
					Servers: []saunafsv1alpha1.ChunkServerSpec{
						{Name: "c0", NodeName: "node-a", MountPaths: []saunafsv1alpha1.MountPath{{Path: "/mnt/hdd0", HostPath: "/mnt/disks/hdd0"}}},
						{Name: "c1", NodeName: "node-b", MountPaths: []saunafsv1alpha1.MountPath{{Path: "/mnt/hdd0", HostPath: "/mnt/disks/hdd0"}}},
					},
				},
			}
		}

		It("reconciles the headless Service, hdd ConfigMap, and StatefulSet for every declared server", func() {
			cluster := createCluster("chunk-dispatch-create", specFor())

			Expect(newReconciler().reconcileChunkServers(ctx, cluster)).To(Succeed())

			for _, name := range []string{"c0", "c1"} {
				Expect(k8sClient.Get(ctx, chunkKey(cluster, name), &corev1.Service{})).To(Succeed())
				Expect(k8sClient.Get(ctx, types.NamespacedName{
					Name: fmt.Sprintf("%s-chunk-%s-hdd", cluster.Name, name), Namespace: cluster.Namespace,
				}, &corev1.ConfigMap{})).To(Succeed())
				Expect(k8sClient.Get(ctx, chunkKey(cluster, name), &appsv1.StatefulSet{})).To(Succeed())
			}
		})

		It("is idempotent across repeated reconciles: exactly one object set per server, no errors", func() {
			cluster := createCluster("chunk-dispatch-idempotent", specFor())
			r := newReconciler()

			Expect(r.reconcileChunkServers(ctx, cluster)).To(Succeed())
			Expect(r.reconcileChunkServers(ctx, cluster)).To(Succeed())

			stsList := &appsv1.StatefulSetList{}
			Expect(k8sClient.List(ctx, stsList, client.InNamespace(cluster.Namespace), client.MatchingLabels{
				"app.kubernetes.io/instance": cluster.Name,
			})).To(Succeed())
			Expect(stsList.Items).To(HaveLen(2))

			// reconcileChunkHeadlessService does not set metadata Labels on the
			// Service (only Spec.Selector), so a label-based List can't be used
			// here; verify uniqueness by exact name instead. Object names are
			// deterministic (cluster+server derived), so "no duplicates" is
			// equivalent to "the expected names exist and nothing else matches".
			svcList := &corev1.ServiceList{}
			Expect(k8sClient.List(ctx, svcList, client.InNamespace(cluster.Namespace))).To(Succeed())
			matching := 0
			for _, s := range svcList.Items {
				if s.Name == chunkKey(cluster, "c0").Name || s.Name == chunkKey(cluster, "c1").Name {
					matching++
				}
			}
			Expect(matching).To(Equal(2))
		})
	})

	// ── reconcileAutoDiscoverChunkServers ────────────────────────────────────

	Describe("reconcileAutoDiscoverChunkServers", func() {
		// forceDeletePV actually removes a PersistentVolume in envtest.
		// The API server's built-in StorageObjectInUseProtection admission
		// plugin stamps every PV with the kubernetes.io/pv-protection
		// finalizer at creation time; that finalizer is normally cleared by
		// the pv-protection *controller*, which lives in kube-controller-manager
		// — a process envtest never starts (it only runs the API server and
		// etcd). Without stripping the finalizer manually, Delete() merely sets
		// a DeletionTimestamp and the PV keeps satisfying later specs'
		// reconcileAutoDiscoverChunkServers PV listings, leaking across tests
		// (this was caught by the "idempotent" spec below picking up PVs left
		// behind by earlier specs).
		forceDeletePV := func(pv *corev1.PersistentVolume) {
			_ = k8sClient.Delete(ctx, pv)
			latest := &corev1.PersistentVolume{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: pv.Name}, latest); err == nil {
				latest.Finalizers = nil
				_ = k8sClient.Update(ctx, latest)
			}
		}

		// createPV creates a cluster-scoped PersistentVolume simulating one
		// produced by the localdisk-operator, optionally setting its phase via
		// the status subresource (Create() always resets .status to empty on
		// core v1 types, so Phase must be set with a follow-up Status().Update).
		createPV := func(name, nodeName string, labels map[string]string, phase corev1.PersistentVolumePhase) *corev1.PersistentVolume {
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
				Spec: corev1.PersistentVolumeSpec{
					Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
					AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
					// Local (not HostPath) mirrors what the localdisk-operator actually
					// produces, and the API server requires nodeAffinity to be set
					// whenever Local is used — which every case using this helper does.
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						Local: &corev1.LocalVolumeSource{Path: "/mnt/disks/" + name},
					},
					NodeAffinity: &corev1.VolumeNodeAffinity{
						Required: &corev1.NodeSelector{
							NodeSelectorTerms: []corev1.NodeSelectorTerm{{
								MatchExpressions: []corev1.NodeSelectorRequirement{{
									Key:      "kubernetes.io/hostname",
									Operator: corev1.NodeSelectorOpIn,
									Values:   []string{nodeName},
								}},
							}},
						},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pv)).To(Succeed())
			DeferCleanup(func() { forceDeletePV(pv) })

			if phase != "" {
				pv.Status.Phase = phase
				Expect(k8sClient.Status().Update(ctx, pv)).To(Succeed())
			}
			return pv
		}

		autoDiscoverSpec := func() saunafsv1alpha1.LeilFSClusterSpec {
			return saunafsv1alpha1.LeilFSClusterSpec{
				Chunk: saunafsv1alpha1.ChunkSpec{
					AutoDiscover: &saunafsv1alpha1.ChunkAutoDiscoverSpec{Enabled: true},
				},
			}
		}

		It("does nothing when spec.chunk.autoDiscover is nil", func() {
			cluster := createCluster("ad-nil", saunafsv1alpha1.LeilFSClusterSpec{})
			Expect(newReconciler().reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			stsList := &appsv1.StatefulSetList{}
			Expect(k8sClient.List(ctx, stsList, client.InNamespace(cluster.Namespace), client.MatchingLabels{
				"app.kubernetes.io/instance": cluster.Name,
			})).To(Succeed())
			Expect(stsList.Items).To(BeEmpty())
		})

		It("does nothing when spec.chunk.autoDiscover.enabled is false", func() {
			pv := createPV("pv-disabled-0001", "worker-disabled", map[string]string{"localdisk-operator.io/disk": "vdb"}, corev1.VolumeAvailable)
			cluster := createCluster("ad-disabled", saunafsv1alpha1.LeilFSClusterSpec{
				Chunk: saunafsv1alpha1.ChunkSpec{
					AutoDiscover: &saunafsv1alpha1.ChunkAutoDiscoverSpec{Enabled: false},
				},
			})

			Expect(newReconciler().reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			srvName := "ad-" + pv.Name[:8]
			err := k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("creates a StatefulSet+PVC+Service+ConfigMap for a matching Available PV", func() {
			pv := createPV("pv-avail-0001", "worker-1", map[string]string{"localdisk-operator.io/disk": "vdb"}, corev1.VolumeAvailable)
			cluster := createCluster("ad-basic", autoDiscoverSpec())

			Expect(newReconciler().reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			srvName := "ad-" + pv.Name[:8]
			pvcName := fmt.Sprintf("%s-chunk-%s", cluster.Name, srvName)

			pvc := &corev1.PersistentVolumeClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: cluster.Namespace}, pvc)).To(Succeed())
			Expect(pvc.Spec.VolumeName).To(Equal(pv.Name))
			Expect(pvc.Spec.Resources.Requests[corev1.ResourceStorage].Equal(resource.MustParse("5Gi"))).To(BeTrue())
			Expect(pvc.Labels).To(HaveKeyWithValue("leilfs.io/auto-discover", "true"))
			Expect(pvc.OwnerReferences).To(HaveLen(1))
			Expect(pvc.OwnerReferences[0].Name).To(Equal(cluster.Name))

			Expect(k8sClient.Get(ctx, chunkKey(cluster, srvName), &corev1.Service{})).To(Succeed())

			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{
				Name: fmt.Sprintf("%s-chunk-%s-hdd", cluster.Name, srvName), Namespace: cluster.Namespace,
			}, cm)).To(Succeed())
			Expect(cm.Data["sfshdd.cfg"]).To(ContainSubstring("/mnt/hdd0\n"))

			sts := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, chunkKey(cluster, srvName), sts)).To(Succeed())
			Expect(sts.Spec.Template.Spec.NodeName).To(Equal("worker-1"))

			var dataVolume *corev1.Volume
			for i := range sts.Spec.Template.Spec.Volumes {
				if sts.Spec.Template.Spec.Volumes[i].PersistentVolumeClaim != nil {
					dataVolume = &sts.Spec.Template.Spec.Volumes[i]
				}
			}
			Expect(dataVolume).NotTo(BeNil())
			Expect(dataVolume.PersistentVolumeClaim.ClaimName).To(Equal(pvcName))

			// LABEL is derived from the node name for goal-based placement.
			Expect(sts.Spec.Template.Spec.InitContainers[0].Command[2]).To(ContainSubstring("LABEL = worker_1"))
		})

		It("creates resources for a matching Bound PV too", func() {
			pv := createPV("pv-bound-00001", "worker-2", map[string]string{"localdisk-operator.io/disk": "vdc"}, corev1.VolumeBound)
			cluster := createCluster("ad-bound", autoDiscoverSpec())

			Expect(newReconciler().reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			srvName := "ad-" + pv.Name[:8]
			Expect(k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})).To(Succeed())
		})

		It("is idempotent: a second reconcile does not create duplicate PVCs or StatefulSets", func() {
			pv := createPV("pv-idemp-0001", "worker-3", map[string]string{"localdisk-operator.io/disk": "vdb"}, corev1.VolumeAvailable)
			cluster := createCluster("ad-idempotent", autoDiscoverSpec())
			r := newReconciler()

			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())
			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			srvName := "ad-" + pv.Name[:8]

			pvcList := &corev1.PersistentVolumeClaimList{}
			Expect(k8sClient.List(ctx, pvcList, client.InNamespace(cluster.Namespace), client.MatchingLabels{
				"app.kubernetes.io/instance": cluster.Name,
			})).To(Succeed())
			Expect(pvcList.Items).To(HaveLen(1))

			stsList := &appsv1.StatefulSetList{}
			Expect(k8sClient.List(ctx, stsList, client.InNamespace(cluster.Namespace), client.MatchingLabels{
				"app.kubernetes.io/instance": cluster.Name,
			})).To(Succeed())
			Expect(stsList.Items).To(HaveLen(1))
			Expect(stsList.Items[0].Name).To(Equal(chunkKey(cluster, srvName).Name))
		})

		It("skips a PV that does not carry the localdisk-operator.io/disk label", func() {
			pv := createPV("pv-nomatch-001", "worker-4", map[string]string{"other-label": "x"}, corev1.VolumeAvailable)
			cluster := createCluster("ad-nomatch", autoDiscoverSpec())

			Expect(newReconciler().reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			srvName := "ad-" + pv.Name[:8]
			err := k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("honors a custom pvLabelSelector, skipping PVs that only carry the base disk label", func() {
			pv := createPV("pv-selector-01", "worker-5", map[string]string{"localdisk-operator.io/disk": "vdb"}, corev1.VolumeAvailable)
			cluster := createCluster("ad-selector", saunafsv1alpha1.LeilFSClusterSpec{
				Chunk: saunafsv1alpha1.ChunkSpec{
					AutoDiscover: &saunafsv1alpha1.ChunkAutoDiscoverSpec{
						Enabled:         true,
						PVLabelSelector: map[string]string{"localdisk-operator.io/disk": "vdb", "tier": "fast"},
					},
				},
			})

			Expect(newReconciler().reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			srvName := "ad-" + pv.Name[:8]
			err := k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("skips a PV that is Pending (neither Available nor Bound)", func() {
			pv := createPV("pv-pending-001", "worker-6", map[string]string{"localdisk-operator.io/disk": "vdb"}, corev1.VolumePending)
			cluster := createCluster("ad-pending", autoDiscoverSpec())

			Expect(newReconciler().reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			srvName := "ad-" + pv.Name[:8]
			err := k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		It("skips a PV that has no nodeAffinity (cannot determine target node)", func() {
			pv := &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "pv-noaffinity-1", Labels: map[string]string{"localdisk-operator.io/disk": "vdb"}},
				Spec: corev1.PersistentVolumeSpec{
					Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
					AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						HostPath: &corev1.HostPathVolumeSource{Path: "/mnt/disks/pv-noaffinity-1"},
					},
				},
			}
			Expect(k8sClient.Create(ctx, pv)).To(Succeed())
			DeferCleanup(func() { forceDeletePV(pv) })
			pv.Status.Phase = corev1.VolumeAvailable
			Expect(k8sClient.Status().Update(ctx, pv)).To(Succeed())

			cluster := createCluster("ad-noaffinity", autoDiscoverSpec())

			Expect(newReconciler().reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			srvName := "ad-" + pv.Name[:8]
			err := k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue())
		})

		// ── Orphan teardown (localdisk-operator.io/disk-state contract) ─────────
		//
		// localdisk-operator sets localdisk-operator.io/disk-state=Missing on a
		// PV once the underlying physical disk is confirmed gone (past its
		// debounce grace period) but never deletes a Bound PV itself — that's
		// the producer-side safety invariant. It is this controller's job to
		// tear down the chunkserver workload bound to such a PV so it can
		// eventually transition to Released and be reaped on the producer
		// side. A PV disappearing entirely is the other orphan trigger.

		markDiskState := func(pv *corev1.PersistentVolume, state string) {
			latest := &corev1.PersistentVolume{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: pv.Name}, latest)).To(Succeed())
			if latest.Labels == nil {
				latest.Labels = map[string]string{}
			}
			latest.Labels["localdisk-operator.io/disk-state"] = state
			Expect(k8sClient.Update(ctx, latest)).To(Succeed())
		}

		// forceDeletePVC mirrors forceDeletePV above: the PVCs this suite
		// creates via ensureAutoDiscoverPVC also get the built-in
		// kubernetes.io/pvc-protection finalizer stamped on by the
		// StorageObjectInUseProtection admission plugin, normally cleared by
		// a controller envtest never runs. Registered as a DeferCleanup
		// backstop so a torn-down (or never-torn-down, on assertion failure)
		// PVC never leaks into later specs.
		forceDeletePVC := func(name string) {
			pvc := &corev1.PersistentVolumeClaim{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, pvc); err != nil {
				return
			}
			_ = k8sClient.Delete(ctx, pvc)
			latest := &corev1.PersistentVolumeClaim{}
			if err := k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, latest); err == nil {
				latest.Finalizers = nil
				_ = k8sClient.Update(ctx, latest)
			}
		}

		expectResourceSetGone := func(cluster *saunafsv1alpha1.LeilFSCluster, srvName string) {
			pvcName := fmt.Sprintf("%s-chunk-%s", cluster.Name, srvName)
			cmName := fmt.Sprintf("%s-chunk-%s-hdd", cluster.Name, srvName)

			err := k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "StatefulSet should have been deleted")

			err = k8sClient.Get(ctx, chunkKey(cluster, srvName), &corev1.Service{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "Service should have been deleted")

			err = k8sClient.Get(ctx, types.NamespacedName{Name: cmName, Namespace: cluster.Namespace}, &corev1.ConfigMap{})
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "ConfigMap should have been deleted")

			// The PVC carries the same built-in kubernetes.io/pvc-protection
			// finalizer as PVs carry kubernetes.io/pv-protection (see
			// forceDeletePV's comment above) — added at creation by the
			// StorageObjectInUseProtection admission plugin and normally
			// cleared by a controller envtest never runs. Delete() therefore
			// only sets a DeletionTimestamp here; accept that as proof the
			// reconciler issued the delete, since full removal is something
			// only a live cluster's pvc-protection controller can finish.
			pvc := &corev1.PersistentVolumeClaim{}
			err = k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: cluster.Namespace}, pvc)
			if err == nil {
				Expect(pvc.DeletionTimestamp).NotTo(BeNil(), "PVC should have been marked for deletion")
			} else {
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "PVC should have been deleted")
			}
		}

		It("tears down StatefulSet+PVC+Service+ConfigMap once the PV's disk-state flips to Missing", func() {
			pv := createPV("pv-missing-0001", "worker-8", map[string]string{"localdisk-operator.io/disk": "vdb"}, corev1.VolumeBound)
			cluster := createCluster("ad-missing", autoDiscoverSpec())
			r := newReconciler()

			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())
			srvName := "ad-" + pv.Name[:8]
			DeferCleanup(func() { forceDeletePVC(fmt.Sprintf("%s-chunk-%s", cluster.Name, srvName)) })
			Expect(k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})).To(Succeed())

			markDiskState(pv, "Missing")

			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())
			expectResourceSetGone(cluster, srvName)
		})

		It("tears down chunk server resources when the underlying PV disappears entirely", func() {
			pv := createPV("pv-vanish-0001", "worker-9", map[string]string{"localdisk-operator.io/disk": "vdb"}, corev1.VolumeBound)
			cluster := createCluster("ad-gone", autoDiscoverSpec())
			r := newReconciler()

			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())
			srvName := "ad-" + pv.Name[:8]
			DeferCleanup(func() { forceDeletePVC(fmt.Sprintf("%s-chunk-%s", cluster.Name, srvName)) })
			Expect(k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})).To(Succeed())

			forceDeletePV(pv)

			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())
			expectResourceSetGone(cluster, srvName)
		})

		It("keeps resources for a PV whose disk-state is Ready (no regression on the nominal path)", func() {
			pv := createPV("pv-ready-00001", "worker-10", map[string]string{"localdisk-operator.io/disk": "vdb", "localdisk-operator.io/disk-state": "Ready"}, corev1.VolumeBound)
			cluster := createCluster("ad-ready", autoDiscoverSpec())

			Expect(newReconciler().reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			srvName := "ad-" + pv.Name[:8]
			DeferCleanup(func() { forceDeletePVC(fmt.Sprintf("%s-chunk-%s", cluster.Name, srvName)) })
			Expect(k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})).To(Succeed())
		})

		It("does not tear down existing resources when autoDiscover is subsequently disabled", func() {
			pv := createPV("pv-adisab-0001", "worker-11", map[string]string{"localdisk-operator.io/disk": "vdb"}, corev1.VolumeBound)
			cluster := createCluster("ad-disable-after", autoDiscoverSpec())
			r := newReconciler()

			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())
			srvName := "ad-" + pv.Name[:8]
			DeferCleanup(func() { forceDeletePVC(fmt.Sprintf("%s-chunk-%s", cluster.Name, srvName)) })
			Expect(k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})).To(Succeed())

			markDiskState(pv, "Missing")
			cluster.Spec.Chunk.AutoDiscover.Enabled = false

			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())

			// Teardown must never fire for a disabled cluster, matching the
			// watch mapping's Enabled=true-only enqueue filter.
			Expect(k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})).To(Succeed())
		})

		It("is idempotent: sweeping an already-torn-down resource set does not error", func() {
			pv := createPV("pv-idemp-0002", "worker-12", map[string]string{"localdisk-operator.io/disk": "vdb"}, corev1.VolumeBound)
			cluster := createCluster("ad-sweep-idempotent", autoDiscoverSpec())
			r := newReconciler()

			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())
			srvName := "ad-" + pv.Name[:8]
			DeferCleanup(func() { forceDeletePVC(fmt.Sprintf("%s-chunk-%s", cluster.Name, srvName)) })
			Expect(k8sClient.Get(ctx, chunkKey(cluster, srvName), &appsv1.StatefulSet{})).To(Succeed())

			markDiskState(pv, "Missing")

			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())
			expectResourceSetGone(cluster, srvName)

			// Second sweep over an already-torn-down set: no error.
			Expect(r.reconcileAutoDiscoverChunkServers(ctx, cluster)).To(Succeed())
			expectResourceSetGone(cluster, srvName)
		})
	})
})
