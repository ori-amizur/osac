import ipaddress

import pytest
from ip_ranges import cidr_contains, cidr_overlaps, summarize_ip_ranges


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


def test_detects_overlapping_cidrs() -> None:
    assert cidr_overlaps("169.254.240.0/24", "169.254.240.128/25")
    assert not cidr_overlaps("169.254.240.0/24", "169.254.241.0/24")
    assert not cidr_overlaps("169.254.240.0/24", "")


def test_checks_that_reserved_range_is_inside_transit_cidr() -> None:
    assert cidr_contains("169.254.240.0/24", "169.254.240.1/32")
    assert cidr_contains(
        "169.254.240.0/24",
        {"start": "169.254.240.8", "end": "169.254.240.15"},
    )
    assert not cidr_contains("169.254.240.0/24", "169.254.241.1/32")
