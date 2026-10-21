"""The shared external-access role only publishes endpoint annotations."""

from pathlib import Path

import yaml


AAP_ROOT = Path(__file__).parents[2]
COLLECTIONS = AAP_ROOT / "collections" / "ansible_collections"
EXTERNAL_ACCESS = COLLECTIONS / "osac" / "service" / "roles" / "external_access"
OCP_CI_SMALL = COLLECTIONS / "osac" / "templates" / "roles" / "ocp_ci_small"
OCP_SMALL = COLLECTIONS / "osac" / "templates" / "roles" / "ocp_small"


def load_tasks(path):
    return yaml.safe_load(path.read_text())


def test_shared_create_publishes_caller_provided_endpoints_only():
    tasks = load_tasks(EXTERNAL_ACCESS / "tasks" / "create.yaml")

    assert len(tasks) == 1
    task = tasks[0]
    assert task["name"] == "Write VIP annotations to ClusterOrder"
    assert "kubernetes.core.k8s" in task
    assert "external_access_api_internal_ip" in str(task)
    assert "external_access_ingress_ip" in str(task)
    assert "include_role" not in str(task)
    assert "k8s_info" not in str(task)


def test_shared_delete_clears_annotations_without_managing_dns():
    tasks = load_tasks(EXTERNAL_ACCESS / "tasks" / "delete.yaml")

    assert len(tasks) == 1
    assert "Clear VIP annotations from ClusterOrder" in tasks[0]["name"]
    assert "api-endpoint" in str(tasks[0])
    assert "ingress-endpoint" in str(tasks[0])
    assert "include_role" not in str(tasks[0])


def test_ci_template_owns_external_access_dns_lifecycle():
    create = load_tasks(OCP_CI_SMALL / "tasks" / "create_external_access.yaml")
    delete = load_tasks(OCP_CI_SMALL / "tasks" / "delete_external_access.yaml")

    assert any("Create DNS records" in task.get("name", "") for task in create)
    assert any("Resolve authoritative nameserver" in task.get("name", "") for task in create)
    assert any("Wait for DNS records" in task.get("name", "") for task in create)
    assert any("Delete DNS records" in task.get("name", "") for task in delete)
    assert "network_steps_collection" not in (OCP_CI_SMALL / "tasks" / "create_external_access.yaml").read_text()
    assert "network_steps_collection" not in (OCP_CI_SMALL / "tasks" / "delete_external_access.yaml").read_text()
    assert not any("retrieve_kubeconfig" in str(task) for task in create)
    assert not any("wait_for_network_cluster_operator" in str(task) for task in create)


def test_ocp_small_keeps_its_dns_task_entry_points():
    install = load_tasks(OCP_SMALL / "tasks" / "install.yaml")
    delete = load_tasks(OCP_SMALL / "tasks" / "delete.yaml")
    install_defaults = install[0]["ansible.builtin.set_fact"]
    delete_defaults = delete[0]["ansible.builtin.set_fact"]

    assert install_defaults["install_step_external_access_default"]["tasks_from"] == "create_dns.yaml"
    assert delete_defaults["delete_step_external_access_default"]["tasks_from"] == "delete_dns.yaml"
