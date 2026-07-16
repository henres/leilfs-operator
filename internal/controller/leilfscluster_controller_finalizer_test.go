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

// This file covers the LeilFSCluster finalizer lifecycle wired into the top
// of Reconcile: registering leilfsClusterFinalizer on a live cluster, and
// clearing it (after finalizeCluster runs) once the cluster is marked for
// deletion so the API server can actually remove the object.
//
// envtest runs only the API server + etcd, not a live manager loop, so
// nothing re-reconciles a cluster automatically after Delete() sets its
// DeletionTimestamp. Every spec below therefore drives the second (deletion)
// reconcile explicitly. forceDeleteCluster is a DeferCleanup-only backstop —
// mirroring forceDeletePV in leilfscluster_controller_chunk_test.go — so a
// failed assertion mid-spec can never leak a zombie CR into the shared
// "default" namespace for the rest of the suite.

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saunafsv1alpha1 "github.com/henres/leilfs-operator/api/v1alpha1"
)

var _ = Describe("LeilFSCluster finalizer", func() {
	ctx := context.Background()

	newReconciler := func() *LeilFSClusterReconciler {
		return &LeilFSClusterReconciler{Client: k8sClient, Scheme: k8sClient.Scheme()}
	}

	// forceDeleteCluster deletes the cluster and, if it is still present
	// (e.g. because the spec never got around to running the deletion
	// reconcile, or that reconcile is what's under test and failed),
	// manually strips its finalizers so it doesn't linger forever. See the
	// file-level comment for why this is necessary under envtest.
	forceDeleteCluster := func(cluster *saunafsv1alpha1.LeilFSCluster) {
		_ = k8sClient.Delete(ctx, cluster)
		latest := &saunafsv1alpha1.LeilFSCluster{}
		if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(cluster), latest); err == nil {
			latest.Finalizers = nil
			_ = k8sClient.Update(ctx, latest)
		}
	}

	createCluster := func(name string) *saunafsv1alpha1.LeilFSCluster {
		cluster := &saunafsv1alpha1.LeilFSCluster{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		}
		Expect(k8sClient.Create(ctx, cluster)).To(Succeed())
		DeferCleanup(func() { forceDeleteCluster(cluster) })
		return cluster
	}

	It("adds the finalizer the first time a live cluster is reconciled", func() {
		cluster := createCluster("finalizer-add")
		key := types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}

		_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		refreshed := &saunafsv1alpha1.LeilFSCluster{}
		Expect(k8sClient.Get(ctx, key, refreshed)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(refreshed, leilfsClusterFinalizer)).To(BeTrue())
	})

	It("is idempotent: reconciling an already-finalized live cluster again doesn't duplicate the finalizer", func() {
		cluster := createCluster("finalizer-idempotent")
		key := types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}
		r := newReconciler()

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		refreshed := &saunafsv1alpha1.LeilFSCluster{}
		Expect(k8sClient.Get(ctx, key, refreshed)).To(Succeed())
		count := 0
		for _, f := range refreshed.Finalizers {
			if f == leilfsClusterFinalizer {
				count++
			}
		}
		Expect(count).To(Equal(1))
	})

	It("clears the finalizer on deletion and lets the CR actually disappear, with no hang", func() {
		cluster := createCluster("finalizer-delete")
		key := types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}
		r := newReconciler()

		By("reconciling once to register the finalizer")
		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		withFinalizer := &saunafsv1alpha1.LeilFSCluster{}
		Expect(k8sClient.Get(ctx, key, withFinalizer)).To(Succeed())
		Expect(controllerutil.ContainsFinalizer(withFinalizer, leilfsClusterFinalizer)).To(BeTrue())

		By("deleting via the normal envtest path: this only sets DeletionTimestamp")
		Expect(k8sClient.Delete(ctx, withFinalizer)).To(Succeed())

		stillPresent := &saunafsv1alpha1.LeilFSCluster{}
		Expect(k8sClient.Get(ctx, key, stillPresent)).To(Succeed())
		Expect(stillPresent.DeletionTimestamp).NotTo(BeNil())
		Expect(controllerutil.ContainsFinalizer(stillPresent, leilfsClusterFinalizer)).To(BeTrue())

		By("reconciling again — what a running manager does in response to the delete-triggered watch event")
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &saunafsv1alpha1.LeilFSCluster{}))
		}).Should(BeTrue(), "CR should be fully removed once the finalizer is cleared")
	})

	It("tolerates a reconcile after the finalizer is already gone (e.g. a stale/duplicate delete event)", func() {
		cluster := createCluster("finalizer-double-delete")
		key := types.NamespacedName{Name: cluster.Name, Namespace: cluster.Namespace}
		r := newReconciler()

		_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())

		withFinalizer := &saunafsv1alpha1.LeilFSCluster{}
		Expect(k8sClient.Get(ctx, key, withFinalizer)).To(Succeed())
		Expect(k8sClient.Delete(ctx, withFinalizer)).To(Succeed())

		// First deletion reconcile clears the finalizer and lets the object go.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
		Eventually(func() bool {
			return apierrors.IsNotFound(k8sClient.Get(ctx, key, &saunafsv1alpha1.LeilFSCluster{}))
		}).Should(BeTrue())

		// A second reconcile for the same (now nonexistent) object must be a
		// harmless no-op, not an error — this is what a duplicate/late watch
		// event delivering the same delete would trigger in a real manager.
		_, err = r.Reconcile(ctx, reconcile.Request{NamespacedName: key})
		Expect(err).NotTo(HaveOccurred())
	})
})
