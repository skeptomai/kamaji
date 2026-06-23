// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pointer "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
)

// Failure Mode 3: mid-flight reconcile interruption.
//
// Killing the leader while it is part-way through a multi-step operation (here,
// a Kubernetes version upgrade) must not leave the TenantControlPlane wedged:
// the new leader has to resume and drive it to completion. This exercises
// reconciler idempotency/resumability — the property most at risk when the
// controllers are validated only end-to-end.
var _ = Describe("Kamaji operator failover during a TenantControlPlane upgrade", func() {
	// A linear minor upgrade from the baseline; must be a version the operator
	// supports (non-linear jumps are rejected by the version webhook).
	const (
		fromVersion = "v1.23.6"
		toVersion   = "v1.24.0"
	)

	var tcp *kamajiv1alpha1.TenantControlPlane

	BeforeEach(func() {
		scaleOperator(2)
		Eventually(leaderPodName, 60*time.Second, time.Second).ShouldNot(BeEmpty())

		tcp = &kamajiv1alpha1.TenantControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "tcp-failover-upgrade", Namespace: "default"},
			Spec: kamajiv1alpha1.TenantControlPlaneSpec{
				ControlPlane: kamajiv1alpha1.ControlPlane{
					Deployment: kamajiv1alpha1.DeploymentSpec{Replicas: pointer.To(int32(1))},
					Service:    kamajiv1alpha1.ServiceSpec{ServiceType: "ClusterIP"},
				},
				NetworkProfile: kamajiv1alpha1.NetworkProfileSpec{Address: controlPlaneAddress(5)},
				Kubernetes: kamajiv1alpha1.KubernetesSpec{
					Version: fromVersion,
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

		// Reach a clean steady state before perturbing it.
		StatusMustEqualTo(tcp, kamajiv1alpha1.VersionReady)
	})

	JustAfterEach(func() {
		scaleOperator(1)
	})

	It("completes the upgrade after the leader is killed mid-flight", func() {
		originalLeader := leaderPodName()
		Expect(originalLeader).NotTo(BeEmpty())

		By("requesting the upgrade to " + toVersion)
		Expect(k8sClient.Get(context.Background(), client.ObjectKeyFromObject(tcp), tcp)).To(Succeed())
		base := tcp.DeepCopy()
		tcp.Spec.Kubernetes.Version = toVersion
		Expect(k8sClient.Patch(context.Background(), tcp, client.MergeFrom(base))).To(Succeed())

		By("killing the leader while the upgrade is in flight: " + originalLeader)
		Expect(k8sClient.Delete(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: originalLeader, Namespace: operatorNamespace},
		})).To(Succeed())

		By("a surviving replica takes over leadership")
		Eventually(leaderPodName, 60*time.Second, time.Second).
			ShouldNot(Or(BeEmpty(), Equal(originalLeader)))

		By("the upgrade converges to Ready under the new leader")
		StatusMustEqualTo(tcp, kamajiv1alpha1.VersionReady)

		By("and the running version reflects the requested upgrade")
		Eventually(func() string {
			if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(tcp), tcp); err != nil {
				return ""
			}

			return tcp.Status.Kubernetes.Version.Version
		}, 5*time.Minute, time.Second).Should(Equal(toVersion))
	})
})
