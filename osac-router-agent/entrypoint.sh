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
# model), so there's nothing to watch yet. This image is where that agent will eventually
# live, once Epic 2's live-attach work needs one.
set -eu

CLUSTER_NET_IFACE="${ROUTER_POD_CLUSTER_NET_IFACE:-eth0}"

# masquerade rewrites the source to whatever address is currently assigned to
# CLUSTER_NET_IFACE -- equivalent to an explicit SNAT-to-own-pod-IP here without needing
# to look the address up (e.g. via the Downward API), since that's exactly this
# interface's own address. `nft`, not `iptables`: RHEL10/UBI10 dropped the iptables
# package in favor of nftables.
nft add table ip nat
nft -- add chain ip nat postrouting '{ type nat hook postrouting priority 100 ; }'
# `add table`/`add chain` are no-ops if already present, but `add rule` isn't -- guard
# against duplicate rules accumulating if the container restarts within the same pod
# (the network namespace, and any nft ruleset in it, outlives a container restart).
if ! nft list chain ip nat postrouting | grep -q "oifname \"${CLUSTER_NET_IFACE}\" masquerade"; then
  nft add rule ip nat postrouting oifname "${CLUSTER_NET_IFACE}" masquerade
fi

exec tail -f /dev/null
