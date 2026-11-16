#!/usr/bin/env python3
"""Unit and Helm contract tests for the CaaS DNS upgrade guard."""

from __future__ import annotations

import contextlib
import importlib.util
import io
import json
import os
import pathlib
import subprocess
import tempfile
import unittest
from typing import Any
from unittest import mock
from urllib.parse import parse_qs

import yaml

INSTALLER = pathlib.Path(__file__).resolve().parents[1]
CHART = INSTALLER / "charts/osac"
DEFAULT_VALUES = CHART / "ci/default-values.yaml"
GUARD_PATH = CHART / "files/caas_dns_guard.py"
HELM = os.environ.get("HELM_BIN", "helm")

spec = importlib.util.spec_from_file_location("caas_dns_guard", GUARD_PATH)
assert spec and spec.loader
guard = importlib.util.module_from_spec(spec)
spec.loader.exec_module(guard)


def render(*overrides: dict[str, Any], upgrade: bool = False) -> list[dict[str, Any]]:
    values: dict[str, Any] = {}
    for override in overrides:
        _merge(values, override)
    with tempfile.NamedTemporaryFile("w", suffix=".json") as extra:
        json.dump(values, extra)
        extra.flush()
        command = [
            HELM,
            "template",
            "osac",
            str(CHART),
            "--namespace",
            "osac",
            "--values",
            str(DEFAULT_VALUES),
            "--values",
            extra.name,
        ]
        if upgrade:
            command.append("--is-upgrade")
        result = subprocess.run(command, capture_output=True, text=True)
    if result.returncode:
        raise AssertionError(f"Helm render failed: {result.stderr}")
    return [doc for doc in yaml.safe_load_all(result.stdout) if doc]


def _merge(destination: dict[str, Any], source: dict[str, Any]) -> None:
    for key, value in source.items():
        if isinstance(value, dict) and isinstance(destination.get(key), dict):
            _merge(destination[key], value)
        else:
            destination[key] = value


def caas_values(
    *,
    external_domain: str = "hosted.example.test",
    hosted_domain: str = "hosted.example.test",
    dns_zone: str = "example.test",
    dns_class: str = "dns.route53.dns",
) -> dict[str, Any]:
    return {
        "global": {"services": {"caas": {"enabled": True}}},
        "aap": {
            "instanceGroups": {
                "clusterFulfillment": {
                    "enabled": True,
                    "config": {
                        "EXTERNAL_ACCESS_BASE_DOMAIN": external_domain,
                        "HOSTED_CLUSTER_BASE_DOMAIN": hosted_domain,
                        "DNS_ZONE": dns_zone,
                        "DNS_CLASS": dns_class,
                    },
                }
            }
        },
    }


def find(docs: list[dict[str, Any]], kind: str, name: str) -> dict[str, Any]:
    for doc in docs:
        if doc.get("kind") == kind and doc.get("metadata", {}).get("name") == name:
            return doc
    raise AssertionError(f"{kind}/{name} not rendered")


class GuardUnitTest(unittest.TestCase):
    def setUp(self) -> None:
        self.env = {
            "CURRENT_HOSTED_CLUSTER_BASE_DOMAIN": "hosted.example.test",
            "CURRENT_EXTERNAL_ACCESS_BASE_DOMAIN": "hosted.example.test",
            "CURRENT_DNS_ZONE": "example.test",
            "CURRENT_DNS_CLASS": "dns.route53.dns",
            "PROPOSED_EXTERNAL_ACCESS_BASE_DOMAIN": "hosted.example.test",
            "PROPOSED_DNS_ZONE": "example.test",
            "PROPOSED_DNS_CLASS": "dns.route53.dns",
        }

    def test_unchanged_settings_skip_the_fulfillment_api(self) -> None:
        output = io.StringIO()
        with (
            contextlib.redirect_stdout(output),
            mock.patch.object(guard, "tls_context", side_effect=AssertionError("unexpected API call")),
        ):
            guard.check_upgrade(self.env)
        self.assertIn("unchanged", output.getvalue())

    def test_changed_settings_are_allowed_when_no_clusters_exist(self) -> None:
        self.env["PROPOSED_DNS_ZONE"] = "parent.example.test"
        with (
            mock.patch.object(guard, "tls_context", return_value=object()),
            mock.patch.object(guard, "fulfillment_access_token", return_value="secret-token"),
            mock.patch.object(guard, "clusters_exist", return_value=False) as check,
        ):
            guard.check_upgrade(self.env)
        check.assert_called_once()

    def test_changed_settings_are_rejected_when_a_cluster_exists(self) -> None:
        self.env["PROPOSED_DNS_CLASS"] = "dns.infoblox.dns"
        with (
            mock.patch.object(guard, "tls_context", return_value=object()),
            mock.patch.object(guard, "fulfillment_access_token", return_value="secret-token"),
            mock.patch.object(guard, "clusters_exist", return_value=True),
            self.assertRaisesRegex(guard.GuardError, "cannot change while Clusters exist"),
        ):
            guard.check_upgrade(self.env)

    def test_external_domain_change_is_rejected_when_a_cluster_exists(self) -> None:
        self.env["PROPOSED_EXTERNAL_ACCESS_BASE_DOMAIN"] = "new.example.test"
        self.env["PROPOSED_DNS_ZONE"] = "new.example.test"
        with (
            mock.patch.object(guard, "tls_context", return_value=object()),
            mock.patch.object(guard, "fulfillment_access_token", return_value="secret-token"),
            mock.patch.object(guard, "clusters_exist", return_value=True),
            self.assertRaisesRegex(guard.GuardError, "EXTERNAL_ACCESS_BASE_DOMAIN"),
        ):
            guard.check_upgrade(self.env)

    def test_missing_previous_configuration_fails_closed_if_cluster_exists(self) -> None:
        del self.env["CURRENT_HOSTED_CLUSTER_BASE_DOMAIN"]
        with (
            mock.patch.object(guard, "tls_context", return_value=object()),
            mock.patch.object(guard, "fulfillment_access_token", return_value="secret-token"),
            mock.patch.object(guard, "clusters_exist", return_value=True),
            self.assertRaisesRegex(guard.GuardError, "previous CaaS DNS settings"),
        ):
            guard.check_upgrade(self.env)

    def test_missing_previous_configuration_is_allowed_without_clusters(self) -> None:
        del self.env["CURRENT_HOSTED_CLUSTER_BASE_DOMAIN"]
        with (
            mock.patch.object(guard, "tls_context", return_value=object()),
            mock.patch.object(guard, "fulfillment_access_token", return_value="secret-token"),
            mock.patch.object(guard, "clusters_exist", return_value=False),
        ):
            guard.check_upgrade(self.env)

    def test_fulfillment_failure_is_not_treated_as_an_empty_cluster_list(self) -> None:
        self.env["PROPOSED_DNS_CLASS"] = "dns.infoblox.dns"
        with (
            mock.patch.object(guard, "tls_context", return_value=object()),
            mock.patch.object(guard, "fulfillment_access_token", return_value="secret-token"),
            mock.patch.object(guard, "clusters_exist", side_effect=guard.GuardError("Fulfillment request failed")),
            self.assertRaisesRegex(guard.GuardError, "Fulfillment request failed"),
        ):
            guard.check_upgrade(self.env)

    def test_cluster_list_uses_private_api_and_one_item_limit(self) -> None:
        response = io.BytesIO(b"{}")
        with mock.patch.object(guard.urllib.request, "urlopen", return_value=response) as urlopen:
            result = guard.clusters_exist(
                {"FULFILLMENT_URL": "https://fulfillment.example.test"}, object(), "access-token"
            )
        self.assertFalse(result)
        request = urlopen.call_args.args[0]
        self.assertEqual(request.full_url, "https://fulfillment.example.test/api/private/v1/clusters?limit=1")
        self.assertEqual(request.get_header("Authorization"), "Bearer access-token")

    def test_cluster_list_with_items_reports_existing_cluster(self) -> None:
        response = io.BytesIO(b'{"size":1,"total":1,"items":[{"id":"cluster-1"}]}')
        with mock.patch.object(guard.urllib.request, "urlopen", return_value=response):
            self.assertTrue(
                guard.clusters_exist({"FULFILLMENT_URL": "https://fulfillment.example.test"}, object(), "access-token")
            )

    def test_cluster_list_total_reports_existing_cluster_without_items(self) -> None:
        response = io.BytesIO(b'{"size":0,"total":2}')
        with mock.patch.object(guard.urllib.request, "urlopen", return_value=response):
            self.assertTrue(
                guard.clusters_exist({"FULFILLMENT_URL": "https://fulfillment.example.test"}, object(), "access-token")
            )

    def test_access_token_uses_client_credentials_secret_file(self) -> None:
        response = io.BytesIO(b'{"access_token":"access-token"}')
        with tempfile.NamedTemporaryFile("w", encoding="utf-8") as secret_file:
            secret_file.write("example-secret")
            secret_file.flush()
            with mock.patch.object(guard.urllib.request, "urlopen", return_value=response) as urlopen:
                token = guard.fulfillment_access_token(
                    {
                        "FULFILLMENT_ISSUER_URL": "https://idp.example.test/realms/osac",
                        "FULFILLMENT_CLIENT_ID": "osac-installer",
                        "FULFILLMENT_CLIENT_SECRET_FILE": secret_file.name,
                    },
                    object(),
                )
        request = urlopen.call_args.args[0]
        self.assertEqual(token, "access-token")
        self.assertEqual(request.full_url, "https://idp.example.test/realms/osac/protocol/openid-connect/token")
        self.assertEqual(
            parse_qs(request.data.decode()),
            {
                "grant_type": ["client_credentials"],
                "client_id": ["osac-installer"],
                "client_secret": ["example-secret"],
            },
        )

    def test_invalid_cluster_list_fails_closed(self) -> None:
        response = io.BytesIO(b'{"size":0,"total":0,"items":null}')
        with (
            mock.patch.object(guard.urllib.request, "urlopen", return_value=response),
            self.assertRaisesRegex(guard.GuardError, "invalid Cluster list"),
        ):
            guard.clusters_exist({"FULFILLMENT_URL": "https://fulfillment.example.test"}, object(), "access-token")


class GuardHelmContractTest(unittest.TestCase):
    def test_external_domain_must_match_hosted_cluster_domain(self) -> None:
        values = caas_values(external_domain="external.example.test")
        with tempfile.NamedTemporaryFile("w", suffix=".json") as extra:
            json.dump(values, extra)
            extra.flush()
            result = subprocess.run(
                [
                    HELM,
                    "template",
                    "osac",
                    str(CHART),
                    "--namespace",
                    "osac",
                    "--values",
                    str(DEFAULT_VALUES),
                    "--values",
                    extra.name,
                ],
                capture_output=True,
                text=True,
            )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("must equal HOSTED_CLUSTER_BASE_DOMAIN", result.stderr)

    def test_authoritative_parent_zone_is_allowed(self) -> None:
        docs = render(caas_values(dns_zone="example.test"))
        config = find(docs, "ConfigMap", "cluster-fulfillment-ig")
        self.assertEqual(config["data"]["DNS_ZONE"], "example.test")

    def test_dns_zone_defaults_to_external_domain(self) -> None:
        docs = render(caas_values(dns_zone=""), upgrade=True)
        job = find(docs, "Job", "caas-dns-guard")
        env = {entry["name"]: entry.get("value") for entry in job["spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env["PROPOSED_DNS_ZONE"], "hosted.example.test")

    def test_upgrade_hook_checks_current_and_proposed_settings(self) -> None:
        docs = render(caas_values(), upgrade=True)
        job = find(docs, "Job", "caas-dns-guard")
        container = job["spec"]["template"]["spec"]["containers"][0]
        env = {entry["name"]: entry for entry in container["env"]}

        self.assertEqual(env["PROPOSED_EXTERNAL_ACCESS_BASE_DOMAIN"]["value"], "hosted.example.test")
        self.assertEqual(env["PROPOSED_DNS_ZONE"]["value"], "example.test")
        self.assertEqual(env["PROPOSED_DNS_CLASS"]["value"], "dns.route53.dns")
        self.assertEqual(
            env["CURRENT_HOSTED_CLUSTER_BASE_DOMAIN"]["valueFrom"]["configMapKeyRef"]["key"],
            "HOSTED_CLUSTER_BASE_DOMAIN",
        )
        self.assertTrue(env["CURRENT_HOSTED_CLUSTER_BASE_DOMAIN"]["valueFrom"]["configMapKeyRef"]["optional"])
        self.assertEqual(job["metadata"]["annotations"]["helm.sh/hook"], "pre-upgrade")

    def test_install_does_not_render_upgrade_guard(self) -> None:
        docs = render(caas_values())
        self.assertFalse(any(doc.get("metadata", {}).get("name") == "caas-dns-guard" for doc in docs))


if __name__ == "__main__":
    unittest.main()
