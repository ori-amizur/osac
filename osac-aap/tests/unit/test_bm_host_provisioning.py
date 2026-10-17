"""Guard the Metal3-to-Assisted-Agent provisioning handoff."""

from pathlib import Path

import yaml


COMPONENT_ROOT = Path(__file__).resolve().parents[2]
CREATE_METAL3_TASKS = (
    COMPONENT_ROOT
    / "collections/ansible_collections/osac/templates/roles/bm_host_provisioning/tasks/create_metal3.yaml"
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
