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
- `charts/kamaji/values.yaml` — a `podDisruptionBudget` block and HA guidance on
  `replicaCount`.

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

Shared helpers (`leaderPodName`, `scaleOperator`, `controllerPods`) live in
`operator_failover_test.go`.

**Harness** (`Makefile`)

- `make chaos-mesh` — install chaos-mesh into the current cluster.
- `make e2e-chaos` — run the chaos lane (`ginkgo --tags=chaos --label-filter=chaos`).

## How to run

```sh
# Deterministic lane (FM1–FM6): gating-safe
make e2e                       # spins up KinD, installs the stack, runs ./e2e

# Chaos lane (FM7–FM8): non-gating, needs fault injection
make e2e-chaos                 # = make e2e + chaos-mesh, then the chaos-tagged specs

# Typecheck without a cluster
go vet ./e2e/                  # deterministic files
go vet -tags chaos ./e2e/      # + chaos files
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

The existing `make e2e` target builds the reference cluster; the HA tests run on
top of it. Anatomy:

- **Base:** a single-node **KinD** cluster (`kind create cluster --name kamaji`)
  on Docker. The control-plane node is a container on the `kind` Docker network
  (default `172.18.0.0/16`). The kube-apiserver runs as a **kubeadm static pod**
  in `kube-system` (what FM7 targets), and the runtime is **containerd**
  (`/run/containerd/containerd.sock`, what chaos-mesh's daemon needs).
- **LoadBalancer:** **MetalLB** in L2 mode, with an address pool carved from the
  `kind` Docker subnet (`hack/metallb.yaml`). Tenant control planes get their
  `ControlPlaneEndpoint` from this pool — which is why the tests hardcode
  addresses like `172.18.0.2`–`172.18.0.7`. **Each TenantControlPlane needs a
  distinct, free address from that pool**; the HA tests reserve `.4`–`.7`.
- **Supporting stack:** cert-manager (webhook + datastore certs), Gateway API
  CRDs + Envoy Gateway (Gateway/Konnectivity tests), Kamaji CRDs, and the Kamaji
  operator (`replicaCount: 1` by default; image side-loaded with
  `pullPolicy: Never`; telemetry disabled).
- **Datastore:** etcd via the `clastix/kamaji-etcd` chart (release `etcd-primary`
  in `kamaji-system`), a multi-member (quorum) StatefulSet — this is what FM8
  exercises.
- **Chaos (FM7/FM8 only):** chaos-mesh, installed with
  `chaosDaemon.runtime=containerd` and the containerd socket path.

### What the cluster must support for the HA tests

- **Scheduling 2–3 operator replicas.** The failover tests scale the operator
  Deployment up and restore it to 1 afterward. A single node is sufficient —
  multiple operator pods co-schedule and leader election still works across them.
- **A multi-member datastore** for FM8 (the kamaji-etcd quorum provides it).
- **chaos-mesh** for the chaos lane.

### Single-node is enough to start; multi-node is closer to production

The deterministic lane (FM1–FM6) runs fully on the default single-node KinD
cluster — including FM4, which uses the **Eviction API** (what `kubectl drain`
calls) rather than a real node drain precisely so it works on one node without
evicting the test harness.

A **multi-node** KinD cluster (1 control-plane + 2–3 workers) is worth using to
exercise what one node cannot:

- a **real node drain** (cordon + `kubectl drain`) against the PDB, not just the
  Eviction API;
- **pod anti-affinity** — a production HA operator should spread its replicas
  across nodes. The chart exposes `affinity` but ships **no default
  anti-affinity**; adding a `topologyKey: kubernetes.io/hostname` rule (and
  testing it lands replicas on distinct nodes) is a recommended follow-up that
  *requires* multiple nodes to validate;
- **cross-node leader failover** (kill the node hosting the leader, not just the
  pod).

### Rough sizing

Each TenantControlPlane runs an apiserver + controller-manager + scheduler (plus
its etcd), so running several TCPs alongside an HA operator and chaos-mesh wants
a Docker host with headroom — on the order of **6–8 vCPU and 12–16 GB RAM** as a
working guideline, more if you raise replica counts or run many TCPs at once.

### Managed-cluster caveat

For a hosted control plane on a **managed** Kubernetes (EKS/GKE/AKS), the
kube-apiserver is not a pod you can see or target, so **FM7 does not apply** —
its split-brain guarantee would need a provider-specific fault-injection
approach. FM1–FM6 and FM8 are portable to any conformant cluster that satisfies
the prerequisites above.
