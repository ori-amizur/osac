"""Unit tests for Kubernetes status-subresource patches in AAP jobs."""

import importlib.util
from pathlib import Path
from unittest import mock

import pytest


MODULE_PATH = (
    Path(__file__).parents[4]
    / "collections/ansible_collections/osac/service/plugins/modules/k8s_status_patch.py"
)
SPEC = importlib.util.spec_from_file_location("osac_k8s_status_patch", MODULE_PATH)
k8s_status_patch = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(k8s_status_patch)


def clear_auth_environment(monkeypatch):
    for name in (
        "K8S_AUTH_API_KEY",
        "K8S_AUTH_HOST",
        "K8S_AUTH_VERIFY_SSL",
        "K8S_AUTH_SSL_CA_CERT",
    ):
        monkeypatch.delenv(name, raising=False)


def test_builds_client_from_container_group_credentials(monkeypatch):
    clear_auth_environment(monkeypatch)
    monkeypatch.setenv("K8S_AUTH_API_KEY", "test-token")
    monkeypatch.setenv("K8S_AUTH_HOST", "https://kubernetes.default.svc")
    monkeypatch.setenv("K8S_AUTH_VERIFY_SSL", "true")
    monkeypatch.setenv("K8S_AUTH_SSL_CA_CERT", "/var/run/kube-api/ca.crt")

    with (
        mock.patch.object(k8s_status_patch.client, "Configuration") as config_factory,
        mock.patch.object(k8s_status_patch.client, "ApiClient") as api_client_factory,
    ):
        configuration = config_factory.return_value
        expected_client = api_client_factory.return_value

        result = k8s_status_patch.build_api_client()

    assert result is expected_client
    assert configuration.host == "https://kubernetes.default.svc"
    assert configuration.api_key == {"authorization": "test-token"}
    assert configuration.api_key_prefix == {"authorization": "Bearer"}
    assert configuration.verify_ssl is True
    assert configuration.ssl_ca_cert == "/var/run/kube-api/ca.crt"
    api_client_factory.assert_called_once_with(configuration=configuration)


def test_explicit_token_requires_api_host(monkeypatch):
    clear_auth_environment(monkeypatch)
    monkeypatch.setenv("K8S_AUTH_API_KEY", "test-token")

    with (
        mock.patch.object(k8s_status_patch.client, "Configuration"),
        pytest.raises(ValueError, match="K8S_AUTH_HOST is required"),
    ):
        k8s_status_patch.build_api_client()


def test_loads_explicit_kubeconfig(monkeypatch):
    clear_auth_environment(monkeypatch)

    with (
        mock.patch.object(k8s_status_patch.config, "load_kube_config") as load_config,
        mock.patch.object(k8s_status_patch.client, "ApiClient") as api_client_factory,
    ):
        expected_client = api_client_factory.return_value
        result = k8s_status_patch.build_api_client("/tmp/remote-kubeconfig")

    assert result is expected_client
    load_config.assert_called_once_with(config_file="/tmp/remote-kubeconfig")


def test_patches_custom_resource_status():
    api_client = mock.Mock()
    body = {"status": {"ips": ["10.240.1.1/24"]}}

    with mock.patch.object(k8s_status_patch.client, "CustomObjectsApi") as api_factory:
        k8s_status_patch.patch_status(
            api_client,
            "k8s.cni.cncf.io/v1alpha1",
            "IPAMClaim",
            "ipamclaims",
            "virtualnetwork-example",
            "subnet-example",
            body,
        )

    api_factory.assert_called_once_with(api_client)
    api_factory.return_value.patch_namespaced_custom_object_status.assert_called_once_with(
        group="k8s.cni.cncf.io",
        version="v1alpha1",
        namespace="virtualnetwork-example",
        plural="ipamclaims",
        name="subnet-example",
        body=body,
    )
