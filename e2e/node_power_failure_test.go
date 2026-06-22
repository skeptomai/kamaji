//go:build multinode

// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pointer "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
)

// Failure Mode 10 (MULTI-NODE, HARDWARE FAULT): total node power-off.
//
// Powers off — for real, via the KAMAJI_E2E_POWER_OFF hook — a node hosting a
// controller replica, and asserts admission keeps working. This exercises the
// EndpointSlice-pruning window: the dead node's pod lingers in the webhook
// Service's endpoints until the node-monitor grace period prunes it, and a
// Fail-policy webhook call routed to it would fail during that window. With a
// replica on a surviving node, admission must recover and stay up. The node is
// powered back on and must rejoin.
var _ = Describe("Admission survives a total node power-off", Label("multinode"), func() {
	var tcp *kamajiv1alpha1.TenantControlPlane
	var poweredOff string

	BeforeEach(func() {
		requireFaultHooks("KAMAJI_E2E_POWER_OFF", "KAMAJI_E2E_POWER_ON")
		if readyNodeCount() < 2 {
			Skip("FM10 needs >= 2 Ready nodes")
		}

		scaleOperator(2)
		// Both replicas on distinct nodes, so one survives the power-off.
		Eventually(func(g Gomega) {
			nodes := map[string]bool{}
			for _, n := range operatorPodNodes() {
				nodes[n] = true
			}
			g.Expect(nodes).To(HaveLen(2))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())

		// A TCP to drive webhook traffic against. We only need it to exist (its
		// create must pass admission); its tenant pods' readiness is irrelevant
		// to whether the operator's admission webhook answers.
		tcp = &kamajiv1alpha1.TenantControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "tcp-node-poweroff", Namespace: "default"},
			Spec: kamajiv1alpha1.TenantControlPlaneSpec{
				ControlPlane: kamajiv1alpha1.ControlPlane{
					Deployment: kamajiv1alpha1.DeploymentSpec{Replicas: pointer.To(int32(1))},
					Service:    kamajiv1alpha1.ServiceSpec{ServiceType: "ClusterIP"},
				},
				Kubernetes: kamajiv1alpha1.KubernetesSpec{
					Version: "v1.23.6",
					Kubelet: kamajiv1alpha1.KubeletSpec{CGroupFS: "cgroupfs"},
					AdmissionControllers: kamajiv1alpha1.AdmissionControllers{
						"LimitRanger",
						"ResourceQuota",
					},
				},
				Addons: kamajiv1alpha1.AddonsSpec{},
			},
		}
		Expect(k8sClient.Create(context.Background(), tcp)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), tcp)
		})
	})

	AfterEach(func() {
		// Always bring the node back, even on failure.
		if poweredOff != "" {
			powerOn(poweredOff)
			Eventually(func() bool { return nodeIsReady(poweredOff) }, 5*time.Minute, 5*time.Second).
				Should(BeTrue(), "node %s should rejoin after power-on", poweredOff)
			poweredOff = ""
		}
	})

	It("keeps admitting writes through the failure and recovers after power-on", func() {
		target := ""
		for _, n := range operatorPodNodes() {
			target = n

			break
		}
		Expect(target).NotTo(BeEmpty())

		By("powering off node " + target)
		powerOff(target)
		poweredOff = target

		By("the node goes NotReady")
		Eventually(func() bool { return nodeIsReady(target) }, 3*time.Minute, 5*time.Second).
			Should(BeFalse())

		// Each poll is a real UPDATE through the Fail-policy mutating webhook.
		// Admission must recover (once the dead pod is pruned from endpoints, if
		// it was even selected) and then stay up.
		probe := 0
		patchWrite := func(g Gomega) {
			probe++
			base := tcp.DeepCopy()
			if tcp.Annotations == nil {
				tcp.Annotations = map[string]string{}
			}
			tcp.Annotations["ha.kamaji.clastix.io/probe"] = fmt.Sprintf("%d", probe)
			g.Expect(k8sClient.Patch(context.Background(), tcp, client.MergeFrom(base))).To(Succeed())
		}

		By("admission recovers while the node is down")
		Eventually(patchWrite, 2*time.Minute, 2*time.Second).Should(Succeed())

		By("and stays available")
		Consistently(patchWrite, 20*time.Second, 2*time.Second).Should(Succeed())

		By("powering the node back on")
		powerOn(target)
		poweredOff = "" // handled here; AfterEach must not power-on again
		Eventually(func() bool { return nodeIsReady(target) }, 5*time.Minute, 5*time.Second).
			Should(BeTrue())

		By("the operator returns to two ready replicas")
		Eventually(func() int32 {
			return operatorDeployment().Status.ReadyReplicas
		}, 3*time.Minute, 5*time.Second).Should(Equal(int32(2)))
	})
})
