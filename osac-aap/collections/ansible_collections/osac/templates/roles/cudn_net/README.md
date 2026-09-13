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
- Creates ClusterUserDefinedNetwork CR in the cluster
- Supports IPv4-only, IPv6-only, and dual-stack configurations
- NetworkClass determines the implementation strategy (cudn_net)
- One VirtualNetwork maps to one ClusterUserDefinedNetwork

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
router-agent ConfigMap. The live path patches the running Pod and waits for the
requested interface change in `k8s.v1.cni.cncf.io/network-status`. The initial
timeout is 60 seconds, configured by
`cudn_net_router_live_attach_timeout_seconds`; each operation has its own
deadline.

If the live patch fails or the expected network-status change is not observed
before the deadline, the role patches the Deployment template with the complete
desired attachment list and waits for the replacement Pod. This is the Epic 1
recreation fallback. Removal uses the same live-first/fallback sequence and
detaches before deleting the IPAMClaim and CUDN.

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
- Create ClusterUserDefinedNetwork CR with Layer2 topology
- Configure CIDR ranges from VirtualNetwork spec

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
- `tasks/patch_router_pod_networks.yaml` - Live mutation with recreation fallback
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
