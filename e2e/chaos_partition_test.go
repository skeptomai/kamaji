//go:build chaos

// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// Failure Mode 7 (CHAOS LANE): network partition / split-brain of the leader.
//
// Requires chaos-mesh installed in the cluster (see `make e2e-chaos`). This file
// is behind the `chaos` build tag and the specs carry Label("chaos"); run with:
//
//	ginkgo --tags=chaos --label-filter=chaos ./e2e
//
// The active leader is partitioned from the API server. controller-runtime must
// fail to renew its lease and self-terminate (os.Exit), so a standby takes over.
// The partitioned manager must NOT keep acting as a second leader — that is the
// dangerous split-brain this test rules out.
var _ = Describe("Leader self-terminates when partitioned from the API server", Label("chaos"), func() {
	var chaos *unstructured.Unstructured

	BeforeEach(func() {
		scaleOperator(2)
		Eventually(leaderPodName, 60*time.Second, time.Second).ShouldNot(BeEmpty())
	})

	AfterEach(func() {
		if chaos != nil {
			_ = k8sClient.Delete(context.Background(), chaos)
			chaos = nil
		}
		scaleOperator(1)
	})

	It("relinquishes leadership under partition and a standby takes over", func() {
		originalLeader := leaderPodName()
		Expect(originalLeader).NotTo(BeEmpty())
		baselineRestarts := containerRestarts(originalLeader)

		By("partitioning the leader pod from the kube-apiserver via chaos-mesh NetworkChaos")
		// Target the leader pod by exact name; cut its egress to the apiserver
		// static pods in kube-system. chaos-mesh injects this in the pod netns,
		// so it works regardless of the CNI's NetworkPolicy support.
		chaos = &unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "chaos-mesh.org/v1alpha1",
				"kind":       "NetworkChaos",
				"metadata": map[string]interface{}{
					"name":      "kamaji-leader-partition",
					"namespace": operatorNamespace,
				},
				"spec": map[string]interface{}{
					"action":    "partition",
					"direction": "to",
					"mode":      "all",
					"selector": map[string]interface{}{
						"namespaces":     []interface{}{operatorNamespace},
						"fieldSelectors": map[string]interface{}{"metadata.name": originalLeader},
					},
					"target": map[string]interface{}{
						"mode": "all",
						"selector": map[string]interface{}{
							"namespaces":     []interface{}{"kube-system"},
							"labelSelectors": map[string]interface{}{"component": "kube-apiserver"},
						},
					},
				},
			},
		}
		Expect(k8sClient.Create(context.Background(), chaos)).To(Succeed())

		By("a different pod acquires leadership (the partitioned leader steps down)")
		Eventually(leaderPodName, 3*time.Minute, 2*time.Second).
			ShouldNot(Or(BeEmpty(), Equal(originalLeader)))

		By("the partitioned manager self-terminated (its container restarted)")
		Eventually(func() int32 {
			return containerRestarts(originalLeader)
		}, 2*time.Minute, 2*time.Second).Should(BeNumerically(">", baselineRestarts))
	})
})

// containerRestarts returns the total container restart count for a pod, or -1
// if the pod cannot be read.
func containerRestarts(pod string) int32 {
	p := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: pod, Namespace: operatorNamespace}, p); err != nil {
		return -1
	}

	var total int32
	for i := range p.Status.ContainerStatuses {
		total += p.Status.ContainerStatuses[i].RestartCount
	}

	return total
}
