// Copyright 2022 Clastix Labs
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"fmt"
	"os"
	"strings"
)

// Environment knobs for running the HA tests (FM1–FM11) against a cluster other
// than the single-node KinD harness. Every value defaults to the KinD harness,
// so the existing `make e2e` lane needs no configuration; a real cluster (e.g.
// bare-metal k3s) sets a handful of KAMAJI_E2E_* vars.

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return def
}

// operatorNamespace is the namespace the Kamaji controller runs in.
// Override with KAMAJI_E2E_OPERATOR_NS.
var operatorNamespace = envOr("KAMAJI_E2E_OPERATOR_NS", "kamaji-system")

// controlPlaneAddress returns a TenantControlPlane endpoint address from the
// configured LoadBalancer pool: KAMAJI_E2E_ADDR_PREFIX sets the prefix (default
// the KinD MetalLB pool), offset is the final octet. The HA tests use offsets
// 3–8; keep them distinct and free in the pool.
func controlPlaneAddress(offset int) string {
	return fmt.Sprintf("%s.%d", envOr("KAMAJI_E2E_ADDR_PREFIX", "172.18.0"), offset)
}

// datastoreSelector returns the label key/value identifying datastore members
// for FM8. Override with KAMAJI_E2E_DATASTORE_SELECTOR="key=value". The default
// matches the clastix/kamaji-etcd release "etcd-primary" that the e2e Makefile
// installs in kamaji-system.
func datastoreSelector() (string, string) {
	sel := envOr("KAMAJI_E2E_DATASTORE_SELECTOR", "app.kubernetes.io/instance=etcd-primary")
	if i := strings.IndexByte(sel, '='); i >= 0 {
		return sel[:i], sel[i+1:]
	}

	return sel, ""
}

// apiserverExternalAddr returns the API endpoint that FM7 partitions the leader
// from. Empty (the default) => FM7 targets the kube-apiserver static pods
// (KinD/kubeadm). Set KAMAJI_E2E_APISERVER_ADDR to a server node IP or the
// kubernetes Service ClusterIP on clusters where the apiserver is not a static
// pod (e.g. k3s).
func apiserverExternalAddr() string {
	return os.Getenv("KAMAJI_E2E_APISERVER_ADDR")
}
