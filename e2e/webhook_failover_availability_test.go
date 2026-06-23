// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pointer "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
)

// Failure Mode 2: webhook availability during failover.
//
// Admission webhook serving is NOT gated by leader election — every replica
// runs its own webhook server, reached through a Service. The operator ships
// Fail-policy webhooks, so with a single replica an operator outage is a hard
// outage (admission is rejected). With 2+ replicas the survivor must keep
// admitting TenantControlPlane writes through the failover window.
var _ = Describe("Admission webhooks stay available during operator failover", func() {
	var tcp *kamajiv1alpha1.TenantControlPlane

	BeforeEach(func() {
		scaleOperator(2)
		Eventually(leaderPodName, 60*time.Second, time.Second).ShouldNot(BeEmpty())

		tcp = &kamajiv1alpha1.TenantControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "tcp-webhook-failover", Namespace: "default"},
			Spec: kamajiv1alpha1.TenantControlPlaneSpec{
				ControlPlane: kamajiv1alpha1.ControlPlane{
					Deployment: kamajiv1alpha1.DeploymentSpec{Replicas: pointer.To(int32(1))},
					Service:    kamajiv1alpha1.ServiceSpec{ServiceType: "ClusterIP"},
				},
				NetworkProfile: kamajiv1alpha1.NetworkProfileSpec{Address: controlPlaneAddress(4)},
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
		// The create itself must pass admission (mutating + validating webhooks).
		Expect(k8sClient.Create(context.Background(), tcp)).To(Succeed())
		DeferCleanup(func() {
			_ = k8sClient.Delete(context.Background(), tcp)
		})
	})

	JustAfterEach(func() {
		scaleOperator(1)
	})

	It("admits TenantControlPlane writes throughout the failover window", func() {
		leader := leaderPodName()
		Expect(leader).NotTo(BeEmpty())

		By("killing the leader: webhooks are served by every replica, so admission must survive")
		Expect(k8sClient.Delete(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: leader, Namespace: operatorNamespace},
		})).To(Succeed())

		// Each invocation patches an annotation — an UPDATE that traverses the
		// Fail-policy mutating webhook. If the survivor were not serving
		// admission (or the dying pod lingered in the webhook Service's
		// endpoints), the API server would reject these with a webhook error.
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

		// Admission must (re)become available promptly — not after a full pod
		// reschedule as it would with a single replica — and then stay up. A
		// sustained failure here is the single-replica outage this HA topology
		// is meant to eliminate. (If brief endpoint-removal lag for the dying
		// pod causes a transient blip, that is itself actionable: it points to
		// a preStop hook / terminationGracePeriod gap.)
		By("admission recovers quickly")
		Eventually(patchWrite, 30*time.Second, time.Second).Should(Succeed())

		By("and remains available")
		Consistently(patchWrite, 15*time.Second, 2*time.Second).Should(Succeed())
	})
})
