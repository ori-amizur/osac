#!/usr/bin/env bash
#
# Recover AAP from the gateway/controller "cryptography.fernet.InvalidToken"
# crash loop caused by an encryption-secret/persisted-data mismatch.
#
# Root cause: the AnsibleAutomationPlatform CR doesn't specify
# spec.db_fields_encryption_secret / spec.controller.secret_key_secret, so the
# AAP operator auto-generates its own secret under each of those exact names --
# owned by the CR, so it gets garbage-collected on `uninstall-osac` and
# freshly regenerated on the next install. The gateway/controller Postgres
# data lives in a StatefulSet PVC, which Kubernetes never deletes on its own.
# Every reinstall that doesn't pin the CR to a stable secret therefore risks
# pairing old, still-encrypted data with a brand-new key that cannot decrypt it.
#
# This script is idempotent and safe to run any time AAP looks stuck:
#   1. Ensures stable encryption secrets exist under the names the operator
#      would otherwise auto-generate (creates them only if missing).
#   2. Patches the AnsibleAutomationPlatform CR to explicitly reference them
#      (retries -- this can race the operator's own reconciliation).
#   3. Resets the gateway/automationcontroller Postgres schemas, since any
#      data already encrypted with a previous, now-orphaned key can never be
#      decrypted by the (possibly different) stable secret -- only needed
#      once per genuine mismatch, but safe to re-run (a schema with nothing
#      in it is reset to nothing).
#   4. Force-deletes any already-crash-looping gateway/controller pods so
#      they retry immediately instead of waiting out CrashLoopBackOff.
#
# Optional env vars (defaults shown):
#   NAMESPACE            osac
#   AAP_CR_NAME          osac-aap
#   AAP_PG_POD           osac-aap-postgres-15-0
#   RESET_AAP_DB         true   (set false to only fix the CR reference, e.g.
#                                 if you know the data was never actually
#                                 encrypted with a mismatched key)
#
# Example:
#   ./hack/fix-aap-encryption-mismatch.sh
#   NAMESPACE=osac RESET_AAP_DB=false ./hack/fix-aap-encryption-mismatch.sh

set -euo pipefail

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

NAMESPACE="${NAMESPACE:-osac}"
AAP_CR_NAME="${AAP_CR_NAME:-osac-aap}"
AAP_PG_POD="${AAP_PG_POD:-osac-aap-postgres-15-0}"
RESET_AAP_DB="${RESET_AAP_DB:-true}"

AAP_GATEWAY_SECRET="osac-aap-db-fields-encryption-secret"
AAP_CONTROLLER_SECRET="osac-aap-controller-secret-key"

command -v kubectl >/dev/null 2>&1 || die "kubectl not found in PATH"
kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || die "namespace '$NAMESPACE' not found"

# ─── 1. Ensure stable encryption secrets exist ─────────────────────────────
log "Ensuring stable AAP encryption secrets exist in namespace $NAMESPACE"
for aap_secret in "$AAP_GATEWAY_SECRET" "$AAP_CONTROLLER_SECRET"; do
	if ! kubectl get secret "$aap_secret" -n "$NAMESPACE" >/dev/null 2>&1; then
		log "Creating stable AAP encryption secret: $aap_secret"
		kubectl create secret generic "$aap_secret" -n "$NAMESPACE" \
			--from-literal=secret_key="$(tr -dc 'A-Za-z0-9' </dev/urandom | head -c 50)"
	else
		echo "  $aap_secret already exists, reusing"
	fi
done

# ─── 2. Patch the AnsibleAutomationPlatform CR to reference them ───────────
log "Pointing $AAP_CR_NAME at the stable encryption secrets"
aap_patch=$(printf '{"spec":{"db_fields_encryption_secret":"%s","controller":{"secret_key_secret":"%s"}}}' \
	"$AAP_GATEWAY_SECRET" "$AAP_CONTROLLER_SECRET")
aap_patch_ok="false"
for _ in $(seq 1 30); do
	if kubectl patch ansibleautomationplatforms.aap.ansible.com "$AAP_CR_NAME" -n "$NAMESPACE" \
		--type=merge -p "$aap_patch" >/dev/null 2>&1; then
		aap_patch_ok="true"
		break
	fi
	sleep 2
done
[[ "$aap_patch_ok" == "true" ]] || die "could not patch $AAP_CR_NAME (not found in $NAMESPACE after 60s)"
kubectl get ansibleautomationplatforms.aap.ansible.com "$AAP_CR_NAME" -n "$NAMESPACE" \
	-o jsonpath='  db_fields_encryption_secret: {.spec.db_fields_encryption_secret}{"\n"}  controller.secret_key_secret: {.spec.controller.secret_key_secret}{"\n"}'

# ─── 3. Reset gateway/automationcontroller schemas ─────────────────────────
if [[ "$RESET_AAP_DB" == "true" ]]; then
	log "Resetting gateway/automationcontroller databases (RESET_AAP_DB=true)"
	for _ in $(seq 1 60); do
		kubectl get pod "$AAP_PG_POD" -n "$NAMESPACE" >/dev/null 2>&1 && break
		sleep 5
	done
	kubectl get pod "$AAP_PG_POD" -n "$NAMESPACE" >/dev/null 2>&1 || die "postgres pod '$AAP_PG_POD' not found in $NAMESPACE after 5m"
	for aap_db in gateway automationcontroller; do
		if kubectl exec -n "$NAMESPACE" "$AAP_PG_POD" -- psql -U postgres -lqt 2>/dev/null | cut -d'|' -f1 | grep -qw "$aap_db"; then
			log "Resetting database: $aap_db"
			kubectl exec -n "$NAMESPACE" "$AAP_PG_POD" -- psql -U postgres -d "$aap_db" \
				-c "DROP SCHEMA public CASCADE; CREATE SCHEMA public AUTHORIZATION ${aap_db};"
		else
			log "Database '$aap_db' not found on $AAP_PG_POD -- nothing to reset (fine on a genuinely fresh install)"
		fi
	done
else
	log "Skipping database reset (RESET_AAP_DB=false)"
fi

# ─── 4. Force-retry any already-crash-looping gateway/controller pods ──────
log "Restarting crash-looping gateway/controller pods, if any"
for label in "app.kubernetes.io/name=gateway" "app.kubernetes.io/name=automationcontroller"; do
	pods="$(kubectl get pods -n "$NAMESPACE" -l "$label" -o name 2>/dev/null || true)"
	for pod in $pods; do
		echo "  Deleting $pod to force immediate retry"
		kubectl delete "$pod" -n "$NAMESPACE" --ignore-not-found
	done
done

log "Done. Watch rollout with: kubectl get pods -n $NAMESPACE -w"
