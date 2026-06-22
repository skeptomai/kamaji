# Management-Plane HA: Failure Modes and Test Plan

This document enumerates the failure modes for running the **Kamaji operator**
(the management plane) in a highly-available topology, and the tests that prove
each one is handled. It is concerned with the availability of the *operator
itself*, not the tenant control planes it manages — those have a separate
availability story rooted in the datastore (see Failure Mode 7).

## Background: what leader election does and does not cover

Kamaji's manager always runs with `--leader-elect` enabled
(`charts/kamaji/templates/controller.yaml`), backed by a
`coordination.k8s.io/v1` Lease named `kamaji.clastix.io` in the operator
namespace. Two consequences shape everything below:

1. **Controllers are active/passive.** Only the lease holder reconciles. When
   the holder dies, a standby acquires the lease and resumes work after roughly
   the lease duration.
2. **Webhook serving is *not* gated by leader election.** Every replica runs its
   own webhook server. The admission webhooks are reached through a Service, so
   webhook availability is governed by Service endpoints and pod readiness, not
   by who holds the lease.

This second point is the crux of the HA story, because the operator ships
`Fail`-policy admission webhooks:

| Replicas | Effect of losing the active pod |
| --- | --- |
| **1** | Hard outage: no webhook endpoint, so `Fail`-policy webhooks reject `TenantControlPlane` create/update until the pod is rescheduled. |
| **2+** | Webhooks keep being served by the survivor; only *controller work* pauses for the lease-transfer window (~lease duration). |

The default chart ships `replicaCount: 1`. Running `replicaCount: 2+` (plus the
`PodDisruptionBudget` introduced alongside this document) is what moves the
operator from "single point of failure" to "active/passive HA".

## Test tiers

- **Deterministic (CI-gating):** standard Ginkgo e2e against an existing cluster,
  same harness as the rest of `e2e/`. Safe to gate the merge queue.
- **Chaos (non-gating):** requires fault injection (network partition, latency,
  process kill under load). Inherently flaky; runs in a separate, non-blocking
  lane.

## Verification status and prerequisites

These caveats apply to all the implemented tests below:

- **Compile-checked, not yet run.** The implemented tests pass `go vet ./e2e/`
  but have not been executed against a live cluster in this branch. They use the
  existing e2e harness (`envtest` with `UseExistingCluster: true`), so they
  require a real management cluster with the Kamaji operator deployed in the
  `kamaji-system` namespace and a working datastore — the same prerequisites as
  the rest of `e2e/`.
- **They scale the operator.** Each failover test scales the controller
  Deployment to 2 replicas in `BeforeEach` and restores `replicaCount: 1` in
  `JustAfterEach`, because the suite's `utils_test.go` asserts a single operator
  pod. Running them concurrently with other specs against the same cluster is
  unsafe; they assume serial execution.
- **Leader identification is implementation-coupled.** The helpers read the
  `coordination.k8s.io/v1` Lease named `kamaji.clastix.io` and parse the leader
  pod from `holderIdentity` (`<podName>_<uuid>`, controller-runtime's format).
  If the `LeaderElectionID` or the holder-identity encoding changes upstream,
  `leaderPodName()` in `e2e/operator_failover_test.go` must be updated.

## Failure modes

### 1. Leader pod death → re-election and resumed reconciliation  *(deterministic)*

**Risk:** the active manager dies and reconciliation never resumes.

**Test:** `e2e/operator_failover_test.go`. Scale to 2, identify the leader from
the Lease `holderIdentity`, delete the leader pod, assert a *different* pod
acquires the lease, then create a `TenantControlPlane` **after** the kill and
assert it reaches `Ready`. The post-kill TCP is the load-bearing assertion: it
proves the new leader is doing work, not merely holding a lease.

**Status:** implemented.

### 2. Webhook availability during failover  *(deterministic)*

**Risk:** the `Fail`-policy webhooks reject tenant operations during the
failover window because the Service still routes to the dead pod.

**Test:** with 2 replicas, delete the leader and *immediately* attempt a
`TenantControlPlane` create/update; assert it is **admitted** (not rejected by a
`Fail` webhook). This validates that readiness probes evict the dying pod from
Service endpoints quickly enough and that the surviving pod serves admission.

**Design note:** the test asserts admission *recovers promptly* (`Eventually`,
30s) and then *stays available* (`Consistently`, 15s), rather than demanding
zero failed calls from the instant of the kill. This is deliberate: with a
single replica the `Eventually` would fail for the entire pod-reschedule
duration, so the contrast that proves HA still holds. If endpoint-removal lag
for the terminating pod produces a transient blip inside the window, the test
surfaces it as a failure — and that is *actionable signal*, not noise: it points
to a missing `preStop` hook or too-short `terminationGracePeriodSeconds` on the
controller, which should be fixed rather than tolerated. The probe is an
annotation `Patch` (monotonically increasing value) so every poll is a genuine
`UPDATE` through the `Fail`-policy mutating webhook, not a no-op the API server
might skip.

**Status:** implemented (`e2e/webhook_failover_availability_test.go`).

### 3. Mid-flight reconcile interruption  *(deterministic)*

**Risk:** the leader dies *during* a multi-step operation (certificate rotation,
Kubernetes version upgrade), leaving a half-applied state.

**Test:** trigger a version upgrade (or cert rotation) on a `TenantControlPlane`,
kill the leader while the operation is in flight, and assert the TCP still
converges to `Ready` and the operation completes. This exercises reconciler
idempotency/resumability — the property most at risk when controller logic is
validated only end-to-end.

**Design note:** the load-bearing assertion is the *running version* after
convergence (`status.kubernetesResources.version.version == toVersion`), not
just `Ready` — a leader could report `Ready` having reverted or stalled the
upgrade. The kill is issued immediately after the version `Patch` so it races
the in-flight reconcile; the test asserts convergence regardless of exactly
where the interruption lands.

**Caveat — upgrade target is an assumption:** the test upgrades `v1.23.6 ->
v1.24.0`, a single linear minor bump (non-linear jumps are rejected by the
version webhook, see `e2e/tcp_validation_version_nonlinear_test.go`). If the
e2e environment supports a different version range, adjust the `fromVersion` /
`toVersion` constants in `e2e/operator_failover_upgrade_test.go` accordingly.

**Status:** implemented (`e2e/operator_failover_upgrade_test.go`).

### 4. PodDisruptionBudget enforcement under node drain  *(deterministic)*

**Risk:** a node drain takes down all operator replicas at once; or, conversely,
a PDB on a single replica wedges drains entirely.

**Test:** with the operator scaled to 2 and a PDB (`maxUnavailable: 1`) in
place, use the **Eviction API** — the same mechanism `kubectl drain` drives — to
take down replicas: the first eviction succeeds, the budget then reports
`disruptionsAllowed: 0`, and the second eviction is refused with HTTP 429. This
proves a node drain can never remove both replicas at once.

**Design note:** the test drives the Eviction API directly rather than cordoning
and draining a real node — a node drain in the single-node KinD e2e cluster
would be destructive and could evict the test harness itself. Eviction is
exactly what `kubectl drain` calls per-pod, so it validates the same guarantee
without the blast radius. The second eviction is attempted only *after*
`disruptionsAllowed` is observed at 0, so the 429 is deterministic rather than
racing the PDB controller.

**Caveat — enforcement, not gating:** the e2e environment installs the chart at
the default `replicaCount: 1`, so the chart's PDB is intentionally absent there;
the test therefore applies a PDB mirroring the chart's render at
`replicaCount > 1`. It validates *enforcement behavior*, not the chart's
*gating* template logic (`replicaCount: 1` -> no PDB). Gating is a templating
property and should be covered by a `helm template` assertion in a chart unit
test (not yet added — recommended follow-up).

**Status:** implemented (`e2e/operator_pdb_test.go`). PDB template added in
`charts/kamaji/templates/pdb.yaml`.

### 5. Lease singleton under concurrent startup  *(deterministic)*

**Risk:** scaling up many replicas at once produces split-brain (two leaders).

**Test:** scale to 3 replicas, then assert a leader is elected, that the holder
is one of the running controller pods, and that the `holderIdentity` stays put
for a sustained interval (`Consistently`, 30s).

**Design note:** the stability check is the real assertion. The Lease can only
name one holder at a time, so "two leaders" cannot be observed as two
`holderIdentity` values — it would instead surface as the holder *flapping*
between pods as managers fight over the lease. A `Consistently` that the holder
does not change is therefore the signal that election converged cleanly with no
contention churn. controller-runtime only runs reconcilers while holding the
lease, so a stable holder also means a single active control loop.

**Status:** implemented (`e2e/leader_singleton_test.go`).

### 6. Operator rolling upgrade  *(deterministic)*

**Risk:** upgrading the operator image causes a reconciliation gap or an
unclean lease handoff.

**Test:** with 2 replicas + PDB, change the operator image and assert no
reconcile gap (a TCP mutated during the rollout still converges) and a clean
lease transfer. Exercises PDB + leader election + readiness composing together.

**Status:** to implement.

### 7. Network partition / split-brain of the active leader  *(chaos)*

**Risk:** the active leader is partitioned from the API server but keeps
running; two managers act simultaneously.

**Test:** network-isolate the leader from the API server (NetworkPolicy or
chaos-mesh), assert it self-terminates on the renew deadline, and assert the
standby takes over with no overlapping writes. Inherently timing-sensitive;
belongs in the chaos lane.

**Status:** to implement (chaos lane).

### 8. Datastore failure  *(chaos / semi-deterministic)*

**Risk:** this is the bigger availability story. Tenant control-plane
availability depends on the datastore (`etcd`, or `kine` over MySQL / PostgreSQL
/ NATS), not on the operator. A datastore failure is the more probable
production incident at scale.

**Test:** kill an `etcd` member or the `kine` backend primary; assert tenant
control planes keep serving and that the operator reconnects and reconciles once
the datastore recovers. Use toxiproxy for latency/partition injection on the
datastore connection.

**Status:** to implement (chaos lane).

## Summary

| # | Failure mode | Tier | Status |
| --- | --- | --- | --- |
| 1 | Leader pod death → re-election | Deterministic | Implemented |
| 2 | Webhook availability during failover | Deterministic | Implemented |
| 3 | Mid-flight reconcile interruption | Deterministic | Implemented |
| 4 | PDB enforcement under drain | Deterministic | Implemented |
| 5 | Lease singleton under concurrent startup | Deterministic | Implemented |
| 6 | Operator rolling upgrade | Deterministic | To implement |
| 7 | Network partition / split-brain | Chaos | To implement |
| 8 | Datastore failure | Chaos | To implement |
