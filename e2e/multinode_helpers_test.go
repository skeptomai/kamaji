//go:build multinode

// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// These helpers back the multi-node HA tests (FM9–FM11). They are behind the
// `multinode` build tag; the specs carry Label("multinode"). Run on a real
// cluster of >= 2 nodes with `make e2e-multinode`.

// --- node / placement helpers ---

// operatorPodNodes maps each controller pod name to the node it runs on.
func operatorPodNodes() map[string]string {
	list := &corev1.PodList{}
	Expect(k8sClient.List(context.Background(), list,
		client.InNamespace(operatorNamespace),
		client.MatchingLabels{"app.kubernetes.io/component": "controller-manager"})).To(Succeed())

	m := map[string]string{}
	for i := range list.Items {
		m[list.Items[i].GetName()] = list.Items[i].Spec.NodeName
	}

	return m
}

// nodeOfPod returns the node hosting a controller pod, or "" if not found.
func nodeOfPod(pod string) string {
	p := &corev1.Pod{}
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Name: pod, Namespace: operatorNamespace}, p); err != nil {
		return ""
	}

	return p.Spec.NodeName
}

// readyNodeCount returns the number of nodes reporting Ready=True.
func readyNodeCount() int {
	list := &corev1.NodeList{}
	if err := k8sClient.List(context.Background(), list); err != nil {
		return 0
	}

	n := 0
	for i := range list.Items {
		for _, c := range list.Items[i].Status.Conditions {
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				n++
			}
		}
	}

	return n
}

// nodeIsReady reports whether a node currently has Ready=True.
func nodeIsReady(node string) bool {
	n := &corev1.Node{}
	if err := k8sClient.Get(context.Background(), types.NamespacedName{Name: node}, n); err != nil {
		return false
	}

	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}

	return false
}

// --- external fault injection (out-of-band hardware control) ---
//
// The test process cannot flip a PDU or a switch port itself, so real faults are
// delegated to operator-supplied commands. The lab's specific IPMI/PDU/switch
// details stay external and are configured by env vars; "{{node}}" in each
// command is replaced with the target node name and the command runs via `sh`:
//
//	KAMAJI_E2E_POWER_OFF  / KAMAJI_E2E_POWER_ON     — total node power control
//	KAMAJI_E2E_NET_CUT    / KAMAJI_E2E_NET_RESTORE  — node network isolation
//
// e.g. KAMAJI_E2E_POWER_OFF='ipmitool -H {{node}}-bmc chassis power off'

// requireFaultHooks Skips the spec unless every named env hook is configured.
// Call it before injecting, so a spec never aborts half-way through a fault.
func requireFaultHooks(keys ...string) {
	for _, k := range keys {
		if os.Getenv(k) == "" {
			Skip("multi-node fault hooks not configured; set " + strings.Join(keys, ", ") +
				" to the lab commands (use {{node}} for the node name)")
		}
	}
}

// runFault executes the command in the given env var with {{node}} substituted.
func runFault(envKey, node string) {
	cmd := strings.ReplaceAll(os.Getenv(envKey), "{{node}}", node)
	out, err := exec.CommandContext(context.Background(), "sh", "-c", cmd).CombinedOutput()
	Expect(err).NotTo(HaveOccurred(), "fault command %q failed: %s", cmd, string(out))
}

func powerOff(node string)       { runFault("KAMAJI_E2E_POWER_OFF", node) }
func powerOn(node string)        { runFault("KAMAJI_E2E_POWER_ON", node) }
func cutNetwork(node string)     { runFault("KAMAJI_E2E_NET_CUT", node) }
func restoreNetwork(node string) { runFault("KAMAJI_E2E_NET_RESTORE", node) }
