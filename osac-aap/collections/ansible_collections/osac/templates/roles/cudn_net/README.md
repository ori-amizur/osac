# cudn_net

Provisions networking resources using ClusterUserDefinedNetwork (CUDN) on OpenShift.

> **Note:** SecurityGroup enforcement (NetworkPolicy) has been extracted to the standalone
> `osac.templates.network_policy` role so it can be reused across any K8s-based NetworkClass.
> `cudn_net`'s `create_security_group`/`delete_security_group` entrypoints delegate to that
> role directly (see [Task Files](#task-files)) — the dispatcher resolves `SecurityGroup` to
> this NetworkClass's fabric manager (`cudn_net`) the same way it does for `VirtualNetwork`
> and `Subnet`, so `cudn_net` must provide these entrypoints even though the underlying
> enforcement mechanism lives in `network_policy`.

## Resources

### VirtualNetwork

VirtualNetworks define the top-level network isolation boundary with CIDR allocation and implementation strategy selection via NetworkClass.

**Key behaviors:**
- For a Secondary VirtualNetwork, creates the VN namespace and router Pod
- For a fabric-backed Secondary VirtualNetwork, consumes that VN's fabric
  transit contract and creates one isolated Primary EVPN CUDN plus a
  persistent transit IPAMClaim
- Each fabric-backed VirtualNetwork owns a separate transit CUDN and MAC-VRF;
  individual subnet changes do not alter the VN-scoped transit interface
- For a Primary VirtualNetwork, this entrypoint remains a logical grouping and
  does not create a router namespace

**Implementation:**
- ClusterUserDefinedNetwork is a cluster-scoped resource
- CIDR ranges are configured via spec.network field
- Layer2 topology is used for CUDN implementation

### Subnet

Subnets subdivide VirtualNetworks into logical segments with isolated namespaces for workload deployment.

**Key behaviors:**
- Creates namespace with specific labels for CUDN attachment
- Namespace labeled with `osac.openshift.io/virtual-network: {vn-name}`
- Namespace labeled with `k8s.ovn.org/primary-user-defined-network: ""`
- Enables pod connectivity to the parent VirtualNetwork's CUDN
- Primary Subnets map to one namespace; Secondary Subnets attach to the shared
  VirtualNetwork router Pod.

### Secondary Subnet live attach and fallback

Secondary Subnet add/remove operations use a per-VirtualNetwork Lease as a mutex
while they read and update the aggregate router Pod network annotation and
router-agent ConfigMap. Attach's live path patches the running Pod and waits
for the requested interface change in `k8s.v1.cni.cncf.io/network-status`. The
initial timeout is 60 seconds, configured by
`cudn_net_router_live_attach_timeout_seconds`; each operation has its own
deadline. Detach's live path instead confirms success directly from the
interface-removal call itself (see below) and patches `network-status` without
waiting, since there is nothing to wait for once the interface is already
confirmed gone.

If the live attach patch fails, the expected network-status change is not
observed before the deadline, or the live detach's direct CNI DEL fails, the
role patches the Deployment template with the complete desired attachment list
and waits for the replacement Pod. This is the Epic 1 recreation fallback,
shared by both directions.

**Attach ordering (create_cudn_for_live_attach.yaml):** the subnet's CUDN is
created *after* the router Pod's annotation already announces it, not before.
OVN-Kubernetes's per-network Pod controller only recomputes a Pod's network
membership from scratch on its own controller startup (CUDN creation) or a Pod
Add event -- never on a plain Pod Update to an already-scheduled Pod, and there
is no removal path on Update either. Announcing the subnet first means the
CUDN's own startup sync discovers the Pod immediately and creates the logical
port without a restart. `multus-dynamic-networks-controller`'s first attempt
races ahead of the NetworkAttachmentDefinition's existence and fails
harmlessly with "not found". The pre-CUDN annotation is deliberately formatted
differently from the normal compact JSON; once the NAD exists, the caller's
normal patch becomes a meaningful Pod update that retriggers only the missing
ADD, without the stale DEL race caused by temporarily removing and restoring
the attachment list.

**Detach (remove_router_pod_subnet.yaml, live_detach_cni_del.yaml,
check_subnet_still_referenced.yaml):** removing a subnet from the running Pod's
own `k8s.v1.cni.cncf.io/networks` annotation cannot trigger OVN-Kubernetes's own
CNI DEL handling -- it resolves which NAD a DEL belongs to by re-reading that
same annotation, which by construction has already lost the entry being
removed. Instead, the role calls Multus's own `/delegate` socket directly, from
a short-lived, node-scoped helper Pod, with the DEL request built from the
NAD's own config *before* the running Pod's annotation is touched. This closes
the ordering gap and detaches the interface without any Pod recreation in the
common case, falling back to the same Deployment recreation as attach only if
the direct DEL itself fails or times out.

Because the CUDN delete only *blocks* on a live, still-referenced subnet (never
disrupts an already-stable interface), the role issues a normal
(non-forcing) CUDN delete *first*, before touching the router Pod at all, then
resolves who else references it by reading the CUDN's own `namespaceSelector`
live (it can match more than one namespace) and checking every pod found
there against the subnet's NAD -- explicitly excluding the router Pod's own
`k8s.ovn.org/pod-networks` entry, which never clears on a live Pod (no
OVN-Kubernetes code path does it, and the `network-node-identity` admission
webhook blocks anyone else from patching it directly). Only once nothing else
references the subnet does the role perform the direct CNI DEL, patch the
running Pod's own annotation and network-status to drop the entry, and
force-clear the CUDN's and NAD's finalizers to let the delete started earlier
complete.

The current router Pod records the result in
`osac.openshift.io/router-attachment-mode` (`live` or `recreated`) and the
Subnet name in `osac.openshift.io/router-attachment-subnet`. These annotations
are observability metadata; Multus watches only its own network annotation.

**Namespace targeting:**
- Pods deployed in the Subnet namespace automatically connect to the VirtualNetwork's CUDN
- No additional network configuration needed on pods
- Network attachment is namespace-based, not interface-based

## Implementation Strategy

This role implements the `cudn_net` NetworkClass strategy using OpenShift's ClusterUserDefinedNetwork (CUDN) feature. The implementation follows these patterns:

**For VirtualNetworks:**
- Create the Secondary VirtualNetwork router namespace and Deployment
- When `osac.openshift.io/transit-capability=netris-evpn`, consume the
  deterministic VN-scoped ConfigMap published by the fabric role and create
  an isolated Layer2/EVPN/MAC-VRF CUDN. The CUDN uses the fabric-owned transit
  CIDR and VNI. Netris and CUDN IPAM use disjoint pools within that CIDR: by
  default Netris owns one `/26`, OSAC/CUDN owns a separate `/26`, and the
  remaining `/25` is reserved. The Netris pool and reserved remainder are
  represented in `reservedSubnets`; a host-list snapshot alone is not enough
  to prevent future duplicate allocations.
- Configure the CUDN with `ipam.lifecycle: Persistent`, create the router Pod's
  `IPAMClaim`, and request it through the Primary UDN claim annotation.
- When the annotation is absent, `none`, or `unsupported`, skip all EVPN
  transit resources. Fabric-manager presence alone is not sufficient.

**For Subnets:**
- Create namespace with CUDN attachment labels
- Label namespace with parent VirtualNetwork reference
- Pods deployed in namespace automatically connect to CUDN

## Task Files

- `tasks/create_virtual_network.yaml` - Creates ClusterUserDefinedNetwork CR from VirtualNetwork resource
- `tasks/delete_virtual_network.yaml` - Removes ClusterUserDefinedNetwork CR
- `tasks/create_subnet.yaml` - Creates namespace with CUDN labels from Subnet resource
- `tasks/delete_subnet.yaml` - Removes namespace or detaches a Secondary Subnet
- `tasks/reconcile_router_pod_subnet.yaml` - Mutex-protected aggregate router state update
- `tasks/create_cudn_for_live_attach.yaml` - Attach-only: creates the Subnet's CUDN after announcing it to the router Pod, then retriggers the live attach
- `tasks/patch_router_pod_networks.yaml` - Live mutation with recreation fallback (used directly by attach)
- `tasks/remove_router_pod_subnet.yaml` - Detach-only: CUDN delete, reference check, live CNI DEL, and finalizer force-clear, all under the per-VirtualNetwork Lease
- `tasks/live_detach_cni_del.yaml` - Direct CNI DEL against Multus's `/delegate` socket from an ephemeral, node-scoped helper Pod
- `tasks/check_subnet_still_referenced.yaml` - Resolves the CUDN's live `namespaceSelector` to determine whether anything besides the router Pod still references the subnet
- `tasks/patch_router_pod_network_status_removal.yaml` - Drops the subnet's entry from the router Pod's `network-status` annotation after a successful direct CNI DEL
- `tasks/recreate_router_pod_networks.yaml` - Shared recreation fallback for both attach and detach
- `tasks/create_security_group.yaml` - Delegates to `osac.templates.network_policy` (`create_security_group`)
- `tasks/delete_security_group.yaml` - Delegates to `osac.templates.network_policy` (`delete_security_group`)

## Usage

### Example: VirtualNetwork Provisioning

```yaml
- name: Create VirtualNetwork
  ansible.builtin.include_role:
    name: cudn_net
    tasks_from: create_virtual_network
  vars:
    virtual_network: "{{ osac_job_vars.resource }}"
    virtual_network_name: "{{ osac_job_vars.resource.metadata.name }}"
```

### Example: Subnet Provisioning

```yaml
- name: Create Subnet
  ansible.builtin.include_role:
    name: cudn_net
    tasks_from: create_subnet
  vars:
    subnet: "{{ osac_job_vars.resource }}"
    subnet_name: "{{ osac_job_vars.resource.metadata.name }}"
```
