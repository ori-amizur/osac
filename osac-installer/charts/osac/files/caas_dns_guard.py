#!/usr/bin/env python3
"""Reject unsafe CaaS DNS setting changes during an OSAC upgrade."""

from __future__ import annotations

import json
import os
import ssl
import sys
import urllib.error
import urllib.parse
import urllib.request

DEFAULT_EXTERNAL_DOMAIN = "box.massopen.cloud"
DEFAULT_DNS_CLASS = "dns.route53.dns"
PROTECTED_SETTINGS = ("EXTERNAL_ACCESS_BASE_DOMAIN", "DNS_ZONE", "DNS_CLASS")


class GuardError(Exception):
    """The upgrade cannot safely change CaaS DNS configuration."""


def effective_settings(env: dict[str, str], prefix: str) -> dict[str, str]:
    external_domain = env.get(f"{prefix}EXTERNAL_ACCESS_BASE_DOMAIN", "") or DEFAULT_EXTERNAL_DOMAIN
    dns_zone = env.get(f"{prefix}DNS_ZONE", "") or external_domain
    dns_class = env.get(f"{prefix}DNS_CLASS", "") or DEFAULT_DNS_CLASS
    return {"EXTERNAL_ACCESS_BASE_DOMAIN": external_domain, "DNS_ZONE": dns_zone, "DNS_CLASS": dns_class}


def tls_context(env: dict[str, str]) -> ssl.SSLContext:
    ca_file = env.get("FULFILLMENT_CA_FILE", "")
    if not ca_file:
        raise GuardError("Fulfillment CA bundle is unavailable")
    try:
        return ssl.create_default_context(cafile=ca_file)
    except (OSError, ssl.SSLError, ValueError):
        raise GuardError("Fulfillment CA bundle could not be loaded") from None


def request_json(
    url: str, *, context: ssl.SSLContext, data: bytes | None = None, headers: dict[str, str] | None = None
) -> dict:
    try:
        request = urllib.request.Request(url, data=data, headers=headers or {})
        with urllib.request.urlopen(request, context=context, timeout=30) as response:
            payload = json.loads(response.read())
    except urllib.error.HTTPError as error:
        raise GuardError(f"Fulfillment returned HTTP {error.code}") from None
    except (UnicodeDecodeError, json.JSONDecodeError):
        raise GuardError("Fulfillment returned an invalid JSON response") from None
    except (urllib.error.URLError, TimeoutError, OSError, ValueError):
        raise GuardError("Fulfillment request failed") from None
    if not isinstance(payload, dict):
        raise GuardError("Fulfillment returned an invalid JSON response")
    return payload


def fulfillment_access_token(env: dict[str, str], context: ssl.SSLContext) -> str:
    issuer_url = env.get("FULFILLMENT_ISSUER_URL", "").rstrip("/")
    client_id = env.get("FULFILLMENT_CLIENT_ID", "")
    secret_file = env.get("FULFILLMENT_CLIENT_SECRET_FILE", "")
    if not issuer_url.startswith("https://") or not client_id or not secret_file:
        raise GuardError("Fulfillment service-account credentials are unavailable")
    try:
        with open(secret_file, encoding="utf-8") as secret:
            client_secret = secret.read()
    except (OSError, UnicodeError):
        raise GuardError("Fulfillment service-account credentials are unavailable") from None
    if not client_secret:
        raise GuardError("Fulfillment service-account credentials are unavailable")

    body = urllib.parse.urlencode(
        {"grant_type": "client_credentials", "client_id": client_id, "client_secret": client_secret}
    ).encode()
    response = request_json(
        f"{issuer_url}/protocol/openid-connect/token",
        context=context,
        data=body,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
    )
    token = response.get("access_token")
    if not isinstance(token, str) or not token:
        raise GuardError("Fulfillment token response was invalid")
    return token


def clusters_exist(env: dict[str, str], context: ssl.SSLContext, access_token: str) -> bool:
    base_url = env.get("FULFILLMENT_URL", "").rstrip("/")
    if not base_url.startswith("https://"):
        raise GuardError("Fulfillment private API URL is unavailable")
    url = f"{base_url}/api/private/v1/clusters?limit=1"
    response = request_json(url, context=context, headers={"Authorization": f"Bearer {access_token}"})
    # Fulfillment uses protojson with default fields omitted, so an empty
    # successful response may be `{}` rather than including `items: []`.
    items = response.get("items", [])
    if not isinstance(items, list):
        raise GuardError("Fulfillment returned an invalid Cluster list")
    if items:
        return True
    for field in ("size", "total"):
        count = response.get(field, 0)
        if field in response and (isinstance(count, bool) or not isinstance(count, int) or count < 0):
            raise GuardError("Fulfillment returned an invalid Cluster list")
        if count:
            return True
    return False


def check_upgrade(env: dict[str, str]) -> None:
    """Compare configured settings and query Fulfillment only when needed."""
    current_known = "CURRENT_HOSTED_CLUSTER_BASE_DOMAIN" in env
    proposed = effective_settings(env, "PROPOSED_")
    changed = []
    if current_known:
        current = effective_settings(env, "CURRENT_")
        changed = [key for key in PROTECTED_SETTINGS if current[key] != proposed[key]]
        if not changed:
            print("CaaS DNS settings are unchanged; skipping the Cluster check.")
            return
        print(f"CaaS DNS settings changed: {', '.join(changed)}.")
    else:
        print("Previous CaaS configuration is unavailable; checking Clusters.")

    context = tls_context(env)
    token = fulfillment_access_token(env, context)
    if clusters_exist(env, context, token):
        if not current_known:
            raise GuardError(
                "Cannot determine the previous CaaS DNS settings while Clusters exist; refusing this upgrade."
            )
        raise GuardError(
            "CaaS DNS settings cannot change while Clusters exist; delete every "
            "Cluster first. Changed settings: " + ", ".join(changed)
        )
    print("No Clusters exist; the CaaS DNS setting change is allowed.")


def main() -> int:
    try:
        check_upgrade(dict(os.environ))
    except GuardError as error:
        print(f"ERROR: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
