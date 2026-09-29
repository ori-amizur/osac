# AAP container-group authentication and router recovery

This note records the AAP authentication and network-provisioning issues found
while preparing the September 30, 2026 SNO test setup. It distinguishes durable
source fixes from test-cluster configuration so the latter can be checked before
being repeated.

## Root causes and source fixes

### AAP job pods do not necessarily have a mounted ServiceAccount token

The template-publisher job failed with `Service token file does not exist.` The
job's container-group PodSpec disabled or did not provide the usual in-cluster
token file, while `osac.service.client_token` tried to load the default
in-cluster Kubernetes configuration. Kubernetes modules configured through
`K8S_AUTH_*` could still authenticate, but this custom module did not use those
credentials.

The durable fix is for the custom module to build its Kubernetes client from
`K8S_AUTH_API_KEY`, `K8S_AUTH_HOST`, `K8S_AUTH_VERIFY_SSL`, and
`K8S_AUTH_SSL_CA_CERT`, with kubeconfig loading retained as a fallback. Do not
make the job depend on an implicitly mounted ServiceAccount token.

### Shelling out to `kubectl` does not inherit Ansible's Kubernetes auth

The router recovery job reached the task that reads the router agent status,
then failed because `kubectl` tried `localhost:8080`. The job had Kubernetes
credentials in `K8S_AUTH_*`, but those environment variables are consumed by
the Ansible Kubernetes collection, not automatically by a separate `kubectl`
process. This made a healthy, Ready router Pod look unrecoverable to AAP.

The recovery status read and router command paths, including live detach, now
use `kubernetes.core.k8s_exec`, which uses the same Kubernetes authentication as
the other `kubernetes.core` tasks. The live-detach request is base64-encoded
before being written in the helper Pod because the pinned `k8s_exec` module does
not expose stdin; the direct Multus CNI DEL request and its ordering are
unchanged. IPAMClaim status writes use the OSAC `k8s_status_patch` module so they
use the authenticated Kubernetes client and the actual status subresource. Do
not fix this by adding a cluster-local `KUBECONFIG` assumption to the job
container.

The subnet-creation, router-attachment, and live-detach command conversions
address the same auth gap in this SNO AAP container group, where no remote
kubeconfig is provided. They preserve the OVN/CNI operations and their
arguments; only the Kubernetes API transport changes. When a remote kubeconfig
is explicitly configured, the Kubernetes modules use it instead of the AAP
container-group credentials.

When adding AAP tasks, prefer `kubernetes.core.k8s`, `k8s_info`, `k8s_json_patch`,
and `k8s_exec` (or an OSAC module using the Kubernetes Python client). A raw
`kubectl` subprocess is only safe when the task explicitly provides a valid
kubeconfig; `K8S_AUTH_*` alone is not enough.

### A running router Pod does not mean router recovery completed

The router Pod was `Running` and Ready, but the VirtualNetwork remained
`Progressing` because the AAP recovery job had failed. Recovery is a separate
reconciliation: it rebuilds the router configuration from Subnet/IPAMClaim
state, reapplies the router Pod's network annotation, waits for the expected
attachments and agent configuration version, then converges OVN port security.
Check the AAP job result and the router agent status/config version, not only
the Pod phase.

The initial failed status read returned a valid router-agent status when read
directly: the agent was ready and had applied the empty network configuration.
That established an AAP-to-Kubernetes exec/authentication failure, not a router
agent or Pod startup failure.

## Source changes made in this worktree

These are implementation changes, not temporary patches to the running cluster:

- `service/plugins/modules/client_token.py` now builds its Kubernetes client
  from the AAP `K8S_AUTH_*` credentials when present, rather than requiring an
  in-cluster token file. This change was already present in the dirty worktree
  when this investigation resumed; its focused tests pass.
- `templates/roles/cudn_net/tasks/recover_router_pod.yaml` reads the agent
  status file using `kubernetes.core.k8s_exec` instead of a `kubectl` process.
- `templates/roles/cudn_net/tasks/live_detach_cni_del.yaml` uses authenticated
  `kubernetes.core.k8s_exec` for all helper-Pod commands instead of shelling
  out to `kubectl exec`. The CNI DEL request is transported as base64 because
  the pinned upstream module does not support stdin; this does not change the
  request contents, target socket, or ordering before the router Pod annotation
  is changed.
- `templates/roles/cudn_net/tasks/patch_router_pod_network_status_removal.yaml`
  patches the Pod's `network-status` metadata annotation through the normal Pod
  API using `kubernetes.core.k8s_json_patch`, matching the authenticated Pod
  annotation updates used elsewhere in this role. It no longer shells out to
  `kubectl` for this metadata update.
- The subnet-add and router-attachment tasks
  (`add_router_pod_subnet.yaml`, `prepare_router_pod_lsp.yaml`,
  `verify_router_pod_networks.yaml`, `patch_router_pod_port_security.yaml`,
  and `patch_router_pod_lsp.yaml`) use authenticated Kubernetes modules for
  status writes and Pod exec. The OVN operations and arguments are preserved;
  task results still check command return codes where success is required.
- `service/plugins/modules/k8s_status_patch.py` is a reusable OSAC Ansible
  module for merge-patching a custom resource's status subresource. It supports
  an explicit remote kubeconfig or the AAP Kubernetes auth environment and is
  used to reserve the Subnet gateway on its IPAMClaim.
- The live-detach CNI DEL behavior is retained; its Kubernetes API transport
  and Pod metadata annotation patch are now consistent with the authenticated
  attach/recovery paths.

No live-cluster PodSpec override, manual router annotation, or ad-hoc
`KUBECONFIG` workaround was left in place. The earlier temporary group-8
PodSpec override was cleared. These source edits have not been committed or
deployed: the AAP project still needs a source revision containing them before
recovery or subnet add/delete can be validated against the cluster.

## Required AAP container-group configuration

For each container group whose jobs call the Kubernetes API:

1. Set the intended `serviceAccountName` and ensure that ServiceAccount has the
   required RBAC in the target cluster.
2. Provide an API token explicitly as `K8S_AUTH_API_KEY` (from a Kubernetes
   Secret key reference), set `K8S_AUTH_HOST` to the in-cluster API endpoint,
   and set `K8S_AUTH_SSL_CA_CERT` to a readable CA file.
3. Mount the namespace `kube-root-ca.crt` ConfigMap at the path used by
   `K8S_AUTH_SSL_CA_CERT`.
4. Keep remote-cluster authentication separate. When a remote kubeconfig is
   provided through `OSAC_REMOTE_CLUSTER_KUBECONFIG` or
   `OSAC_REMOTE_CLUSTER_KUBECONFIG_CONTENT`, tasks pass that kubeconfig to the
   Kubernetes modules; otherwise they use the container-group `K8S_AUTH_*`
   credentials.

The AAP source configuration currently in this worktree applies explicit auth
per group as follows:

| AAP job group | Kubernetes identity / token Secret |
| --- | --- |
| Cluster fulfillment, networking, compute-instance, storage, bare-metal | `osac-sa` / `osac-sa-token` |
| Config-as-code | `osac-aap-config-as-code` / `osac-aap-config-as-code-token` |
| Template publisher | `template-publisher` / `template-publisher-token` |

Each group also sets `K8S_AUTH_HOST=https://kubernetes.default.svc` and mounts
`kube-root-ca.crt` at `/var/run/osac-kube-api/ca.crt`. Recheck any newly added
container group: credentials on one group do not propagate to another.

Do not put token values, kubeconfig contents, or private SSH key material in
this document, AAP job output, or source control. Secret references and file
paths are sufficient for configuration review.

## Test-cluster configuration and cautions

These are captured test-environment settings, not universal defaults:

- The cluster's default NetworkClass was changed to the Kubernetes-only
  implementation because this test has no fabric manager. This affects new
  VirtualNetworks using that default; confirm the intended NetworkClass before
  creating resources in another environment.
- The default Primary VirtualNetwork and its default Subnet were already
  provisioned and Ready. They should be reused rather than duplicated.
- The requested one external IP uses the OSAC ExternalIPPool backed by
  MetalLB. Before changing a pool, inspect all MetalLB pools and allocated
  LoadBalancer addresses for overlap. During this test, the CaaS range was
  narrowed to `192.168.160.240-192.168.160.247`, and the OSAC pool used
  `192.168.160.248/29` (six host addresses available). This was a coordinated
  split of the then-default range, not a general recommendation to change a
  cluster's pool ranges.
- The shared VM SSH private key is held locally at
  `/home/oamizur/.ssh/osac-test-vms`; only its public key should be supplied to
  VM resources. The private key is intentionally not included here.

## Captured provisioning state

At the time of this note:

- Tenant `tenant1`, the Primary VN/Subnet, the SSH keypair, the test DiskImage,
  the test InstanceType, and the OSAC external-IP pool had been created.
- Secondary VN `tenant1-secondary-vn` (Kubernetes object
  `virtualnetwork-499hv`, ID `01a0eef4-9509-7762-aecf-a2ba978bfa2e`) existed,
  but remained `Progressing` while recovery was failing.
- Its router Pod was Running and Ready. No Secondary Subnets or VMs had yet
  been created, and no external IP had yet been allocated to a VM.
- The AAP project's live SCM revision was
  `065d83bf85446dcab1494496cc1a0a1247e3430b`. The source fixes in the local
  worktree were not part of that revision, so they were not live in AAP. They
  must be committed to the authorized fork branch and the AAP project synced
  before cluster provisioning can validate them.

Refresh this section after each test run; IDs, pool allocations, and AAP SCM
revisions are environment state, not durable configuration.

## Validation of source changes

The following checks passed after the changes described above:

- `uv run --project . pytest -c pyproject.toml tests/unit` — 114 passed.
- Targeted `ansible-lint` on the changed CUDN task/default files — no failures or
  warnings.
- `ansible-playbook -i localhost, playbook_osac_delete_subnet.yml
  --syntax-check` — passed.

These are source-level checks. The live AAP project was still on the older SCM
revision recorded above, so neither recovery nor live detach has been runtime-
verified with these changes yet. Do not report the cluster workflow as fixed
until AAP is synced to a revision containing the source changes and the
relevant lifecycle is exercised.
