// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	pointer "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
)

const (
	operatorNamespace = "kamaji-system"
	// LeaderElectionID configured in cmd/manager/cmd.go; controller-runtime
	// backs it with a coordination.k8s.io/v1 Lease of the same name.
	leaderElectionLease = "kamaji.clastix.io"
)

// leaderPodName returns the pod currently holding the leader-election Lease.
// controller-runtime encodes the holder identity as "<podName>_<uuid>".
func leaderPodName() string {
	lease := &coordinationv1.Lease{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: leaderElectionLease, Namespace: operatorNamespace},
		lease); err != nil {
		return ""
	}

	if lease.Spec.HolderIdentity == nil {
		return ""
	}

	return strings.SplitN(*lease.Spec.HolderIdentity, "_", 2)[0]
}

func operatorDeployment() *appsv1.Deployment {
	list := &appsv1.DeploymentList{}
	Expect(k8sClient.List(context.Background(), list,
		client.InNamespace(operatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": "controller-manager"})).To(Succeed())
	Expect(list.Items).To(HaveLen(1), "expected exactly one Kamaji controller Deployment")

	return &list.Items[0]
}

func scaleOperator(replicas int32) {
	deploy := operatorDeployment()
	deploy.Spec.Replicas = pointer.To(replicas)
	Expect(k8sClient.Update(context.Background(), deploy)).To(Succeed())

	Eventually(func() int32 {
		d := operatorDeployment()

		return d.Status.ReadyReplicas
	}, 120*time.Second, 2*time.Second).Should(Equal(replicas),
		"operator should report %d ready replicas", replicas)
}

var _ = Describe("Kamaji operator leader failover", func() {
	// Bring the operator up in active/passive HA, and always restore the
	// single-replica default the rest of the suite (and utils_test.go) expects.
	BeforeEach(func() {
		scaleOperator(2)

		Eventually(leaderPodName, 60*time.Second, time.Second).ShouldNot(BeEmpty(),
			"a leader should hold the lease before failover")
	})

	JustAfterEach(func() {
		scaleOperator(1)
	})

	It("elects a new leader and resumes reconciliation after the leader pod dies", func() {
		originalLeader := leaderPodName()
		Expect(originalLeader).NotTo(BeEmpty())

		By("deleting the current leader pod: " + originalLeader)
		Expect(k8sClient.Delete(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: originalLeader, Namespace: operatorNamespace},
		})).To(Succeed())

		By("a different pod should acquire the lease within the lease duration")
		Eventually(leaderPodName, 60*time.Second, time.Second).
			ShouldNot(Or(BeEmpty(), Equal(originalLeader)),
				"a surviving replica should take over leadership")

		By("a TenantControlPlane created after failover should still reconcile to Ready")
		tcp := &kamajiv1alpha1.TenantControlPlane{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "tcp-failover",
				Namespace: "default",
			},
			Spec: kamajiv1alpha1.TenantControlPlaneSpec{
				ControlPlane: kamajiv1alpha1.ControlPlane{
					Deployment: kamajiv1alpha1.DeploymentSpec{Replicas: pointer.To(int32(1))},
					Service:    kamajiv1alpha1.ServiceSpec{ServiceType: "ClusterIP"},
				},
				NetworkProfile: kamajiv1alpha1.NetworkProfileSpec{Address: "172.18.0.3"},
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

		// Proves the new leader is actually doing work, not merely holding the lease.
		StatusMustEqualTo(tcp, kamajiv1alpha1.VersionReady)
	})
})
