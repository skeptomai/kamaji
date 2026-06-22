// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Failure Mode 4: PodDisruptionBudget enforcement under voluntary disruption.
//
// The chart renders a PDB (charts/kamaji/templates/pdb.yaml) only when
// replicaCount > 1. The e2e environment installs the operator at the default
// replicaCount: 1, so the chart PDB is intentionally absent there. This test
// scales the operator to HA and applies a PDB mirroring the chart's render
// (maxUnavailable: 1) to prove the budget actually protects the operator: the
// Eviction API — what `kubectl drain` uses — must refuse to take both replicas
// down at once.
//
// NOTE: the chart's *gating* (replicaCount: 1 -> no PDB; replicaCount: 2 -> a
// PDB with maxUnavailable 1) is a templating property, best verified with
// `helm template` in a chart unit test; it is not exercised here.
var _ = Describe("PodDisruptionBudget protects the operator under voluntary disruption", func() {
	const pdbName = "kamaji-ha-test"

	var clientset *kubernetes.Clientset

	BeforeEach(func() {
		var err error
		clientset, err = kubernetes.NewForConfig(cfg)
		Expect(err).NotTo(HaveOccurred())

		scaleOperator(2)
		Eventually(leaderPodName, 60*time.Second, time.Second).ShouldNot(BeEmpty())

		maxUnavailable := intstr.FromInt32(1)
		pdb := &policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Name: pdbName, Namespace: operatorNamespace},
			Spec: policyv1.PodDisruptionBudgetSpec{
				MaxUnavailable: &maxUnavailable,
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app.kubernetes.io/component": "controller-manager"},
				},
			},
		}
		Expect(k8sClient.Create(context.Background(), pdb)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), pdb)
		})
	})

	JustAfterEach(func() {
		scaleOperator(1)
	})

	It("allows exactly one voluntary disruption at a time", func() {
		pdbKey := client.ObjectKey{Name: pdbName, Namespace: operatorNamespace}

		By("the budget observes both replicas and allows one disruption")
		Eventually(func(g Gomega) {
			pdb := &policyv1.PodDisruptionBudget{}
			g.Expect(k8sClient.Get(context.Background(), pdbKey, pdb)).To(Succeed())
			g.Expect(pdb.Status.ExpectedPods).To(Equal(int32(2)))
			g.Expect(pdb.Status.DisruptionsAllowed).To(Equal(int32(1)))
		}, 60*time.Second, time.Second).Should(Succeed())

		pods := controllerPods()
		Expect(pods).To(HaveLen(2))

		By("evicting one replica succeeds")
		Expect(clientset.CoreV1().Pods(operatorNamespace).EvictV1(context.Background(), &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: pods[0], Namespace: operatorNamespace},
		})).To(Succeed())

		// Wait until the budget reflects the disruption, so the second eviction
		// is denied deterministically rather than racing the PDB controller.
		By("the budget is now exhausted")
		Eventually(func() int32 {
			pdb := &policyv1.PodDisruptionBudget{}
			if err := k8sClient.Get(context.Background(), pdbKey, pdb); err != nil {
				return -1
			}

			return pdb.Status.DisruptionsAllowed
		}, 30*time.Second, time.Second).Should(Equal(int32(0)))

		By("evicting the second, still-healthy replica is refused (429)")
		err := clientset.CoreV1().Pods(operatorNamespace).EvictV1(context.Background(), &policyv1.Eviction{
			ObjectMeta: metav1.ObjectMeta{Name: pods[1], Namespace: operatorNamespace},
		})
		Expect(apierrors.IsTooManyRequests(err)).To(BeTrue(),
			"second eviction should be denied by the PodDisruptionBudget, got: %v", err)
	})
})
