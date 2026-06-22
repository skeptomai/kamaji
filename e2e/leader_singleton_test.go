// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// Failure Mode 5: a single, stable leader under concurrent startup.
//
// Scaling several replicas up at once must not produce split-brain. The Lease
// is the source of truth: controller-runtime only reconciles while holding it,
// so a healthy election yields exactly one holder that stays put — the leader
// renews, the standbys wait. A flapping holderIdentity would signal contention
// or repeated lease loss.
var _ = Describe("Leader election yields a single stable leader under concurrent startup", func() {
	BeforeEach(func() {
		scaleOperator(3)
	})

	JustAfterEach(func() {
		scaleOperator(1)
	})

	It("elects exactly one leader and holds it steady", func() {
		By("a leader is elected")
		var leader string
		Eventually(func() string {
			leader = leaderPodName()

			return leader
		}, 60*time.Second, time.Second).ShouldNot(BeEmpty())

		By("the leader is one of the running replicas")
		Expect(controllerPods()).To(ContainElement(leader))

		By("leadership does not flap (no split-brain churn)")
		Consistently(leaderPodName, 30*time.Second, 2*time.Second).Should(Equal(leader))
	})
})
