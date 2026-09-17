#!/usr/bin/env bash
#
# Create a Primary VirtualNetwork (1 subnet) and a Secondary VirtualNetwork (2
# subnets), via the osac CLI, for exercising the secondary-virtualnetwork-router-pod
# feature end to end. Requires `osac login` to have already been run against the
# target cluster -- this script does not manage credentials.
#
# Optional env vars (defaults shown):
#   OSAC_BIN                  ./osac if present in the repo root, else "osac" on PATH
#   OSAC_TENANT               tenant for the test resources (required; do not use
#                             the reserved "shared" or "system" tenants)
#   NAME_SUFFIX                $(date +%s)  (appended to every resource name so
#                               repeat runs don't collide; set to a fixed value,
#                               e.g. "1", for stable/repeatable resource names)
#   WAIT_FOR_READY             true  (poll each resource until READY/FAILED before
#                               moving on; set false to fire-and-forget)
#   WAIT_TIMEOUT_SECONDS       300
#   REUSE_EXISTING             false (reuse matching resources instead of failing)
#   PRIMARY_VN_CIDR            10.210.0.0/16
#   PRIMARY_SUBNET_CIDR        10.210.1.0/24
#   SECONDARY_VN_CIDR          10.240.0.0/16
#   SECONDARY_SUBNET1_CIDR     10.240.1.0/24
#   SECONDARY_SUBNET2_CIDR     10.240.2.0/24
#   PRIMARY_VN_NAME            primary-vn-${NAME_SUFFIX}
#   PRIMARY_SUBNET_NAME        primary-subnet-${NAME_SUFFIX}
#   SECONDARY_VN_NAME          secondary-vn-${NAME_SUFFIX}
#   SECONDARY_SUBNET1_NAME     secondary-subnet1-${NAME_SUFFIX}
#   SECONDARY_SUBNET2_NAME     secondary-subnet2-${NAME_SUFFIX}
#
# Example:
#   ./hack/create-test-virtual-networks.sh
#   NAME_SUFFIX=1 WAIT_FOR_READY=false ./hack/create-test-virtual-networks.sh

set -euo pipefail

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [[ -z "${OSAC_BIN:-}" ]]; then
	if [[ -x "${REPO_ROOT}/osac" ]]; then
		OSAC_BIN="${REPO_ROOT}/osac"
	else
		OSAC_BIN="osac"
	fi
fi
command -v "$OSAC_BIN" >/dev/null 2>&1 || die "osac CLI not found/executable at '$OSAC_BIN' -- build it first (go build -o osac ./cmd/osac in fulfillment-service) or set OSAC_BIN"
command -v jq >/dev/null 2>&1 || die "jq is required to validate existing network resources"
command -v python3 >/dev/null 2>&1 || die "python3 is required to validate subnet CIDRs"

NAME_SUFFIX="${NAME_SUFFIX:-$(date +%s)}"
WAIT_FOR_READY="${WAIT_FOR_READY:-true}"
WAIT_TIMEOUT_SECONDS="${WAIT_TIMEOUT_SECONDS:-300}"
REUSE_EXISTING="${REUSE_EXISTING:-false}"
OSAC_TENANT="${OSAC_TENANT:-}"

[[ -n "$OSAC_TENANT" ]] || die "OSAC_TENANT is required; set it to an existing tenant such as 'test-tenant'"
# Resolve the tenant from the authenticated user's organization claim. Do not
# force --tenant here: an explicit tenant filter hides shared catalog resources
# (for example the ComputeInstanceTemplate and InstanceType) from tenant users,
# and tenant admins are not necessarily allowed to Get the public Tenant object.
# The preflight get below still verifies that the login is usable.
OSAC_GLOBAL_ARGS=()

PRIMARY_VN_CIDR="${PRIMARY_VN_CIDR:-10.210.0.0/16}"
PRIMARY_SUBNET_CIDR="${PRIMARY_SUBNET_CIDR:-10.210.1.0/24}"
SECONDARY_VN_CIDR="${SECONDARY_VN_CIDR:-10.240.0.0/16}"
SECONDARY_SUBNET1_CIDR="${SECONDARY_SUBNET1_CIDR:-10.240.1.0/24}"
SECONDARY_SUBNET2_CIDR="${SECONDARY_SUBNET2_CIDR:-10.240.2.0/24}"

PRIMARY_VN_NAME="${PRIMARY_VN_NAME:-primary-vn-${NAME_SUFFIX}}"
PRIMARY_SUBNET_NAME="${PRIMARY_SUBNET_NAME:-primary-subnet-${NAME_SUFFIX}}"
SECONDARY_VN_NAME="${SECONDARY_VN_NAME:-secondary-vn-${NAME_SUFFIX}}"
SECONDARY_SUBNET1_NAME="${SECONDARY_SUBNET1_NAME:-secondary-subnet1-${NAME_SUFFIX}}"
SECONDARY_SUBNET2_NAME="${SECONDARY_SUBNET2_NAME:-secondary-subnet2-${NAME_SUFFIX}}"

[[ "$REUSE_EXISTING" == "true" || "$REUSE_EXISTING" == "false" ]] || \
	die "REUSE_EXISTING must be true or false"

# Validate the topology before making API calls. In particular, both secondary
# subnets must be inside the same secondary VirtualNetwork.
python3 - "$PRIMARY_VN_CIDR" "$PRIMARY_SUBNET_CIDR" "$SECONDARY_VN_CIDR" \
	"$SECONDARY_SUBNET1_CIDR" "$SECONDARY_SUBNET2_CIDR" <<'PY'
import ipaddress
import sys

primary_vn, primary_subnet, secondary_vn, secondary_subnet1, secondary_subnet2 = map(
    ipaddress.ip_network, sys.argv[1:]
)
if not primary_subnet.subnet_of(primary_vn):
    raise SystemExit(f"primary subnet {primary_subnet} is outside primary VN {primary_vn}")
for subnet in (secondary_subnet1, secondary_subnet2):
    if not subnet.subnet_of(secondary_vn):
        raise SystemExit(f"secondary subnet {subnet} is outside secondary VN {secondary_vn}")
if secondary_subnet1.overlaps(secondary_subnet2):
    raise SystemExit(f"secondary subnets {secondary_subnet1} and {secondary_subnet2} overlap")
PY

log "Configuration"
cat <<EOF
  OSAC_BIN:               $OSAC_BIN
  OSAC_TENANT:            $OSAC_TENANT
  NAME_SUFFIX:            $NAME_SUFFIX
  WAIT_FOR_READY:         $WAIT_FOR_READY
  REUSE_EXISTING:         $REUSE_EXISTING

  Primary VN:             $PRIMARY_VN_NAME ($PRIMARY_VN_CIDR)
    subnet:               $PRIMARY_SUBNET_NAME ($PRIMARY_SUBNET_CIDR)

  Secondary VN:           $SECONDARY_VN_NAME ($SECONDARY_VN_CIDR)
    subnet 1:             $SECONDARY_SUBNET1_NAME ($SECONDARY_SUBNET1_CIDR)
    subnet 2:             $SECONDARY_SUBNET2_NAME ($SECONDARY_SUBNET2_CIDR)
EOF

if ! "$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" get virtualnetwork >/dev/null 2>&1; then
	die "'$OSAC_BIN --tenant $OSAC_TENANT get virtualnetwork' failed -- run '$OSAC_BIN login ...' and verify that tenant exists"
fi

# wait_for_ready <kind> <name>
# Polls `osac describe <kind> <name>` until its State reaches READY or FAILED, or
# WAIT_TIMEOUT_SECONDS elapses. No-op (besides a log line) when WAIT_FOR_READY=false.
wait_for_ready() {
	local kind="$1" name="$2"
	if [[ "$WAIT_FOR_READY" != "true" ]]; then
		log "Not waiting for $kind '$name' to become Ready (WAIT_FOR_READY=false)"
		return 0
	fi
	log "Waiting for $kind '$name' to become Ready (timeout ${WAIT_TIMEOUT_SECONDS}s)"
	local waited=0
	local state=""
	while (( waited < WAIT_TIMEOUT_SECONDS )); do
		# A resource can briefly be unavailable while the API/backend updates its
		# status.  Do not let set -e turn that transient query failure into a
		# script failure; the next poll can observe the resource again.
		if ! state="$("$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" describe "$kind" "$name" 2>/dev/null | awk -F':[[:space:]]*' '/^State:/ {print $2}')"; then
			state=""
			echo "  ...unable to query $kind '$name'; retrying (waited ${waited}s)"
			sleep 10
			waited=$(( waited + 10 ))
			continue
		fi
		case "$state" in
			READY)
				echo "  $kind '$name' is READY"
				return 0
				;;
			FAILED)
				"$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" describe "$kind" "$name" 2>/dev/null | sed 's/^/  /'
				die "$kind '$name' reached FAILED state"
				;;
			*)
				echo "  ...$kind '$name' state: ${state:-<unknown>} (waited ${waited}s)"
				;;
		esac
		sleep 10
		waited=$(( waited + 10 ))
	done
	die "$kind '$name' did not become Ready within ${WAIT_TIMEOUT_SECONDS}s (last state: ${state:-<unknown>})"
}

ensure_virtualnetwork() {
	local name="$1" cidr="$2" networking_type="$3"
	if "$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" describe virtualnetwork "$name" >/dev/null 2>&1; then
		[[ "$REUSE_EXISTING" == "true" ]] || die "virtualnetwork '$name' already exists (set REUSE_EXISTING=true to validate and reuse it)"
		local json
		json="$($OSAC_BIN "${OSAC_GLOBAL_ARGS[@]}" get virtualnetwork "$name" -o json)" || \
			die "failed to inspect existing virtualnetwork '$name'"
		jq -e --arg cidr "$cidr" --arg type "$networking_type" '
			.spec.ipv4_cidr == $cidr and
			(if $type == "Secondary" then
				.spec.networking_type == "VIRTUAL_NETWORK_NETWORKING_TYPE_SECONDARY"
			 else
				(.spec.networking_type // "VIRTUAL_NETWORK_NETWORKING_TYPE_DEFAULT") != "VIRTUAL_NETWORK_NETWORKING_TYPE_SECONDARY"
			 end)
		' <<<"$json" >/dev/null || \
			die "existing virtualnetwork '$name' does not match CIDR '$cidr' or networking type '$networking_type'"
		log "Reusing existing VirtualNetwork: $name"
		wait_for_ready virtualnetwork "$name"
		return 0
	fi

	log "Creating $networking_type VirtualNetwork: $name"
	"$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" create virtualnetwork --name "$name" --ipv4-cidr "$cidr" --networking-type "$networking_type"
	wait_for_ready virtualnetwork "$name"
}

ensure_subnet() {
	local name="$1" parent="$2" cidr="$3"
	if "$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" describe subnet "$name" >/dev/null 2>&1; then
		[[ "$REUSE_EXISTING" == "true" ]] || die "subnet '$name' already exists (set REUSE_EXISTING=true to validate and reuse it)"
		local json
		json="$($OSAC_BIN "${OSAC_GLOBAL_ARGS[@]}" get subnet "$name" -o json)" || \
			die "failed to inspect existing subnet '$name'"
		jq -e --arg cidr "$cidr" --arg parent "$parent" \
			'.spec.ipv4_cidr == $cidr and .spec.virtual_network.name == $parent' \
			<<<"$json" >/dev/null || \
			die "existing subnet '$name' does not match CIDR '$cidr' or parent '$parent'"
		log "Reusing existing subnet: $name"
		wait_for_ready subnet "$name"
		return 0
	fi

	log "Creating subnet: $name"
	"$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" create subnet --name "$name" --virtual-network "$parent" --ipv4-cidr "$cidr"
	wait_for_ready subnet "$name"
}

# ─── Primary VirtualNetwork + 1 subnet ──────────────────────────────────────────
ensure_virtualnetwork "$PRIMARY_VN_NAME" "$PRIMARY_VN_CIDR" Primary
ensure_subnet "$PRIMARY_SUBNET_NAME" "$PRIMARY_VN_NAME" "$PRIMARY_SUBNET_CIDR"

# ─── Secondary VirtualNetwork + 2 subnets ───────────────────────────────────────
ensure_virtualnetwork "$SECONDARY_VN_NAME" "$SECONDARY_VN_CIDR" Secondary
ensure_subnet "$SECONDARY_SUBNET1_NAME" "$SECONDARY_VN_NAME" "$SECONDARY_SUBNET1_CIDR"
ensure_subnet "$SECONDARY_SUBNET2_NAME" "$SECONDARY_VN_NAME" "$SECONDARY_SUBNET2_CIDR"

log "Done -- final state"
for kind_name in "virtualnetwork $PRIMARY_VN_NAME" "subnet $PRIMARY_SUBNET_NAME" \
	"virtualnetwork $SECONDARY_VN_NAME" "subnet $SECONDARY_SUBNET1_NAME" "subnet $SECONDARY_SUBNET2_NAME"; do
	read -r kind name <<<"$kind_name"
	echo "--- $kind $name ---"
	"$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" describe "$kind" "$name" | sed 's/^/  /'
done
