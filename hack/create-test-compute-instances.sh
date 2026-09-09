#!/usr/bin/env bash
#
# Create three ComputeInstances for exercising multi-network VM attachments:
#
#   1. primary subnet only
#   2. primary subnet plus the first secondary subnet
#   3. the second secondary subnet only
#
# Requires an existing tenant-visible InstanceType and DiskImage. The network
# references default to the resources created by create-test-virtual-networks.sh
# for NAME_SUFFIX, but can be overridden with the *_SUBNET variables below.
# Subnet names are resolved to IDs before they are passed to the ComputeInstance
# API, whose --network-attachment field requires IDs.
#
# Optional env vars (defaults shown):
#   OSAC_BIN                  ./osac if present in the repo root, else "osac"
#   OSAC_TENANT               tenant for the test resources (required)
#   NAME_SUFFIX               $(date +%s)
#   PRIMARY_SUBNET            primary-subnet-${NAME_SUFFIX}
#   SECONDARY_SUBNET1         secondary-subnet1-${NAME_SUFFIX}
#   SECONDARY_SUBNET2         secondary-subnet2-${NAME_SUFFIX}
#   VM1_NAME                  primary-only-vm-${NAME_SUFFIX}
#   VM2_NAME                  primary-secondary-vm-${NAME_SUFFIX}
#   VM3_NAME                  secondary2-only-vm-${NAME_SUFFIX}
#   TEMPLATE                  ocp-virt-vm
#   INSTANCE_TYPE             required; e.g. small
#   DISK_IMAGE                required; e.g. fedora-41
#   BOOT_DISK_SIZE            10
#   BOOT_DISK_STORAGE_TIER    empty (use the platform default when empty)
#   RUN_STRATEGY              Always
#   SSH_KEY_PATH              ${TMPDIR:-/tmp}/osac-${OSAC_TENANT}-${NAME_SUFFIX}.ed25519
#   USER_DATA                  empty
#   EXTERNAL_IP_ATTACHMENT     true
#   REUSE_EXISTING             true (reuse an existing VM with the requested name)
#   WAIT_FOR_RUNNING           true
#   WAIT_TIMEOUT_SECONDS      600
#
# Example:
#   OSAC_TENANT=test-tenant \
#   NAME_SUFFIX=1788980240 \
#   INSTANCE_TYPE=small \
#   DISK_IMAGE=fedora-41 \
#   ./hack/create-test-compute-instances.sh

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
[[ -x "$OSAC_BIN" || "$(command -v "$OSAC_BIN" 2>/dev/null || true)" ]] || \
	die "osac CLI not found/executable at '$OSAC_BIN' -- build it first or set OSAC_BIN"
command -v jq >/dev/null 2>&1 || die "jq is required to resolve subnet names to IDs"

OSAC_TENANT="${OSAC_TENANT:-}"
[[ -n "$OSAC_TENANT" ]] || die "OSAC_TENANT is required; set it to an existing tenant such as 'test-tenant'"
[[ "$OSAC_TENANT" != "shared" && "$OSAC_TENANT" != "system" ]] || \
	die "OSAC_TENANT must be a tenant-scoped test tenant, not '$OSAC_TENANT'"
OSAC_GLOBAL_ARGS=(--tenant "$OSAC_TENANT")

NAME_SUFFIX="${NAME_SUFFIX:-$(date +%s)}"
PRIMARY_SUBNET="${PRIMARY_SUBNET:-primary-subnet-${NAME_SUFFIX}}"
SECONDARY_SUBNET1="${SECONDARY_SUBNET1:-secondary-subnet1-${NAME_SUFFIX}}"
SECONDARY_SUBNET2="${SECONDARY_SUBNET2:-secondary-subnet2-${NAME_SUFFIX}}"

VM1_NAME="${VM1_NAME:-primary-only-vm-${NAME_SUFFIX}}"
VM2_NAME="${VM2_NAME:-primary-secondary-vm-${NAME_SUFFIX}}"
VM3_NAME="${VM3_NAME:-secondary2-only-vm-${NAME_SUFFIX}}"

TEMPLATE="${TEMPLATE:-ocp-virt-vm}"
INSTANCE_TYPE="${INSTANCE_TYPE:-}"
DISK_IMAGE="${DISK_IMAGE:-}"
BOOT_DISK_SIZE="${BOOT_DISK_SIZE:-10}"
BOOT_DISK_STORAGE_TIER="${BOOT_DISK_STORAGE_TIER:-}"
RUN_STRATEGY="${RUN_STRATEGY:-Always}"
SSH_KEY_PATH="${SSH_KEY_PATH:-${TMPDIR:-/tmp}/osac-${OSAC_TENANT}-${NAME_SUFFIX}.ed25519}"
USER_DATA="${USER_DATA:-}"
EXTERNAL_IP_ATTACHMENT="${EXTERNAL_IP_ATTACHMENT:-true}"
REUSE_EXISTING="${REUSE_EXISTING:-true}"
WAIT_FOR_RUNNING="${WAIT_FOR_RUNNING:-true}"
WAIT_TIMEOUT_SECONDS="${WAIT_TIMEOUT_SECONDS:-600}"

[[ -n "$INSTANCE_TYPE" ]] || die "INSTANCE_TYPE is required; set it to a tenant-visible InstanceType"
[[ -n "$DISK_IMAGE" ]] || die "DISK_IMAGE is required; set it to a tenant-visible DiskImage"
[[ "$EXTERNAL_IP_ATTACHMENT" == "true" || "$EXTERNAL_IP_ATTACHMENT" == "false" ]] || \
	die "EXTERNAL_IP_ATTACHMENT must be true or false"
[[ "$REUSE_EXISTING" == "true" || "$REUSE_EXISTING" == "false" ]] || \
	die "REUSE_EXISTING must be true or false"
[[ "$WAIT_FOR_RUNNING" == "true" || "$WAIT_FOR_RUNNING" == "false" ]] || \
	die "WAIT_FOR_RUNNING must be true or false"

ensure_ssh_keypair() {
	command -v ssh-keygen >/dev/null 2>&1 || die "ssh-keygen is required to generate the VM SSH key pair"
	local key_dir
	key_dir="$(dirname "${SSH_KEY_PATH}")"
	if [[ ! -d "${key_dir}" ]]; then
		mkdir -p "${key_dir}"
		chmod 700 "${key_dir}"
	fi

	if [[ -e "${SSH_KEY_PATH}" || -e "${SSH_KEY_PATH}.pub" ]]; then
		[[ -f "${SSH_KEY_PATH}" ]] || die "SSH_KEY_PATH exists but is not a regular file: ${SSH_KEY_PATH}"
		if [[ ! -f "${SSH_KEY_PATH}.pub" ]]; then
			ssh-keygen -y -f "${SSH_KEY_PATH}" > "${SSH_KEY_PATH}.pub" || \
				die "failed to derive SSH public key from ${SSH_KEY_PATH}"
		fi
	else
		umask 077
		ssh-keygen -q -t ed25519 -N "" \
			-C "osac-${OSAC_TENANT}-${NAME_SUFFIX}" \
			-f "${SSH_KEY_PATH}" || die "failed to generate SSH key pair at ${SSH_KEY_PATH}"
	fi

	chmod 600 "${SSH_KEY_PATH}"
	chmod 644 "${SSH_KEY_PATH}.pub"
	SSH_PUBLIC_KEY="$(tr -d '\n' < "${SSH_KEY_PATH}.pub")"
	[[ -n "${SSH_PUBLIC_KEY}" ]] || die "generated SSH public key is empty"
}

ensure_ssh_keypair

log "Configuration"
cat <<EOF
  OSAC_BIN:                 $OSAC_BIN
  OSAC_TENANT:              $OSAC_TENANT
  NAME_SUFFIX:              $NAME_SUFFIX
  TEMPLATE:                 $TEMPLATE
  INSTANCE_TYPE:            $INSTANCE_TYPE
  DISK_IMAGE:               $DISK_IMAGE
  SSH_PRIVATE_KEY:          $SSH_KEY_PATH
  SSH_PUBLIC_KEY:           $SSH_KEY_PATH.pub
  EXTERNAL_IP_ATTACHMENT:   $EXTERNAL_IP_ATTACHMENT
  REUSE_EXISTING:            $REUSE_EXISTING
  WAIT_FOR_RUNNING:         $WAIT_FOR_RUNNING

  VM 1:                     $VM1_NAME
    network:                $PRIMARY_SUBNET

  VM 2:                     $VM2_NAME
    networks:               $PRIMARY_SUBNET, $SECONDARY_SUBNET1

  VM 3:                     $VM3_NAME
    network:                $SECONDARY_SUBNET2
EOF

describe_state() {
	local kind="$1" ref="$2"
	"$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" describe "$kind" "$ref" 2>/dev/null |
		awk -F':[[:space:]]*' '/^State:/ {print $2; exit}' || true
}

require_ready() {
	local kind="$1" ref="$2" state
	state="$(describe_state "$kind" "$ref")"
	case "$state" in
		READY|ACTIVE) ;;
		*) die "$kind '$ref' is not ready (state: ${state:-<unknown>})" ;;
	esac
}

require_object() {
	local kind="$1" ref="$2"
	if ! "$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" describe "$kind" "$ref" >/dev/null 2>&1; then
		die "$kind '$ref' was not found or is not visible in tenant '$OSAC_TENANT'"
	fi
}

resolve_subnet_id() {
	local ref="$1" id
	id="$("$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" get subnet "$ref" -o json | jq -r '.id // empty')" || \
		die "failed to resolve subnet '$ref' in tenant '$OSAC_TENANT'"
	[[ -n "$id" ]] || die "subnet '$ref' has no ID"
	printf '%s\n' "$id"
}

wait_for_running() {
	local name="$1"
	if [[ "$WAIT_FOR_RUNNING" != "true" ]]; then
		log "Not waiting for ComputeInstance '$name' to become Running (WAIT_FOR_RUNNING=false)"
		return 0
	fi

	log "Waiting for ComputeInstance '$name' to become Running (timeout ${WAIT_TIMEOUT_SECONDS}s)"
	local waited=0 state
	while (( waited < WAIT_TIMEOUT_SECONDS )); do
		state="$(describe_state computeinstance "$name")"
		case "$state" in
			RUNNING|READY)
				echo "  ComputeInstance '$name' is $state"
				return 0
				;;
			FAILED|ERROR|DELETING)
				"$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" describe computeinstance "$name" 2>/dev/null | sed 's/^/  /'
				die "ComputeInstance '$name' reached $state state"
				;;
			*)
				echo "  ...ComputeInstance '$name' state: ${state:-<unknown>} (waited ${waited}s)"
				;;
		esac
		sleep 10
		waited=$((waited + 10))
	done
	die "ComputeInstance '$name' did not become Running within ${WAIT_TIMEOUT_SECONDS}s (last state: ${state:-<unknown>})"
}

create_vm() {
	local name="$1"
	shift
	local -a args=(
		"${OSAC_GLOBAL_ARGS[@]}"
		create computeinstance
		--name "$name"
		--template "$TEMPLATE"
		--instance-type "$INSTANCE_TYPE"
		--disk-image "$DISK_IMAGE"
		--boot-disk-size "$BOOT_DISK_SIZE"
		--run-strategy "$RUN_STRATEGY"
	)

	if [[ -n "$BOOT_DISK_STORAGE_TIER" ]]; then
		args+=(--boot-disk-storage-tier "$BOOT_DISK_STORAGE_TIER")
	fi
	args+=(--ssh-public-key "$SSH_PUBLIC_KEY")
	if [[ "$EXTERNAL_IP_ATTACHMENT" == "true" ]]; then
		args+=(--external-ip-attachment)
	fi
	if [[ -n "$USER_DATA" ]]; then
		args+=(--user-data "$USER_DATA")
	fi
	while (($#)); do
		args+=(--network-attachment "$1")
		shift
	done

	"$OSAC_BIN" "${args[@]}"
}

create_or_reuse_vm() {
	local name="$1"
	shift
	if [[ "$REUSE_EXISTING" == "true" ]] && "$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" describe computeinstance "$name" >/dev/null 2>&1; then
		log "Reusing existing ComputeInstance: $name"
	else
		create_vm "$name" "$@"
	fi
	wait_for_running "$name"
}

require_object computeinstancetemplate "$TEMPLATE"
require_object instancetype "$INSTANCE_TYPE"
require_object diskimage "$DISK_IMAGE"
require_ready subnet "$PRIMARY_SUBNET"
require_ready subnet "$SECONDARY_SUBNET1"
require_ready subnet "$SECONDARY_SUBNET2"

# The CLI accepts subnet names for lookup, but the API request uses a local
# subnet reference whose ID field must contain the resource ID.
PRIMARY_SUBNET_ID="$(resolve_subnet_id "$PRIMARY_SUBNET")"
SECONDARY_SUBNET1_ID="$(resolve_subnet_id "$SECONDARY_SUBNET1")"
SECONDARY_SUBNET2_ID="$(resolve_subnet_id "$SECONDARY_SUBNET2")"

log "Creating primary-only ComputeInstance: $VM1_NAME"
create_or_reuse_vm "$VM1_NAME" "$PRIMARY_SUBNET_ID"

log "Creating primary + secondary ComputeInstance: $VM2_NAME"
create_or_reuse_vm "$VM2_NAME" "$PRIMARY_SUBNET_ID" "$SECONDARY_SUBNET1_ID"

log "Creating second-secondary-only ComputeInstance: $VM3_NAME"
create_or_reuse_vm "$VM3_NAME" "$SECONDARY_SUBNET2_ID"

log "Done -- final state"
for name in "$VM1_NAME" "$VM2_NAME" "$VM3_NAME"; do
	echo "--- computeinstance $name ---"
	"$OSAC_BIN" "${OSAC_GLOBAL_ARGS[@]}" describe computeinstance "$name" | sed 's/^/  /'
done
