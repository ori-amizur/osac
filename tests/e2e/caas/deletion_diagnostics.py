"""Temporary read-only snapshots for diagnosing CaaS deletion.

Only emit allowlisted metadata, never kubectl output, conditions, annotations or
exception text: CI logs are uploaded as artifacts.
"""

from __future__ import annotations

import json
import subprocess
from typing import Any

from tests.e2e.core.k8s_client import K8sClient
from tests.e2e.core.runner import run_unchecked

_HYPERSHIFT_FINALIZER = "hypershift.openshift.io/finalizer"
_AGENTCLUSTER_FINALIZER = "agentclustercapi-provider.agent-install.openshift.io/deprovision"
_MACHINE_HOOK = "pre-terminate.delete.hook.machine.cluster.x-k8s.io/agentmachine"


def _read(k8s: K8sClient, resource: str, namespace: str, name: str | None = None) -> dict[str, Any] | None:
    args = [*k8s._base(), "get", resource]
    if name is not None:
        args.append(name)
    if resource != "namespaces":
        args.extend(["-n", namespace])
    args.extend(["-o", "json"])
    try:
        output, rc = run_unchecked(*args, timeout=3)
        if rc != 0:
            return None
        result = json.loads(output)
        return result if isinstance(result, dict) else None
    except (subprocess.TimeoutExpired, ValueError):
        return None


def _items(k8s: K8sClient, resource: str, namespace: str) -> list[dict[str, Any]] | None:
    response = _read(k8s, resource, namespace)
    if response is None or not isinstance(response.get("items"), list):
        return None
    return response["items"]


def _terminating(item: dict[str, Any]) -> bool:
    return bool(item.get("metadata", {}).get("deletionTimestamp"))


def snapshot_cluster_deletion(k8s: K8sClient, name: str) -> list[str]:
    """Return bounded, allowlisted facts about this ClusterOrder's deletion chain."""
    ns = k8s.namespace
    hc_ns = f"{ns}-{name}"
    cp_ns = f"{hc_ns}-{name}"
    lines = []
    for label, resource, namespace, object_name, known_finalizer in (
        ("clusterorder", "clusterorders", ns, name, "osac.openshift.io/finalizer"),
        ("infraenv", "infraenvs.agent-install.openshift.io", ns, f"{name}-infraenv", None),
        ("hostedcluster", "hostedclusters.hypershift.openshift.io", hc_ns, name, _HYPERSHIFT_FINALIZER),
    ):
        items = _items(k8s, resource, namespace)
        if items is None:
            lines.append(f"{label} unavailable")
            continue
        item = next((item for item in items if item.get("metadata", {}).get("name") == object_name), None)
        if item is None:
            lines.append(f"{label} present=False")
            continue
        metadata = item.get("metadata", {})
        line = (
            f"{label} present=True terminating={_terminating(item)} finalizers={len(metadata.get('finalizers') or [])}"
        )
        if known_finalizer:
            finalizer_label = "hypershift_finalizer" if label == "hostedcluster" else "osac_finalizer"
            has_finalizer = known_finalizer in (metadata.get("finalizers") or [])
            line += f" {finalizer_label}={has_finalizer}"
        lines.append(line)

    pools = _items(k8s, "nodepools.hypershift.openshift.io", hc_ns)
    if pools is None:
        lines.append("nodepool unavailable")
    else:
        owned = [p for p in pools if p.get("metadata", {}).get("name", "").startswith(f"nodepool-{name}-")]
        lines.append(f"nodepool count={len(owned)} terminating={sum(_terminating(p) for p in owned)}")

    for label, resource in (
        ("hostedcontrolplane", "hostedcontrolplanes.hypershift.openshift.io"),
        ("capi-cluster", "clusters.cluster.x-k8s.io"),
        ("machineset", "machinesets.cluster.x-k8s.io"),
    ):
        resources = _items(k8s, resource, cp_ns)
        if resources is None:
            lines.append(f"{label} unavailable")
        else:
            lines.append(
                f"{label} count={len(resources)} terminating={sum(_terminating(item) for item in resources)} "
                f"finalizers={sum(len(item.get('metadata', {}).get('finalizers') or []) for item in resources)}"
            )

    machines = _items(k8s, "machines.cluster.x-k8s.io", cp_ns)
    if machines is None:
        lines.append("machine unavailable")
    else:
        lines.append(
            f"machine count={len(machines)} terminating={sum(_terminating(m) for m in machines)} "
            f"preterminate_hook={sum(_MACHINE_HOOK in m.get('metadata', {}).get('annotations', {}) for m in machines)}"
        )

    agents = _items(k8s, "agentclusters.capi-provider.agent-install.openshift.io", cp_ns)
    if agents is None:
        lines.append("agentcluster unavailable")
    else:
        blocked = sum(_AGENTCLUSTER_FINALIZER in (a.get("metadata", {}).get("finalizers") or []) for a in agents)
        lines.append(f"agentcluster count={len(agents)} deprovision_finalizer={blocked}")

    for label, namespace in (("hosted", hc_ns), ("control-plane", cp_ns)):
        data = _read(k8s, "namespaces", namespace, namespace)
        if data is None:
            lines.append(f"namespace {label} unavailable")
        else:
            lines.append(
                f"namespace {label} present=True terminating={_terminating(data)} "
                f"finalizers={len(data.get('spec', {}).get('finalizers') or [])}"
            )
    return lines
