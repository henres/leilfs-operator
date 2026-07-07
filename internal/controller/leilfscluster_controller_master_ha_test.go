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

// This file covers the master HA slice of the controller in isolation:
//   - reconcileMasterStatefulSet (the unified master+shadow StatefulSet)
//   - reconcileMasterHA          (passive Lease observer)
//   - reconcileMasterHARBAC      (ServiceAccount/Role/RoleBinding for the sidecar)
//
// The actual election protocol (init-container + ha-sidecar shell scripts) runs
// inside real containers and cannot be exercised under envtest, which provides
// only the kube-apiserver + etcd (no kubelet, no container runtime, no
// StatefulSet/Pod controllers). Tests here therefore focus on what the
// controller itself does deterministically: the StatefulSet spec it produces,
// the Lease/RBAC objects it manages, and its reaction to Lease/label state that
// a real sidecar would have produced (simulated here via direct API writes).
//
// KNOWN, DOCUMENTED GAP (see ROADMAP.md "Durcissement opérateur"): the current
// design promotes whichever pod's sidecar wins the Lease CAS race first, not
// necessarily the shadow with the most up-to-date metadata changelog. That is
// intentional/out of scope for this test file; see the failover test below for
// where this shows up.

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	saunafsv1alpha1 "github.com/henres/leilfs-operator/api/v1alpha1"
)

// masterHAFindContainer returns a pointer to the container with the given
// name, or nil if not present.
func masterHAFindContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

var _ = Describe("Master HA slice: reconcileMasterStatefulSet, reconcileMasterHA, reconcileMasterHARBAC", func() {
	ctx := context.Background()

	newReconciler := func() *LeilFSClusterReconciler {
		return &LeilFSClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

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

	int32Ptr := func(v int32) *int32 { return &v }
	boolPtr := func(v bool) *bool { return &v }

	// ── reconcileMasterStatefulSet ──────────────────────────────────────────

	Describe("reconcileMasterStatefulSet", func() {
		It("creates a single-replica StatefulSet with the expected containers, volumes, and mounts when shadow is not configured", func() {
			name := "ha-slice-sts-single"
			cluster := createCluster(name, nil)

			Expect(newReconciler().reconcileMasterStatefulSet(ctx, cluster)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: name + "-master", Namespace: "default"}, sts)
			}).Should(Succeed())

			Expect(*sts.Spec.Replicas).To(Equal(int32(1)))
			Expect(sts.Spec.ServiceName).To(Equal(name + "-master-hl"))

			// No HA sidecar when shadow is not configured.
			Expect(masterHAFindContainer(sts.Spec.Template.Spec.Containers, "ha-sidecar")).To(BeNil())

			// Exporter sidecar is on by default.
			exporter := masterHAFindContainer(sts.Spec.Template.Spec.Containers, "leilfs-exporter")
			Expect(exporter).NotTo(BeNil())

			master := masterHAFindContainer(sts.Spec.Template.Spec.Containers, "leilfs-master")
			Expect(master).NotTo(BeNil())
			Expect(master.Image).To(Equal(defaultMasterImage))
			// Protocol is left unset by the controller but defaulted to "TCP" by
			// the (real, envtest) apiserver on write.
			Expect(master.Ports).To(ConsistOf(
				corev1.ContainerPort{Name: "admin", ContainerPort: 9419, Protocol: corev1.ProtocolTCP},
				corev1.ContainerPort{Name: "cs", ContainerPort: 9420, Protocol: corev1.ProtocolTCP},
				corev1.ContainerPort{Name: "client", ContainerPort: 9421, Protocol: corev1.ProtocolTCP},
			))
			Expect(master.VolumeMounts).To(ConsistOf(
				corev1.VolumeMount{Name: "leilfs-cfg", MountPath: "/etc/saunafs"},
				corev1.VolumeMount{Name: "master-data", MountPath: "/var/lib/saunafs"},
			))

			initConfig := masterHAFindContainer(sts.Spec.Template.Spec.InitContainers, "init-config")
			Expect(initConfig).NotTo(BeNil())
			Expect(initConfig.Image).To(Equal(defaultMasterImage))
			Expect(initConfig.VolumeMounts).To(ConsistOf(
				corev1.VolumeMount{Name: "leilfs-cfg", MountPath: "/etc/saunafs"},
				corev1.VolumeMount{Name: "master-data", MountPath: "/var/lib/saunafs"},
			))

			initMeta := masterHAFindContainer(sts.Spec.Template.Spec.InitContainers, "init-metadata")
			Expect(initMeta).NotTo(BeNil())
			Expect(initMeta.VolumeMounts).To(ConsistOf(
				corev1.VolumeMount{Name: "master-data", MountPath: "/var/lib/saunafs"},
			))

			// No goals ConfigMap volume when spec.goals is empty.
			var volNames []string
			for _, v := range sts.Spec.Template.Spec.Volumes {
				volNames = append(volNames, v.Name)
			}
			Expect(volNames).To(ConsistOf("leilfs-cfg"))

			Expect(sts.Spec.VolumeClaimTemplates).To(HaveLen(1))
			Expect(sts.Spec.VolumeClaimTemplates[0].Name).To(Equal("master-data"))
			Expect(sts.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests.Storage().String()).To(Equal("1Gi"))
		})

		It("creates totalReplicas = 1 + shadow.replicas and injects the ha-sidecar container when shadow is configured", func() {
			name := "ha-slice-sts-shadow"
			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(2)}
			})

			Expect(newReconciler().reconcileMasterStatefulSet(ctx, cluster)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: name + "-master", Namespace: "default"}, sts)
			}).Should(Succeed())

			Expect(*sts.Spec.Replicas).To(Equal(int32(3)))

			sidecar := masterHAFindContainer(sts.Spec.Template.Spec.Containers, "ha-sidecar")
			Expect(sidecar).NotTo(BeNil())
			Expect(sidecar.Command).To(HaveLen(3))
			script := sidecar.Command[2]
			Expect(script).To(ContainSubstring(fmt.Sprintf(`LEASE_NAME="%s-master-ha"`, name)))
			Expect(script).To(ContainSubstring("RENEW_INTERVAL=10"))
			Expect(script).To(ContainSubstring("OBSERVE_INTERVAL=5"))
			Expect(script).To(ContainSubstring("LEASE_DURATION=30"))
			// Default StartupGracePeriod is 30s when unset on spec.master.
			Expect(script).To(ContainSubstring("STARTUP_GRACE=30"))

			var hasPodNameEnv bool
			for _, e := range sidecar.Env {
				if e.Name == "POD_NAME" {
					hasPodNameEnv = true
					Expect(e.ValueFrom).NotTo(BeNil())
					Expect(e.ValueFrom.FieldRef).NotTo(BeNil())
					Expect(e.ValueFrom.FieldRef.FieldPath).To(Equal("metadata.name"))
				}
			}
			Expect(hasPodNameEnv).To(BeTrue())
			Expect(sidecar.Resources).To(Equal(sidecarDefaultResources()))

			// shareProcessNamespace is required for the sidecar to observe sfsmaster.
			Expect(sts.Spec.Template.Spec.ShareProcessNamespace).NotTo(BeNil())
			Expect(*sts.Spec.Template.Spec.ShareProcessNamespace).To(BeTrue())

			// init-config also embeds the same Lease name to determine personality.
			initConfig := masterHAFindContainer(sts.Spec.Template.Spec.InitContainers, "init-config")
			Expect(initConfig).NotTo(BeNil())
			Expect(initConfig.Command[2]).To(ContainSubstring(fmt.Sprintf(`LEASE_NAME="%s-master-ha"`, name)))
		})

		It("is idempotent across re-reconciles and updates the StatefulSet in place rather than duplicating it", func() {
			name := "ha-slice-sts-idempotent"
			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(1)}
			})
			r := newReconciler()

			Expect(r.reconcileMasterStatefulSet(ctx, cluster)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: name + "-master", Namespace: "default"}, sts)
			}).Should(Succeed())
			firstUID := sts.UID
			Expect(*sts.Spec.Replicas).To(Equal(int32(2)))

			// Bump shadow replicas and reconcile again: the same object should be
			// updated, not recreated (create-or-update semantics).
			cluster.Spec.Shadow.Replicas = int32Ptr(3)
			Expect(r.reconcileMasterStatefulSet(ctx, cluster)).To(Succeed())

			Eventually(func() (int32, error) {
				updated := &appsv1.StatefulSet{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-master", Namespace: "default"}, updated); err != nil {
					return 0, err
				}
				return *updated.Spec.Replicas, nil
			}).Should(Equal(int32(4)))

			final := &appsv1.StatefulSet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: name + "-master", Namespace: "default"}, final)).To(Succeed())
			Expect(final.UID).To(Equal(firstUID), "expected an in-place update, not a StatefulSet recreation")
		})

		It("honors spec.master.image and spec.master.ports overrides across every relevant container", func() {
			name := "ha-slice-sts-overrides"
			customImage := "ghcr.io/henres/leilfs-container/leilfs-master:custom-tag"
			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Master.Image = customImage
				c.Spec.Master.Ports = []saunafsv1alpha1.NamedPort{{Name: "admin", ContainerPort: 19419}}
				c.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(1)}
			})

			Expect(newReconciler().reconcileMasterStatefulSet(ctx, cluster)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: name + "-master", Namespace: "default"}, sts)
			}).Should(Succeed())

			for _, c := range sts.Spec.Template.Spec.Containers {
				if c.Name == "leilfs-exporter" {
					continue // exporter has its own, independent default image
				}
				Expect(c.Image).To(Equal(customImage), "container %s should use spec.master.image", c.Name)
			}
			for _, c := range sts.Spec.Template.Spec.InitContainers {
				Expect(c.Image).To(Equal(customImage), "init container %s should use spec.master.image", c.Name)
			}

			master := masterHAFindContainer(sts.Spec.Template.Spec.Containers, "leilfs-master")
			Expect(master.Ports).To(ConsistOf(corev1.ContainerPort{Name: "admin", ContainerPort: 19419, Protocol: corev1.ProtocolTCP}))
		})

		It("omits the leilfs-exporter sidecar when explicitly disabled", func() {
			name := "ha-slice-sts-noexporter"
			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Master.Exporter = &saunafsv1alpha1.ExporterSpec{Enabled: boolPtr(false)}
			})

			Expect(newReconciler().reconcileMasterStatefulSet(ctx, cluster)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: name + "-master", Namespace: "default"}, sts)
			}).Should(Succeed())

			Expect(masterHAFindContainer(sts.Spec.Template.Spec.Containers, "leilfs-exporter")).To(BeNil())
		})

		It("mounts a ConfigMap-backed goals volume on init-config when spec.goals is set", func() {
			name := "ha-slice-sts-goals"
			two := int32(2)
			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Goals = []saunafsv1alpha1.GoalSpec{{ID: 10, Name: "two_copies", Replication: &two}}
			})

			Expect(newReconciler().reconcileMasterStatefulSet(ctx, cluster)).To(Succeed())

			sts := &appsv1.StatefulSet{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: name + "-master", Namespace: "default"}, sts)
			}).Should(Succeed())

			var goalsVol *corev1.Volume
			for i := range sts.Spec.Template.Spec.Volumes {
				if sts.Spec.Template.Spec.Volumes[i].Name == "leilfs-goals" {
					goalsVol = &sts.Spec.Template.Spec.Volumes[i]
				}
			}
			Expect(goalsVol).NotTo(BeNil())
			Expect(goalsVol.ConfigMap).NotTo(BeNil())
			Expect(goalsVol.ConfigMap.Name).To(Equal(name + "-master-goals"))

			initConfig := masterHAFindContainer(sts.Spec.Template.Spec.InitContainers, "init-config")
			Expect(initConfig).NotTo(BeNil())
			var hasGoalsMount bool
			for _, m := range initConfig.VolumeMounts {
				if m.Name == "leilfs-goals" {
					hasGoalsMount = true
					Expect(m.MountPath).To(Equal("/etc/leilfs-goals"))
					Expect(m.ReadOnly).To(BeTrue())
				}
			}
			Expect(hasGoalsMount).To(BeTrue())

			// The main container never mounts the goals ConfigMap directly; only
			// init-config copies the file into the shared /etc/saunafs emptyDir.
			master := masterHAFindContainer(sts.Spec.Template.Spec.Containers, "leilfs-master")
			for _, m := range master.VolumeMounts {
				Expect(m.Name).NotTo(Equal("leilfs-goals"))
			}
		})
	})

	// ── reconcileMasterHA ─────────────────────────────────────────────────────

	Describe("reconcileMasterHA", func() {
		It("is a no-op and removes a stale Lease when shadow is not configured", func() {
			name := "ha-slice-lease-noshadow"
			cluster := createCluster(name, nil)

			leaseName := name + "-master-ha"
			staleLease := &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: "default"},
			}
			Expect(k8sClient.Create(ctx, staleLease)).To(Succeed())

			Expect(newReconciler().reconcileMasterHA(ctx, cluster)).To(Succeed())

			Eventually(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: leaseName, Namespace: "default"}, &coordinationv1.Lease{})
				return apierrors.IsNotFound(err)
			}).Should(BeTrue())
		})

		It("bootstraps an empty Lease on first reconcile and is idempotent", func() {
			name := "ha-slice-lease-bootstrap"
			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(1)}
			})
			r := newReconciler()

			Expect(r.reconcileMasterHA(ctx, cluster)).To(Succeed())

			leaseName := name + "-master-ha"
			lease := &coordinationv1.Lease{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: leaseName, Namespace: "default"}, lease)
			}).Should(Succeed())
			Expect(lease.Spec.HolderIdentity).To(BeNil())
			Expect(lease.Spec.LeaseDurationSeconds).NotTo(BeNil())
			Expect(*lease.Spec.LeaseDurationSeconds).To(Equal(int32(30)))
			Expect(cluster.Status.ActiveMaster).To(BeEmpty())

			// Idempotent: reconciling again must not error or attempt to recreate
			// the Lease (Create would fail with AlreadyExists if it tried).
			Expect(r.reconcileMasterHA(ctx, cluster)).To(Succeed())
			again := &coordinationv1.Lease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaseName, Namespace: "default"}, again)).To(Succeed())
			Expect(again.UID).To(Equal(lease.UID))
		})

		It("labels the Lease holder as active-master, clears it from other pods, syncs the Service selector, and follows a failover to a new holder", func() {
			name := "ha-slice-lease-holder"
			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(1)}
			})

			// The master Service must already exist for setMasterServiceSelector to
			// succeed; reconcileMasterHA only ever updates it (creating it is
			// reconcileMasterService's job, out of scope for this test file), so we
			// create a stand-in directly.
			svcName := name + "-master"
			svc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: svcName, Namespace: "default"},
				Spec: corev1.ServiceSpec{
					Selector: map[string]string{"app.kubernetes.io/name": "leilfs-master", "app.kubernetes.io/instance": name},
					Ports:    []corev1.ServicePort{{Name: "admin", Port: 9419}},
				},
			}
			Expect(k8sClient.Create(ctx, svc)).To(Succeed())

			podLabels := map[string]string{"app.kubernetes.io/name": "leilfs-master", "app.kubernetes.io/instance": name}
			pod0 := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-master-0", Namespace: "default", Labels: podLabels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "leilfs-master", Image: "busybox"}}},
			}
			pod1 := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: name + "-master-1", Namespace: "default", Labels: podLabels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "leilfs-master", Image: "busybox"}}},
			}
			Expect(k8sClient.Create(ctx, pod0)).To(Succeed())
			Expect(k8sClient.Create(ctx, pod1)).To(Succeed())

			// Simulate the sidecar of pod-1 having won the election: a fresh Lease
			// with holderIdentity=pod-1.
			leaseName := name + "-master-ha"
			holder := pod1.Name
			now := metav1.NowMicro()
			durationSec := int32(30)
			lease := &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: "default"},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       &holder,
					RenewTime:            &now,
					LeaseDurationSeconds: &durationSec,
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())

			r := newReconciler()
			Expect(r.reconcileMasterHA(ctx, cluster)).To(Succeed())

			Eventually(func() (string, error) {
				got := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod1.Name, Namespace: "default"}, got); err != nil {
					return "", err
				}
				return got.Labels[labelActiveMaster], nil
			}).Should(Equal("true"))

			Eventually(func() (string, error) {
				got := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod0.Name, Namespace: "default"}, got); err != nil {
					return "", err
				}
				return got.Labels[labelActiveMaster], nil
			}).Should(BeEmpty())

			Eventually(func() (map[string]string, error) {
				got := &corev1.Service{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: svcName, Namespace: "default"}, got); err != nil {
					return nil, err
				}
				return got.Spec.Selector, nil
			}).Should(Equal(map[string]string{"app.kubernetes.io/instance": name, labelActiveMaster: "true"}))

			Expect(cluster.Status.ActiveMaster).To(Equal(pod1.Name))

			// Idempotent re-reconcile with the same holder: no error, same end state.
			Expect(r.reconcileMasterHA(ctx, cluster)).To(Succeed())
			Expect(cluster.Status.ActiveMaster).To(Equal(pod1.Name))

			// ── Failover: the Lease changes hands to pod-0 ─────────────────────
			// NOTE: this simulates whichever shadow's sidecar wins the Lease CAS
			// race, which is not necessarily the most up-to-date shadow — that is
			// a KNOWN, documented gap ("HA promeut le premier au Lease, pas le
			// shadow le plus à jour" in ROADMAP.md) and intentionally out of scope
			// here. This test only asserts that the controller correctly follows
			// whatever holderIdentity the Lease reports.
			refreshedLease := &coordinationv1.Lease{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: leaseName, Namespace: "default"}, refreshedLease)).To(Succeed())
			newHolder := pod0.Name
			refreshedLease.Spec.HolderIdentity = &newHolder
			newNow := metav1.NowMicro()
			refreshedLease.Spec.RenewTime = &newNow
			Expect(k8sClient.Update(ctx, refreshedLease)).To(Succeed())

			Expect(r.reconcileMasterHA(ctx, cluster)).To(Succeed())

			Eventually(func() (string, error) {
				got := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod0.Name, Namespace: "default"}, got); err != nil {
					return "", err
				}
				return got.Labels[labelActiveMaster], nil
			}).Should(Equal("true"))

			Eventually(func() (string, error) {
				got := &corev1.Pod{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: pod1.Name, Namespace: "default"}, got); err != nil {
					return "", err
				}
				return got.Labels[labelActiveMaster], nil
			}).Should(BeEmpty())

			Eventually(func() (map[string]string, error) {
				got := &corev1.Service{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: svcName, Namespace: "default"}, got); err != nil {
					return nil, err
				}
				return got.Spec.Selector, nil
			}).Should(Equal(map[string]string{"app.kubernetes.io/instance": name, labelActiveMaster: "true"}))

			Expect(cluster.Status.ActiveMaster).To(Equal(pod0.Name))
		})

		It("clears status.ActiveMaster without touching pod labels or the Service selector when the Lease has expired", func() {
			name := "ha-slice-lease-expired"
			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(1)}
				c.Status.ActiveMaster = name + "-master-0" // a previously known active master
			})

			leaseName := name + "-master-ha"
			holder := name + "-master-0"
			staleTime := metav1.NewMicroTime(time.Now().Add(-2 * leaseDuration))
			durationSec := int32(30)
			lease := &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{Name: leaseName, Namespace: "default"},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       &holder,
					RenewTime:            &staleTime,
					LeaseDurationSeconds: &durationSec,
				},
			}
			Expect(k8sClient.Create(ctx, lease)).To(Succeed())

			// Deliberately do NOT create the master Service or any Pods: if the
			// expired-Lease branch tried to sync labels/selector it would error
			// out (Service/Pods not found), so a passing test here also proves
			// that branch returns before reaching that code.
			Expect(newReconciler().reconcileMasterHA(ctx, cluster)).To(Succeed())

			Expect(cluster.Status.ActiveMaster).To(BeEmpty())
		})
	})

	// ── reconcileMasterHARBAC ─────────────────────────────────────────────────

	Describe("reconcileMasterHARBAC", func() {
		It("creates nothing when shadow is not configured", func() {
			name := "ha-slice-rbac-noshadow"
			cluster := createCluster(name, nil)

			Expect(newReconciler().reconcileMasterHARBAC(ctx, cluster)).To(Succeed())

			saName := name + "-master"
			Consistently(func() bool {
				err := k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"}, &corev1.ServiceAccount{})
				return apierrors.IsNotFound(err)
			}, "300ms", "50ms").Should(BeTrue())
		})

		It("creates the ServiceAccount, Role, and RoleBinding scoped to the master pods when shadow is configured", func() {
			name := "ha-slice-rbac-shadow"
			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(2)}
			})

			Expect(newReconciler().reconcileMasterHARBAC(ctx, cluster)).To(Succeed())

			saName := name + "-master"
			roleName := name + "-master-ha"
			leaseName := name + "-master-ha"

			sa := &corev1.ServiceAccount{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: saName, Namespace: "default"}, sa)
			}).Should(Succeed())

			role := &rbacv1.Role{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "default"}, role)
			}).Should(Succeed())

			var leaseRule, podRule *rbacv1.PolicyRule
			for i := range role.Rules {
				rule := &role.Rules[i]
				if len(rule.Resources) > 0 && rule.Resources[0] == "leases" {
					leaseRule = rule
				}
				if len(rule.Resources) > 0 && rule.Resources[0] == "pods" {
					podRule = rule
				}
			}
			Expect(leaseRule).NotTo(BeNil())
			Expect(leaseRule.ResourceNames).To(ConsistOf(leaseName))
			Expect(leaseRule.Verbs).To(ConsistOf("get", "update", "patch"))

			Expect(podRule).NotTo(BeNil())
			Expect(podRule.Verbs).To(ConsistOf("delete"))
			// 1 primary + 2 shadow replicas = 3 deterministic pod names.
			Expect(podRule.ResourceNames).To(ConsistOf(name+"-master-0", name+"-master-1", name+"-master-2"))

			rb := &rbacv1.RoleBinding{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "default"}, rb)
			}).Should(Succeed())
			Expect(rb.RoleRef).To(Equal(rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: roleName}))
			Expect(rb.Subjects).To(ConsistOf(rbacv1.Subject{Kind: "ServiceAccount", Name: saName, Namespace: "default"}))
		})

		It("copies imagePullSecrets from the namespace's default ServiceAccount", func() {
			name := "ha-slice-rbac-pullsecrets"
			// envtest does not run the serviceaccount admission/token controllers,
			// so the "default" SA is not auto-created; create or reuse it.
			defaultSA := &corev1.ServiceAccount{}
			err := k8sClient.Get(ctx, types.NamespacedName{Name: "default", Namespace: "default"}, defaultSA)
			if apierrors.IsNotFound(err) {
				defaultSA = &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}}
				Expect(k8sClient.Create(ctx, defaultSA)).To(Succeed())
			} else {
				Expect(err).NotTo(HaveOccurred())
			}
			originalPullSecrets := defaultSA.DeepCopy().ImagePullSecrets
			defaultSA.ImagePullSecrets = append(defaultSA.ImagePullSecrets, corev1.LocalObjectReference{Name: "ha-slice-regcred"})
			Expect(k8sClient.Update(ctx, defaultSA)).To(Succeed())
			DeferCleanup(func() {
				latest := &corev1.ServiceAccount{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: "default", Namespace: "default"}, latest); err == nil {
					latest.ImagePullSecrets = originalPullSecrets
					_ = k8sClient.Update(ctx, latest)
				}
			})

			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(1)}
			})

			Expect(newReconciler().reconcileMasterHARBAC(ctx, cluster)).To(Succeed())

			Eventually(func() ([]corev1.LocalObjectReference, error) {
				sa := &corev1.ServiceAccount{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: name + "-master", Namespace: "default"}, sa); err != nil {
					return nil, err
				}
				return sa.ImagePullSecrets, nil
			}).Should(ContainElement(corev1.LocalObjectReference{Name: "ha-slice-regcred"}))
		})

		It("is idempotent and updates the Role's pod ResourceNames in place when shadow replicas change", func() {
			name := "ha-slice-rbac-idempotent"
			cluster := createCluster(name, func(c *saunafsv1alpha1.LeilFSCluster) {
				c.Spec.Shadow = &saunafsv1alpha1.ShadowSpec{Replicas: int32Ptr(1)}
			})
			r := newReconciler()

			Expect(r.reconcileMasterHARBAC(ctx, cluster)).To(Succeed())

			roleName := name + "-master-ha"
			role := &rbacv1.Role{}
			Eventually(func() error {
				return k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "default"}, role)
			}).Should(Succeed())
			firstUID := role.UID

			var podRule *rbacv1.PolicyRule
			for i := range role.Rules {
				if len(role.Rules[i].Resources) > 0 && role.Rules[i].Resources[0] == "pods" {
					podRule = &role.Rules[i]
				}
			}
			Expect(podRule).NotTo(BeNil())
			Expect(podRule.ResourceNames).To(ConsistOf(name+"-master-0", name+"-master-1"))

			cluster.Spec.Shadow.Replicas = int32Ptr(3)
			Expect(r.reconcileMasterHARBAC(ctx, cluster)).To(Succeed())

			Eventually(func() ([]string, error) {
				updated := &rbacv1.Role{}
				if err := k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "default"}, updated); err != nil {
					return nil, err
				}
				for i := range updated.Rules {
					if len(updated.Rules[i].Resources) > 0 && updated.Rules[i].Resources[0] == "pods" {
						return updated.Rules[i].ResourceNames, nil
					}
				}
				return nil, nil
			}).Should(ConsistOf(name+"-master-0", name+"-master-1", name+"-master-2", name+"-master-3"))

			final := &rbacv1.Role{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: roleName, Namespace: "default"}, final)).To(Succeed())
			Expect(final.UID).To(Equal(firstUID), "expected an in-place Role update, not a recreation")
		})
	})
})
