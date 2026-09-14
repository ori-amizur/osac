"""Filters for turning fabric-owned address ranges into CNI CIDRs."""

import ipaddress
from collections.abc import Iterable, Mapping

from ansible.errors import AnsibleFilterError


def _as_network(value: object) -> ipaddress.IPv4Network:
    try:
        if isinstance(value, str) and "/" not in value:
            return ipaddress.ip_network(f"{value}/32", strict=False)
        network = ipaddress.ip_network(str(value), strict=False)
        if not isinstance(network, ipaddress.IPv4Network):
            raise ValueError("only IPv4 ranges are supported")
        return network
    except ValueError as exc:
        raise AnsibleFilterError(f"invalid IPv4 range {value!r}: {exc}") from exc


def summarize_ip_ranges(values: Iterable[object]) -> list[str]:
    """Return a minimal, deterministic CIDR cover for addresses and ranges.

    Items may be IPv4 addresses, CIDRs, or mappings with ``start`` and ``end``
    IPv4 address keys. The result is suitable for CUDN ``reservedSubnets``.
    """

    networks: list[ipaddress.IPv4Network] = []
    for value in values or []:
        if isinstance(value, Mapping):
            if "start" not in value or "end" not in value:
                raise AnsibleFilterError(
                    "an IP range mapping must contain both 'start' and 'end'"
                )
            try:
                start = ipaddress.ip_address(str(value["start"]))
                end = ipaddress.ip_address(str(value["end"]))
            except ValueError as exc:
                raise AnsibleFilterError(f"invalid IPv4 range {value!r}: {exc}") from exc
            if not isinstance(start, ipaddress.IPv4Address) or not isinstance(
                end, ipaddress.IPv4Address
            ):
                raise AnsibleFilterError(f"only IPv4 ranges are supported: {value!r}")
            if int(start) > int(end):
                raise AnsibleFilterError(f"range start is after end: {value!r}")
            networks.extend(ipaddress.summarize_address_range(start, end))
            continue
        networks.append(_as_network(value))

    return [str(network) for network in ipaddress.collapse_addresses(networks)]


class FilterModule:
    def filters(self) -> dict[str, object]:
        return {"summarize_ip_ranges": summarize_ip_ranges}
