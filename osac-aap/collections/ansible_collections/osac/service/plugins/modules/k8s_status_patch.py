#!/usr/bin/python

"""Patch a Kubernetes resource through its status subresource."""

import os

from ansible.module_utils.basic import AnsibleModule
from kubernetes import client, config


DOCUMENTATION = r"""
---
module: k8s_status_patch
short_description: Patch a Kubernetes resource status subresource
description:
  - Applies a merge patch through the resource's status endpoint.
  - Uses an explicit kubeconfig when provided, otherwise honors the Kubernetes
    authentication environment variables used by AAP container groups.
options:
  api_version:
    description:
      - Kubernetes API version, in C(group/version) form for a custom resource.
    type: str
    required: true
  kind:
    description:
      - Kubernetes resource kind.
    type: str
    required: true
  plural:
    description:
      - Kubernetes API plural name for the resource.
    type: str
    required: true
  namespace:
    description:
      - Namespace containing the resource.
    type: str
    required: true
  name:
    description:
      - Name of the resource to patch.
    type: str
    required: true
  body:
    description:
      - Merge-patch body sent to the status subresource.
    type: dict
    required: true
  kubeconfig:
    description:
      - Path to a kubeconfig for a remote cluster. If omitted, AAP Kubernetes
        authentication environment variables or the default client config are used.
    type: path
    required: false
author: OSAC
"""


def build_api_client(kubeconfig_path=None):
    """Build a Kubernetes API client for kubeconfig or AAP container credentials."""
    if kubeconfig_path:
        config.load_kube_config(config_file=kubeconfig_path)
        return client.ApiClient()

    bearer_token = os.environ.get("K8S_AUTH_API_KEY")
    if bearer_token:
        host = os.environ.get("K8S_AUTH_HOST")
        if not host:
            raise ValueError("K8S_AUTH_HOST is required when K8S_AUTH_API_KEY is set")

        configuration = client.Configuration()
        configuration.host = host
        configuration.api_key = {"authorization": bearer_token}
        configuration.api_key_prefix = {"authorization": "Bearer"}
        configuration.verify_ssl = os.environ.get("K8S_AUTH_VERIFY_SSL", "true").lower() not in {
            "false",
            "no",
            "0",
        }
        ca_cert = os.environ.get("K8S_AUTH_SSL_CA_CERT")
        if ca_cert:
            configuration.ssl_ca_cert = ca_cert
        return client.ApiClient(configuration=configuration)

    config.load_config()
    return client.ApiClient()


def patch_status(api_client, api_version, kind, plural, namespace, name, body):
    """Apply a custom-resource status patch using the Kubernetes API."""
    if "/" not in api_version:
        raise ValueError(
            "Custom resources require api_version in '<group>/<version>' form"
        )
    group, version = api_version.split("/", 1)
    # This generated endpoint already sets application/merge-patch+json and
    # rejects `_content_type` as an unsupported keyword argument.
    return client.CustomObjectsApi(api_client).patch_namespaced_custom_object_status(
        group=group,
        version=version,
        namespace=namespace,
        plural=plural,
        name=name,
        body=body,
    )


def main():
    module = AnsibleModule(
        argument_spec={
            "api_version": {"type": "str", "required": True},
            "kind": {"type": "str", "required": True},
            "plural": {"type": "str", "required": True},
            "namespace": {"type": "str", "required": True},
            "name": {"type": "str", "required": True},
            "body": {"type": "dict", "required": True},
            "kubeconfig": {"type": "path"},
        },
        supports_check_mode=True,
    )

    if module.check_mode:
        module.exit_json(changed=True, msg="Status patch would be applied")

    try:
        api_client = build_api_client(module.params.get("kubeconfig"))
        result = patch_status(
            api_client=api_client,
            api_version=module.params["api_version"],
            kind=module.params["kind"],
            plural=module.params["plural"],
            namespace=module.params["namespace"],
            name=module.params["name"],
            body=module.params["body"],
        )
    except Exception as error:  # Kubernetes client errors should be actionable in AAP.
        module.fail_json(msg=f"Failed to patch {module.params['kind']} status: {error}")

    module.exit_json(changed=True, result=api_client.sanitize_for_serialization(result))


if __name__ == "__main__":
    main()
