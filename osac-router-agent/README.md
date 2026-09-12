# OSAC router agent

The router agent runs inside a Secondary VirtualNetwork's router pod. It reads
`/etc/osac-router-agent/config/config.json`, which is projected from the
`router-config` ConfigMap, and polls the file for updates. Kubernetes updates a
projected ConfigMap atomically, so the agent can notice changes without a pod
restart.

The current contract is:

```json
{
  "version": "opaque operator version",
  "gatewayIPs": [
    {"interface": "subnet-example", "address": "10.220.1.1/24"}
  ],
  "routes": [
    {"destination": "10.240.0.0/16", "interface": "subnet-example"},
    {"destination": "0.0.0.0/0", "gateway": "10.220.1.1", "interface": "subnet-example"}
  ]
}
```

The agent brings configured interfaces up, applies gateway addresses and routes,
and removes state that disappeared from the last successfully applied
configuration. If a referenced interface has not been injected yet, the
configuration remains pending and is retried on the next poll.

OVN port security is intentionally not handled here. AAP's `cudn_net` role
currently performs that temporary OVN northbound database reconciliation.
