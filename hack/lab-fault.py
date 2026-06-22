#!/usr/bin/env python3
# Copyright 2022 Clastix Labs
# SPDX-License-Identifier: Apache-2.0
"""
Example fault-injection CLI for the multi-node HA tests (FM10/FM11).

The e2e tests do not power nodes off or cut networks themselves — they shell out
to operator-supplied commands so the lab's specific hardware (smart outlets,
IPMI/BMC, managed switch) stays external to the test suite. Point the env hooks
at this script (or your own equivalent) and `make e2e-multinode`:

    export KAMAJI_E2E_POWER_OFF='python3 hack/lab-fault.py off     {{node}}'
    export KAMAJI_E2E_POWER_ON='python3 hack/lab-fault.py on      {{node}}'
    export KAMAJI_E2E_NET_CUT='python3 hack/lab-fault.py cut     {{node}}'
    export KAMAJI_E2E_NET_RESTORE='python3 hack/lab-fault.py restore {{node}}'

The test framework replaces "{{node}}" with the target Kubernetes node name.

Contract (what the tests rely on):
  - argv: <action> <node>, action in {off, on, cut, restore}
  - exit 0  => the fault was applied / cleared successfully
  - exit !0 => failure; stdout/stderr is surfaced in the test output
  - `on` and `restore` must be idempotent (cleanup may call them more than once)

By default this stub FAILS (exit 1) until you wire in your outlet/switch library
below — that is deliberate, so FM10/FM11 cannot pass green without a real fault
actually being injected. Use --dry-run (or LAB_FAULT_DRYRUN=1) to exercise the
test plumbing without touching hardware; it logs the intended action and exits 0
with a loud warning.

Fill in:
  1. NODE_TARGETS — map each Kubernetes node name to its outlet/BMC/switch-port.
  2. the four action functions — call your smart-outlet / IPMI / switch library.
"""

import argparse
import os
import sys

# 1) Map Kubernetes node names to whatever your hardware library addresses.
#    e.g. {"k3s-node-1": {"outlet": "pdu-a:3", "switch_port": "sw1:Gi0/5"}, ...}
NODE_TARGETS: dict[str, dict[str, str]] = {
    # "k3s-node-1": {"outlet": "...", "switch_port": "..."},
}


def _target(node: str) -> dict[str, str]:
    target = NODE_TARGETS.get(node)
    if target is None:
        fail(f"no hardware mapping for node {node!r}; add it to NODE_TARGETS")
    return target


def fail(msg: str) -> "NoReturn":  # type: ignore[name-defined]
    print(f"lab-fault: ERROR: {msg}", file=sys.stderr)
    sys.exit(1)


def power_off(node: str) -> None:
    target = _target(node)
    # TODO: call your smart-outlet library to cut power, e.g.:
    #   from myoutlets import Outlet; Outlet(target["outlet"]).off()
    fail(f"power_off not implemented for {node} (outlet={target.get('outlet')!r})")


def power_on(node: str) -> None:
    target = _target(node)
    # TODO: restore power (idempotent), e.g. Outlet(target["outlet"]).on()
    fail(f"power_on not implemented for {node} (outlet={target.get('outlet')!r})")


def net_cut(node: str) -> None:
    target = _target(node)
    # TODO: disable the node's switch port, e.g. Switch().shutdown(target["switch_port"])
    fail(f"net_cut not implemented for {node} (port={target.get('switch_port')!r})")


def net_restore(node: str) -> None:
    target = _target(node)
    # TODO: re-enable the switch port (idempotent), e.g. Switch().no_shutdown(...)
    fail(f"net_restore not implemented for {node} (port={target.get('switch_port')!r})")


ACTIONS = {"off": power_off, "on": power_on, "cut": net_cut, "restore": net_restore}


def main() -> None:
    parser = argparse.ArgumentParser(description="Lab fault injection for Kamaji multi-node HA tests")
    parser.add_argument("action", choices=sorted(ACTIONS))
    parser.add_argument("node", help="target Kubernetes node name")
    parser.add_argument(
        "--dry-run",
        action="store_true",
        default=os.environ.get("LAB_FAULT_DRYRUN") == "1",
        help="log the intended action and exit 0 without touching hardware",
    )
    args = parser.parse_args()

    if args.dry_run:
        print(f"lab-fault: DRY-RUN: would '{args.action}' node {args.node!r} "
              f"(NO real fault injected)", file=sys.stderr)
        sys.exit(0)

    ACTIONS[args.action](args.node)


if __name__ == "__main__":
    main()
