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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	pointer "k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kamajiv1alpha1 "github.com/clastix/kamaji/api/v1alpha1"
)

// Failure Mode 8 (CHAOS LANE): datastore member loss.
//
// Tenant control-plane availability rides on the datastore, not the operator.
// Killing one member of a multi-member (quorum) datastore must not take the
// tenant control plane down: quorum holds, and the datastore recovers the lost
// member. Behind the `chaos` build tag + Label("chaos"); run via `make e2e-chaos`.
//
// NOTE: the datastore pod selector below is environment-specific. The test Skips
// (rather than fails) if it cannot find a multi-member datastore, so adjust the
// label for your install. A latency/partition variant (chaos-mesh IOChaos or a
// toxiproxy sidecar in front of the datastore) is a natural extension.
var _ = Describe("Tenant control plane survives a datastore member loss", Label("chaos"), func() {
	// The e2e env installs etcd via the clastix/kamaji-etcd chart as Helm release
	// "etcd-primary" in kamaji-system (see the Makefile `datastore-etcd` target),
	// so members carry app.kubernetes.io/instance=etcd-primary. Adjust per install.
	const (
		datastoreLabelKey   = "app.kubernetes.io/instance"
		datastoreLabelValue = "etcd-primary"
	)

	var tcp *kamajiv1alpha1.TenantControlPlane

	BeforeEach(func() {
		// Operator HA is orthogonal to datastore resilience; keep it simple.
		scaleOperator(1)

		tcp = &kamajiv1alpha1.TenantControlPlane{
			ObjectMeta: metav1.ObjectMeta{Name: "tcp-datastore-chaos", Namespace: "default"},
			Spec: kamajiv1alpha1.TenantControlPlaneSpec{
				ControlPlane: kamajiv1alpha1.ControlPlane{
					Deployment: kamajiv1alpha1.DeploymentSpec{Replicas: pointer.To(int32(1))},
					Service:    kamajiv1alpha1.ServiceSpec{ServiceType: "ClusterIP"},
				},
				NetworkProfile: kamajiv1alpha1.NetworkProfileSpec{Address: "172.18.0.7"},
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

	It("stays Ready when one datastore member is killed and the member recovers", func() {
		pods := datastorePods(datastoreLabelKey, datastoreLabelValue)
		if len(pods) < 2 {
			Skip("could not locate a multi-member datastore via " +
				datastoreLabelKey + "=" + datastoreLabelValue +
				"; adjust the selector for this environment")
		}
		original := len(pods)

		By("killing one datastore member")
		Expect(k8sClient.Delete(context.Background(), &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: pods[0], Namespace: operatorNamespace},
		})).To(Succeed())

		By("the tenant control plane stays Ready through the member loss (quorum holds)")
		Consistently(func() kamajiv1alpha1.KubernetesVersionStatus {
			if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(tcp), tcp); err != nil {
				return ""
			}
			if tcp.Status.Kubernetes.Version.Status == nil {
				return ""
			}

			return *tcp.Status.Kubernetes.Version.Status
		}, 30*time.Second, 2*time.Second).Should(Equal(kamajiv1alpha1.VersionReady))

		By("the datastore recovers its full membership")
		Eventually(func() int {
			return len(datastorePods(datastoreLabelKey, datastoreLabelValue))
		}, 2*time.Minute, 2*time.Second).Should(Equal(original))
	})
})

// datastorePods returns the names of Running datastore pods matching the label.
func datastorePods(key, val string) []string {
	list := &corev1.PodList{}
	if err := k8sClient.List(context.Background(), list,
		client.InNamespace(operatorNamespace),
		client.MatchingLabels{key: val}); err != nil {
		return nil
	}

	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Status.Phase == corev1.PodRunning {
			names = append(names, list.Items[i].GetName())
		}
	}

	return names
}
