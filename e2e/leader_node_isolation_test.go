//go:build multinode

// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Failure Mode 11 (MULTI-NODE, HARDWARE FAULT): the leader's node is isolated.
//
// Cuts the network — for real, via the KAMAJI_E2E_NET_CUT hook — to the node
// hosting the lease holder. This is the dangerous split-brain shape: the leader
// process keeps running but can no longer renew its lease, so a standby on a
// surviving node must take over while the isolated manager self-terminates
// rather than continuing to act. Leadership moving to a *different node* (the
// lease has exactly one holder) is the proof there is no second active leader.
//
// A power-off variant (KAMAJI_E2E_POWER_OFF on the leader's node) is the clean-
// death case and is covered structurally by Failure Mode 10; network isolation
// is the harder test because the isolated process is still alive.
var _ = Describe("Leadership moves to a surviving node when the leader's node is isolated", Label("multinode"), func() {
	var isolated string

	BeforeEach(func() {
		requireFaultHooks("KAMAJI_E2E_NET_CUT", "KAMAJI_E2E_NET_RESTORE")
		if readyNodeCount() < 2 {
			Skip("FM11 needs >= 2 Ready nodes")
		}

		scaleOperator(2)
		Eventually(func(g Gomega) {
			nodes := map[string]bool{}
			for _, n := range operatorPodNodes() {
				nodes[n] = true
			}
			g.Expect(nodes).To(HaveLen(2))
		}, 2*time.Minute, 2*time.Second).Should(Succeed())
	})

	AfterEach(func() {
		// Always restore connectivity, even on failure.
		if isolated != "" {
			restoreNetwork(isolated)
			Eventually(func() bool { return nodeIsReady(isolated) }, 5*time.Minute, 5*time.Second).
				Should(BeTrue(), "node %s should rejoin after network restore", isolated)
			isolated = ""
		}
	})

	It("elects a new leader on a different node and the isolated leader does not keep acting", func() {
		leader := leaderPodName()
		Expect(leader).NotTo(BeEmpty())
		leaderNode := nodeOfPod(leader)
		Expect(leaderNode).NotTo(BeEmpty())

		By("isolating the leader's node from the network: " + leaderNode)
		cutNetwork(leaderNode)
		isolated = leaderNode

		By("the isolated node goes NotReady")
		Eventually(func() bool { return nodeIsReady(leaderNode) }, 3*time.Minute, 5*time.Second).
			Should(BeFalse())

		By("a different pod acquires the lease (the isolated leader loses it)")
		Eventually(leaderPodName, 3*time.Minute, 2*time.Second).
			ShouldNot(Or(BeEmpty(), Equal(leader)))

		By("the new leader runs on a surviving node, not the isolated one")
		Eventually(func() string {
			return nodeOfPod(leaderPodName())
		}, 1*time.Minute, 2*time.Second).ShouldNot(Or(BeEmpty(), Equal(leaderNode)))

		By("restoring the network")
		restoreNetwork(leaderNode)
		isolated = "" // handled here; AfterEach must not restore again
		Eventually(func() bool { return nodeIsReady(leaderNode) }, 5*time.Minute, 5*time.Second).
			Should(BeTrue())
	})
})
