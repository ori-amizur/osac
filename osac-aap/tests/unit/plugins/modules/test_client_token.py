"""Tests for Kubernetes authentication in the AAP container-group module."""

import importlib.util
from pathlib import Path
from unittest import mock

import pytest


MODULE_PATH = (
    Path(__file__).parents[4]
    / "collections/ansible_collections/osac/service/plugins/modules/client_token.py"
)
SPEC = importlib.util.spec_from_file_location("osac_client_token", MODULE_PATH)
client_token = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(client_token)


def clear_auth_environment(monkeypatch):
    for name in (
        "K8S_AUTH_API_KEY",
        "K8S_AUTH_HOST",
        "K8S_AUTH_VERIFY_SSL",
        "K8S_AUTH_SSL_CA_CERT",
    ):
        monkeypatch.delenv(name, raising=False)


def test_builds_api_client_from_explicit_container_group_credentials(monkeypatch):
    clear_auth_environment(monkeypatch)
    monkeypatch.setenv("K8S_AUTH_API_KEY", "test-token")
    monkeypatch.setenv("K8S_AUTH_HOST", "https://kubernetes.default.svc")
    monkeypatch.setenv("K8S_AUTH_VERIFY_SSL", "true")
    monkeypatch.setenv("K8S_AUTH_SSL_CA_CERT", "/var/run/kube-ca/ca.crt")

    with (
        mock.patch.object(client_token.client, "Configuration") as configuration_factory,
        mock.patch.object(client_token.client, "ApiClient") as api_client_factory,
        mock.patch.object(client_token.client, "CoreV1Api") as core_api_factory,
    ):
        configuration = configuration_factory.return_value
        api_client = api_client_factory.return_value
        expected_api = core_api_factory.return_value

        result = client_token.build_core_v1_api()

    assert result is expected_api
    assert configuration.host == "https://kubernetes.default.svc"
    assert configuration.api_key == {"authorization": "test-token"}
    assert configuration.api_key_prefix == {"authorization": "Bearer"}
    assert configuration.verify_ssl is True
    assert configuration.ssl_ca_cert == "/var/run/kube-ca/ca.crt"
    api_client_factory.assert_called_once_with(configuration=configuration)
    core_api_factory.assert_called_once_with(api_client=api_client)


def test_explicit_token_requires_api_host(monkeypatch):
    clear_auth_environment(monkeypatch)
    monkeypatch.setenv("K8S_AUTH_API_KEY", "test-token")

    with pytest.raises(ValueError, match="K8S_AUTH_HOST is required"):
        client_token.build_core_v1_api()


def test_falls_back_to_kubernetes_client_config(monkeypatch):
    clear_auth_environment(monkeypatch)

    with (
        mock.patch.object(client_token.config, "load_config") as load_config,
        mock.patch.object(client_token.client, "CoreV1Api") as core_api_factory,
    ):
        expected_api = core_api_factory.return_value
        result = client_token.build_core_v1_api()

    assert result is expected_api
    load_config.assert_called_once_with()
    core_api_factory.assert_called_once_with()
