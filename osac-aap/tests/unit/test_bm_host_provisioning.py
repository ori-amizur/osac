"""Guard the Metal3-to-Assisted-Agent provisioning handoff."""

from pathlib import Path

import yaml


COMPONENT_ROOT = Path(__file__).resolve().parents[2]
CREATE_METAL3_TASKS = (
    COMPONENT_ROOT
    / "collections/ansible_collections/osac/templates/roles/bm_host_provisioning/tasks/create_metal3.yaml"
)
DEFAULT_NETWORK_DATA_TASKS = (
    COMPONENT_ROOT
    / "collections/ansible_collections/osac/templates/roles/bm_host_provisioning/tasks/default_network_data.yaml"
)
BUILD_BMH_PATCH_TASKS = (
    COMPONENT_ROOT
    / "collections/ansible_collections/osac/templates/roles/bm_host_provisioning/tasks/build_bmh_patch.yaml"
)


def test_assisted_agent_wait_is_gated_to_ignition_and_matches_bmh_mac() -> None:
    tasks = yaml.safe_load(CREATE_METAL3_TASKS.read_text())
    provision_block = next(
        task["block"] for task in tasks if task.get("name") == "Provision BMH"
    )
    build_user_data = next(
        task for task in provision_block if task.get("name") == "Build userData content"
    )
    detect = next(
        task for task in build_user_data["block"]
        if task.get("name") == "Detect Assisted Installer ignition user data"
    )
    wait = next(
        task for task in provision_block if task.get("name") == "Wait for the host's Assisted Agent to register"
    )

    assert detect["block"][0]["ansible.builtin.set_fact"]["wait_for_assisted_agent"] == (
        "{{ (user_data_secret_result.resources[0].data.userdata | b64decode | from_json).ignition is defined }}"
    )
    assert detect["rescue"][0]["ansible.builtin.set_fact"]["wait_for_assisted_agent"] is False
    assert wait["when"] == "wait_for_assisted_agent | default(false) | bool"
    assert wait["kubernetes.core.k8s_info"]["kind"] == "Agent"
    assert wait["kubernetes.core.k8s_info"]["namespace"] == "{{ bare_metal_instance.metadata.namespace }}"
    assert "selectattr('macAddress', 'equalto', bmh_info.spec.bootMACAddress)" in wait["until"]
    assert wait["retries"] == 180
    assert wait["delay"] == 10


def test_metal3_network_data_defaults_to_boot_interface_dhcp() -> None:
    create_tasks = yaml.safe_load(CREATE_METAL3_TASKS.read_text())
    default_tasks = yaml.safe_load(DEFAULT_NETWORK_DATA_TASKS.read_text())
    patch_tasks = yaml.safe_load(BUILD_BMH_PATCH_TASKS.read_text())
    provision_block = next(
        task["block"] for task in create_tasks if task.get("name") == "Provision BMH"
    )

    extract = next(
        task for task in create_tasks
        if task.get("name") == "Extract SSH public key, userDataSecret, and networkData from template parameters"
    )
    default = next(
        task for task in create_tasks
        if task.get("name") == "Default networkData to DHCP on the Metal3 boot interface"
    )
    secret = next(
        task for task in provision_block if task.get("name") == "Create networkData Secret"
    )
    patch = next(
        task for task in patch_tasks if task.get("name") == "Add networkData to BMH patch"
    )

    assert extract["ansible.builtin.set_fact"]["bmh_network_data"] == (
        "{{ template_params.networkData | default('') }}"
    )
    assert default["ansible.builtin.include_tasks"] == "default_network_data.yaml"
    assert secret["when"] == "bmh_network_data | length > 0"
    assert secret["kubernetes.core.k8s"]["definition"]["stringData"]["networkData"] == (
        "{{ bmh_network_data }}"
    )
    assert patch["when"] == "bmh_network_data | default('') | length > 0"
    assert default_tasks[0]["when"] == (
        "bmh_network_data | default('') | length == 0 and bmh_info.spec.bootMACAddress | default('') | length > 0"
    )
    network_data = default_tasks[0]["ansible.builtin.set_fact"]["bmh_network_data"]
    assert "ipv4_dhcp" in network_data
    assert "ethernet_mac_address': bmh_info.spec.bootMACAddress" in network_data
