import ipaddress

import pytest
from ip_ranges import summarize_ip_ranges


def test_summarizes_gateway_and_dhcp_ranges_without_overreserving_the_network() -> None:
    result = summarize_ip_ranges(
        [
            "10.241.1.1",
            {"start": "10.241.1.8", "end": "10.241.1.15"},
        ]
    )

    assert result == ["10.241.1.1/32", "10.241.1.8/29"]


def test_collapses_overlapping_and_adjacent_cidrs() -> None:
    result = summarize_ip_ranges(["10.241.1.0/30", "10.241.1.4", "10.241.1.5"])

    assert result == ["10.241.1.0/30", "10.241.1.4/31"]


def test_rejects_reversed_ranges() -> None:
    with pytest.raises(Exception, match="range start is after end"):
        summarize_ip_ranges([{"start": "10.241.1.9", "end": "10.241.1.8"}])


def test_result_is_valid_ipv4_cidr() -> None:
    result = summarize_ip_ranges(["10.241.1.7"])

    assert ipaddress.ip_network(result[0]) == ipaddress.ip_network("10.241.1.7/32")
