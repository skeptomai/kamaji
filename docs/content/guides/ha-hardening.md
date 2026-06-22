# Management-Plane HA Hardening

This is the index for the work that makes the **Kamaji operator** (the management
plane) safe to run highly-available, and proves it with tests. For the detailed
per-failure-mode rationale and design notes, see
[HA Failure Modes and Test Plan](ha-failure-modes.md).

## Why

The operator ships single-replica by default, with `failurePolicy: Fail`
admission webhooks and no PodDisruptionBudget. The key insight (see the failure-
modes doc) is that **`Fail` is correct and should stay** — the defect is serving
a fail-closed webhook from a *single replica*, where one pod's outage becomes a
full admission outage. Webhook serving is not leader-gated (every replica serves
it), so the fix is `replicaCount ≥ 2` plus a PDB — preserving the safety
guarantee while removing the single point of failure.

## What changed

**Chart**

- `charts/kamaji/templates/pdb.yaml` — a `PodDisruptionBudget`, rendered only
  when `replicaCount > 1` (a PDB on a single replica cannot help and would block
  node drains).
- `charts/kamaji/templates/controller.yaml` — a **default soft `podAntiAffinity`**
  (when `affinity` is unset) that spreads controller replicas across nodes
  best-effort, plus support for `topologySpreadConstraints`. Without node spread,
  two replicas can co-locate on one node and a single node loss takes out every
  replica — and the `Fail`-policy webhooks with it. Soft by default so it never
  wedges scheduling on small/single-node clusters.
- `charts/kamaji/values.yaml` — `podDisruptionBudget`, `topologySpreadConstraints`,
  and HA guidance on `replicaCount`/`affinity`.
- `charts/kamaji/values-ha.yaml` — a ready-to-use **multi-node HA overlay**:
  `replicaCount: 2`, PDB `minAvailable: 1`, and a *hard* hostname spread
  (`DoNotSchedule`) plus soft zone spread, scoped with
  `matchLabelKeys: [pod-template-hash]` so rolling updates don't stall.

**Tests** (`e2e/`)

| Mode | File | Lane |
| --- | --- | --- |
| FM1 — leader death → re-election | `operator_failover_test.go` | deterministic |
| FM2 — webhooks survive failover | `webhook_failover_availability_test.go` | deterministic |
| FM3 — upgrade completes after mid-flight kill | `operator_failover_upgrade_test.go` | deterministic |
| FM4 — PDB enforces one disruption at a time | `operator_pdb_test.go` | deterministic |
| FM5 — single stable leader on concurrent startup | `leader_singleton_test.go` | deterministic |
| FM6 — survives a rolling upgrade | `operator_rolling_upgrade_test.go` | deterministic |
| FM7 — leader self-terminates under partition | `chaos_partition_test.go` | chaos |
| FM8 — tenant CP survives datastore member loss | `chaos_datastore_test.go` | chaos |
| FM9 — replicas spread across nodes | `node_spread_placement_test.go` | multi-node |
| FM10 — total node power-off (admission survival) | `node_power_failure_test.go` | multi-node (HW fault) |
| FM11 — leader's node network-isolated | `leader_node_isolation_test.go` | multi-node (HW fault) |

Multi-node fault injection helpers (power/network hooks, node helpers) live in
`multinode_helpers_test.go`.

Shared helpers (`leaderPodName`, `scaleOperator`, `controllerPods`) live in
`operator_failover_test.go`.

**Harness** (`Makefile`)

- `make chaos-mesh` — install chaos-mesh into the current cluster.
- `make e2e-chaos` — run the chaos lane (`ginkgo --tags=chaos --label-filter=chaos`).
- `make e2e-multinode` — run the multi-node lane against an existing ≥2-node cluster.

## How to run

```sh
# Deterministic lane (FM1–FM6): gating-safe
make e2e                       # spins up KinD, installs the stack, runs ./e2e

# Chaos lane (FM7–FM8): non-gating, needs chaos-mesh
make e2e-chaos                 # = make e2e + chaos-mesh, then the chaos-tagged specs

# Multi-node lane (FM9–FM11): non-gating, needs a real >=2-node cluster
#   install Kamaji with -f charts/kamaji/values-ha.yaml; FM10/FM11 need fault hooks:
#   KAMAJI_E2E_POWER_OFF / KAMAJI_E2E_POWER_ON / KAMAJI_E2E_NET_CUT / KAMAJI_E2E_NET_RESTORE
make e2e-multinode

# Typecheck without a cluster
go vet ./e2e/                  # deterministic files
go vet -tags chaos ./e2e/      # + chaos files
go vet -tags multinode ./e2e/  # + multi-node files
```

## Verification status

Everything is **compile-checked only** (`go vet` in both lanes) and has **not**
been run against a live cluster on this branch. The two assertions most likely
to need environment tuning before a green run:

- **FM7** targets the `kube-apiserver` static pods (`kube-system`,
  `component: kube-apiserver`). This holds on kubeadm/KinD clusters but **not** on
  managed control planes (EKS/GKE/AKS) where the apiserver is hidden — FM7 only
  applies to self-hosted clusters.
- **FM8** selects datastore members by `app.kubernetes.io/instance=etcd-primary`;
  it **Skips** if it can't find a multi-member datastore, so adjust per install.

## What a test cluster looks like

There are two profiles. The single-node KinD harness tests the HA *mechanics*;
the real multi-node cluster tests the HA *guarantee* — surviving the loss of a
genuine, independent failure domain. The guarantee is the goal.

### Reference target: a real multi-node k3s cluster on bare-metal nodes

This is the cluster the HA work is built for. "Bare metal" matters: each node is
independent hardware (its own power, kernel, NIC, disk), so "lose a node" is a
real machine failure, not a container or hypervisor abstraction. Racks/rooms map
to `topology.kubernetes.io/zone` for real spread testing.

- **Control plane:** **k3s HA with embedded etcd** — an odd number (3+) of
  `server` nodes (`--cluster-init` on the first, join the rest), plus `agent`
  nodes. This is the HA *management* cluster the whole stack ultimately depends
  on (Kamaji runs every tenant control plane as pods here, so the management
  apiserver/etcd availability is the ceiling for everything).
- **Operator HA:** deploy with `-f charts/kamaji/values-ha.yaml` — `replicaCount: 2`,
  PDB, and hard hostname spread so the two replicas land on different physical
  nodes. Without that spread, the HA is nominal: both replicas could share a node
  and one machine loss takes admission down.
- **Storage:** k3s ships `local-path`, but those PVs are **node-pinned** — a
  datastore member's volume can't move if its node dies. Use **Longhorn** for
  replicated, movable volumes so etcd members can reschedule across physical
  nodes.
- **LoadBalancer:** k3s servicelb (Klipper) on the real LAN, or MetalLB (L2/BGP).
  Tenant-CP `ControlPlaneEndpoint`s come from real routable addresses — **not**
  the KinD `172.18.0.x` range, so the tests' hardcoded addresses must be
  parametrized for this cluster.

**k3s-specific deltas the tests must account for** (k3s architecture facts):

- **FM7 must retarget.** k3s runs the apiserver *inside the `k3s server` process*,
  not as a `kube-system` static pod, so the chaos-mesh `component: kube-apiserver`
  selector finds nothing. Partition the operator pod from the **API endpoint**
  instead (server node IPs `:6443` / the `kubernetes` service ClusterIP). k3s also
  ships a NetworkPolicy controller, so a plain `NetworkPolicy` partition may work
  without chaos-mesh.
- **chaos-mesh socket** on k3s is `/run/k3s/containerd/containerd.sock` (not
  `/run/containerd/...`).

### Dev/CI harness: single-node KinD (`make e2e`)

The existing `make e2e` builds a single-node **KinD** cluster on Docker: apiserver
as a kubeadm **static pod** in `kube-system` (FM7's default target), containerd at
`/run/containerd/containerd.sock`, **MetalLB** L2 with a pool from the `kind`
subnet (hence the hardcoded `172.18.0.2`–`.7` addresses; each TCP needs a distinct
one, and the HA tests reserve `.4`–`.7`), cert-manager, Gateway API + Envoy
Gateway, the Kamaji operator (`replicaCount: 1`, image side-loaded), and the
`clastix/kamaji-etcd` datastore (release `etcd-primary` in `kamaji-system`).

The deterministic lane (FM1–FM6) runs fully here — including FM4, which uses the
**Eviction API** (what `kubectl drain` calls) rather than a real node drain
precisely so it works on one node without evicting the harness. What single-node
**cannot** test, and the bare-metal cluster can:

- a **real node drain** (cordon + `kubectl drain`) against the PDB;
- **node-spread placement** — that the two replicas actually land on distinct
  nodes (the chart's default anti-affinity / `values-ha.yaml` spread is now in
  place; multi-node is where it's *validated*);
- **ungraceful node loss** — the EndpointSlice-pruning window where a dead node's
  pod lingers in the webhook Service endpoints and `Fail`-policy calls routed to
  it fail until pruned;
- **cross-node leader failover** (lose the node hosting the leader, not just the
  pod).

These are the multi-node failure modes (FM9–FM11, implemented in the `multinode`
lane) that this target exists to exercise: FM9 asserts node-spread placement,
FM10 powers a node off (admission survival + recovery), and FM11 network-isolates
the leader's node (cross-node leadership handoff, no split-brain).

### Rough sizing

Each TenantControlPlane runs an apiserver + controller-manager + scheduler (plus
its etcd). Running several TCPs alongside an HA operator and chaos-mesh wants
nodes with headroom — on the order of **6–8 vCPU and 12–16 GB RAM per node** as a
working guideline, more if you raise replica counts or run many TCPs.

### Managed-cluster caveat

On a **managed** Kubernetes (EKS/GKE/AKS) the kube-apiserver is not a pod you can
see or target, so **FM7 does not apply** as written — its split-brain guarantee
would need a provider-specific fault-injection approach (the same retarget the
k3s profile needs). FM1–FM6 and FM8 are portable to any conformant cluster that
satisfies the prerequisites above.
