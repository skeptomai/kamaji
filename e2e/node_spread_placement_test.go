//go:build multinode

// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Failure Mode 9 (MULTI-NODE): controller replicas are spread across nodes.
//
// Two replicas with no node spread can co-locate, so a single node loss takes
// out both — and the Fail-policy webhooks with them. This validates that the
// chart's anti-affinity / topology spread actually lands the replicas on
// distinct nodes. Run against an HA install (-f charts/kamaji/values-ha.yaml)
// for the hard guarantee; the default soft anti-affinity should also pass on a
// lightly-loaded cluster.
var _ = Describe("Operator replicas are spread across nodes", Label("multinode"), func() {
	BeforeEach(func() {
		if readyNodeCount() < 2 {
			Skip("FM9 needs >= 2 Ready nodes")
		}
		scaleOperator(2)
	})

	// Leave the operator at 2 replicas: the multi-node lane runs it HA.

	It("places the two controller replicas on distinct nodes", func() {
		Eventually(func(g Gomega) {
			pods := operatorPodNodes()
			g.Expect(pods).To(HaveLen(2))

			nodes := map[string]bool{}
			for pod, node := range pods {
				g.Expect(node).NotTo(BeEmpty(), "pod %s has no node assigned", pod)
				nodes[node] = true
			}
			g.Expect(nodes).To(HaveLen(2),
				"the two replicas must land on distinct nodes (anti-affinity / topology spread)")
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
})
