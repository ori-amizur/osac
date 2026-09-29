"""Guard CaaS deletion ordering while Agents and Machines unbind."""

from pathlib import Path

import pytest
import yaml
from jinja2 import Environment, StrictUndefined

DELETE_TASKS = (
    Path(__file__).resolve().parents[2]
    / "collections/ansible_collections/osac/service/roles/hosted_cluster/tasks/delete_hosted_cluster.yaml"
)


def test_worker_teardown_precedes_hosted_cluster_removal() -> None:
    tasks = yaml.safe_load(DELETE_TASKS.read_text())
    by_name = {task["name"]: (index, task) for index, task in enumerate(tasks)}
    names = (
        "Delete NodePool resources",
        "Wait for NodePool resources to finish deleting",
        "Wait for baremetalworker to finish deleting workers",
        "Delete HostedCluster resource",
        "Wait for HostedCluster to finish deleting",
    )
    assert [by_name[name][0] for name in names] == sorted(by_name[name][0] for name in names)

    nodepool_wait = by_name[names[1]][1]
    assert nodepool_wait["register"] == "remaining_nodepools"
    assert "remaining_nodepools.resources | length == 0" in nodepool_wait["until"]
    assert nodepool_wait["retries"] >= 180

    worker_wait = by_name[names[2]][1]
    assert worker_wait["register"] == "worker_clusterorder"
    assert "baremetalworker-finalizer" in worker_wait["until"]
    assert worker_wait["retries"] >= 180

    # Guest credentials and cluster registration must survive until Machine
    # drain/unbinding is complete.
    for name in (
        "Delete Secret resource containing pull-secret",
        "Delete Secret resource containing ssh-key",
        "Delete ManagedCluster resource",
    ):
        assert by_name[name][0] > by_name[names[2]][0]


@pytest.mark.parametrize(
    ("resources", "should_proceed"),
    [
        ([], True),
        ([{"metadata": {"finalizers": ["osac.openshift.io/baremetalworker-finalizer"]}}], False),
        ([{"metadata": {"finalizers": ["osac.openshift.io/finalizer"]}}], True),
    ],
)
def test_hosted_cluster_removal_waits_only_for_worker_finalizer(
    resources: list[dict[str, dict[str, list[str]]]], should_proceed: bool
) -> None:
    tasks = yaml.safe_load(DELETE_TASKS.read_text())
    wait = next(task for task in tasks if task["name"] == "Wait for baremetalworker to finish deleting workers")
    condition = Environment(undefined=StrictUndefined).compile_expression(wait["until"])
    assert condition(worker_clusterorder={"resources": resources}) is should_proceed
