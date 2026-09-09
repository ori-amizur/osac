#!/bin/sh
# One-shot router pod setup: SNAT egress traffic leaving via the cluster-network interface
# to the pod's own IP, unconditionally -- see design.md's "Cluster-network egress SNAT is
# unconditional, not destination-dependent" note. This intentionally does not distinguish
# cluster-internal from external destinations; OVN's own Service load-balancing and the
# node's pre-existing external-masquerade rule already handle that downstream.
#
# net.ipv4.ip_forward is NOT set here: confirmed (via direct testing) that `/proc/sys` is
# mounted read-only regardless of NET_ADMIN, so an in-container `sysctl -w` fails with
# "permission denied" even with that capability. It must be set declaratively via the pod's
# own securityContext.sysctls instead (Kubernetes' safe/namespaced-sysctls mechanism,
# applied by the container runtime before this entrypoint ever runs) -- see
# create_virtual_network.yaml's router pod Deployment spec.
#
# This is a one-shot script, not the ConfigMap-watching agent design.md describes -- the
# router pod is fully recreated on any subnet change (Stories 1.02/1.03/1.05's baseline
# model), so there is nothing to watch yet. The gateway mapping is supplied by AAP from
# the persistent IPAMClaim status, and is keyed by the explicit Multus interface name.
set -eu

CLUSTER_NET_IFACE="${ROUTER_POD_CLUSTER_NET_IFACE:-eth0}"
CLUSTER_NET_IP="${ROUTER_POD_CLUSTER_NET_IP:?ROUTER_POD_CLUSTER_NET_IP must be set from the Downward API}"
GATEWAY_IPS="${ROUTER_POD_GATEWAY_IPS:-}"

# OVN assigns a normal address to a Multus interface first. The IPAMClaim reserves the
# well-known .1 address, but does not itself configure that address inside the pod. Add
# the reserved address as a second address on the interface so the router really owns the
# gateway that VMs use for ARP and forwarding. Entries are interface=address/prefix,
# comma-separated, for example: subnet-dbfwf=10.220.1.1/24,subnet-gv6xz=10.220.2.1/24.
old_ifs="$IFS"
IFS=,
for gateway in $GATEWAY_IPS; do
  IFS="$old_ifs"
  gateway_iface=${gateway%%=*}
  gateway_address=${gateway#*=}
  if [ -z "$gateway_iface" ] || [ -z "$gateway_address" ] || [ "$gateway_iface" = "$gateway" ]; then
    echo "invalid ROUTER_POD_GATEWAY_IPS entry: $gateway" >&2
    exit 1
  fi
  ip link show dev "$gateway_iface" >/dev/null
  ip link set dev "$gateway_iface" up
  ip addr replace "$gateway_address" dev "$gateway_iface"
  IFS=,
done
IFS="$old_ifs"

# Use explicit SNAT rather than nft's masquerade expression. The OpenShift node used for
# this deployment supports the SNAT expression but returns ENOENT for masquerade, while
# the Downward API gives us the same cluster-network pod IP that masquerade would select.
# `nft`, not `iptables`: RHEL10/UBI10 dropped the iptables package in favor of nftables.
nft add table ip nat
nft -- add chain ip nat postrouting '{ type nat hook postrouting priority 100 ; }'
# `add table`/`add chain` are no-ops if already present, but `add rule` isn't -- guard
# against duplicate rules accumulating if the container restarts within the same pod
# (the network namespace, and any nft ruleset in it, outlives a container restart).
if ! nft list chain ip nat postrouting | grep -q "oifname \"${CLUSTER_NET_IFACE}\" snat to"; then
  nft add rule ip nat postrouting oifname "\"${CLUSTER_NET_IFACE}\"" snat to "${CLUSTER_NET_IP}"
fi

exec tail -f /dev/null
