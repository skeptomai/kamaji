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

// Failure Mode 6: operator rolling upgrade.
//
// Rolling a new operator version (here simulated by a pod-template change that
// forces a rollout with the same image) must not break admission or stall
// reconciliation.
//
// NOTE: a Deployment rolling update is governed by the Deployment's own strategy
// (maxUnavailable/maxSurge), NOT by a PodDisruptionBudget — the PDB only
// constrains the Eviction API (drains), which is Failure Mode 4. With the
// default strategy at 2 replicas, maxUnavailable resolves to 0 and maxSurge to
// 1, so a ready endpoint is always present. This test pins that the webhook
// stays reachable, leadership transfers to a rolled pod, and the new leader
// still reconciles.
var _ = Describe("Kamaji operator survives a rolling upgrade", func() {
	var tcp *kamajiv1alpha1.TenantControlPlane

	BeforeEach(func() {
		scaleOperator(2)
		Eventually(leaderPodName, 60*time.Second, time.Second).ShouldNot(BeEmpty())

		tcp = &kamajiv1alpha1.TenantControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "tcp-rolling-upgrade", Namespace: "default"},
			Spec: kamajiv1alpha1.TenantControlPlaneSpec{
				ControlPlane: kamajiv1alpha1.ControlPlane{
					Deployment: kamajiv1alpha1.DeploymentSpec{Replicas: pointer.To(int32(1))},
					Service:    kamajiv1alpha1.ServiceSpec{ServiceType: "ClusterIP"},
				},
				NetworkProfile: kamajiv1alpha1.NetworkProfileSpec{Address: "172.18.0.6"},
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
		StatusMustEqualTo(tcp, kamajiv1alpha1.VersionReady)
	})

	JustAfterEach(func() {
		scaleOperator(1)
	})

	It("keeps admission available and reconciliation healthy across the roll", func() {
		By("triggering a rolling restart of the operator")
		deploy := operatorDeployment()
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = map[string]string{}
		}
		deploy.Spec.Template.Annotations["ha.kamaji.clastix.io/restartedAt"] = "rolling-upgrade-test"
		Expect(k8sClient.Update(context.Background(), deploy)).To(Succeed())
		// Update populates the object with the post-change generation; the
		// rollout is complete once the status observes at least this.
		rolloutGeneration := deploy.Generation

		// Webhooks are served by every replica, so a rollout that keeps at least
		// one ready endpoint (default strategy: maxUnavailable 0) must never drop
		// admission. Each poll is a real UPDATE through the Fail-policy webhook.
		By("admission stays available throughout the rollout")
		probe := 0
		Consistently(func(g Gomega) {
			probe++
			base := tcp.DeepCopy()
			if tcp.Annotations == nil {
				tcp.Annotations = map[string]string{}
			}
			tcp.Annotations["ha.kamaji.clastix.io/probe"] = fmt.Sprintf("%d", probe)
			g.Expect(k8sClient.Patch(context.Background(), tcp, client.MergeFrom(base))).To(Succeed())
		}, 45*time.Second, 2*time.Second).Should(Succeed())

		By("the rollout completes with all replicas updated and ready")
		Eventually(func(g Gomega) {
			d := operatorDeployment()
			g.Expect(d.Status.ObservedGeneration).To(BeNumerically(">=", rolloutGeneration))
			g.Expect(d.Status.UpdatedReplicas).To(Equal(int32(2)))
			g.Expect(d.Status.ReadyReplicas).To(Equal(int32(2)))
		}, 120*time.Second, 2*time.Second).Should(Succeed())

		By("a leader is held by one of the rolled replicas")
		var leader string
		Eventually(func() string {
			leader = leaderPodName()

			return leader
		}, 60*time.Second, time.Second).ShouldNot(BeEmpty())
		Expect(controllerPods()).To(ContainElement(leader))

		By("the new leader still reconciles: a post-roll spec change converges")
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(tcp), tcp)).To(Succeed())
		base := tcp.DeepCopy()
		tcp.Spec.ControlPlane.Deployment.Replicas = pointer.To(int32(2))
		Expect(k8sClient.Patch(context.Background(), tcp, client.MergeFrom(base))).To(Succeed())
		wantGeneration := tcp.Generation

		StatusMustEqualTo(tcp, kamajiv1alpha1.VersionReady)
		Eventually(func() int64 {
			if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(tcp), tcp); err != nil {
				return -1
			}

			return tcp.Status.ObservedGeneration
		}, 2*time.Minute, time.Second).Should(Equal(wantGeneration),
			"ObservedGeneration should catch up to the post-roll spec change")
	})
})
