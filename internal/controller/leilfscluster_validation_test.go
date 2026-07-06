package controller

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saunafsv1alpha1 "github.com/henres/leilfs-operator/api/v1alpha1"
)

var _ = Describe("LeilFSCluster unsupported API contract validation", func() {
	ctx := context.Background()

	newReconciler := func() *LeilFSClusterReconciler {
		return &LeilFSClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	createCluster := func(name string, mutate func(*saunafsv1alpha1.LeilFSCluster)) {
		cluster := &saunafsv1alpha1.LeilFSCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		}
		if mutate != nil {
			mutate(cluster)
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { _ = k8sClient.Delete(ctx, cluster) })
	}

	boolPtr := func(v bool) *bool { return &v }

	It("fails fast when the unsupported CSI driver is enabled", func() {
		name := "unsupported-csi"
		createCluster(name, func(cluster *saunafsv1alpha1.LeilFSCluster) {
			cluster.Spec.CSI.Enabled = boolPtr(true)
			cluster.Spec.CSI.DriverName = "leilfs.csi.leilfs-operator.io"
		})

		_, err := newReconciler().Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: name, Namespace: "default"},
		})
		Expect(err).To(MatchError(ContainSubstring("spec.csi.enabled is not supported")))

		refreshed := &saunafsv1alpha1.LeilFSCluster{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, refreshed)).To(Succeed())
		condition := apimeta.FindStatusCondition(refreshed.Status.Conditions, saunafsv1alpha1.ConditionReady)
		Expect(condition).NotTo(BeNil())
		Expect(condition.Status).To(Equal(metav1.ConditionFalse))
		Expect(condition.Reason).To(Equal(saunafsv1alpha1.ReasonUnsupportedSpec))
		Expect(condition.Message).To(ContainSubstring("spec.csi.enabled is not supported"))

		sts := &appsv1.StatefulSet{}
		err = k8sClient.Get(ctx, types.NamespacedName{Name: fmt.Sprintf("%s-master", name), Namespace: "default"}, sts)
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("rejects manual chunk mount paths that would otherwise fall back to emptyDir", func() {
		cluster := &saunafsv1alpha1.LeilFSCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "dynamic-mount", Namespace: "default"},
			Spec: saunafsv1alpha1.LeilFSClusterSpec{
				Chunk: saunafsv1alpha1.ChunkSpec{
					Servers: []saunafsv1alpha1.ChunkServerSpec{{
						Name:     "chunk-0",
						NodeName: "worker-0",
						MountPaths: []saunafsv1alpha1.MountPath{{
							Path:             "/mnt/hdd001",
							StorageClassName: "fast-local",
							Size:             resource.MustParse("10Gi"),
						}},
					}},
				},
			},
		}

		err := validateClusterSpec(cluster)
		Expect(err).To(MatchError(ContainSubstring("spec.chunk.servers[0].mountPaths[0]")))
		Expect(err).To(MatchError(ContainSubstring("dynamic PVC provisioning for chunk mountPaths is not implemented")))
	})

	It("rejects shadow fields that cannot be applied per replica in the unified StatefulSet", func() {
		cluster := &saunafsv1alpha1.LeilFSCluster{
			ObjectMeta: metav1.ObjectMeta{Name: "unsupported-shadow", Namespace: "default"},
			Spec: saunafsv1alpha1.LeilFSClusterSpec{
				Master: saunafsv1alpha1.MasterSpec{Image: "ghcr.io/henres/leilfs-container/leilfs-master:5.10.1"},
				Shadow: &saunafsv1alpha1.ShadowSpec{
					Image:        "ghcr.io/henres/leilfs-container/leilfs-master:experimental",
					NodeSelector: map[string]string{"role": "shadow"},
					Tolerations:  []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "shadow"}},
				},
			},
		}

		err := validateClusterSpec(cluster)
		Expect(err).To(MatchError(ContainSubstring("spec.shadow.image differs from spec.master.image")))
		Expect(err).To(MatchError(ContainSubstring("spec.shadow.nodeSelector is not supported")))
		Expect(err).To(MatchError(ContainSubstring("spec.shadow.tolerations is not supported")))
	})
})
