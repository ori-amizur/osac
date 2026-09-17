#!/usr/bin/env bash
#
# Build and deploy the secondary-virtualnetwork-router-pod feature branch to a cluster.
#
# Covers: pushing the branch AAP will sync from, building/pushing the osac-operator,
# fulfillment-service, and osac-router-agent images, installing infra prerequisites
# (cert-manager/trust-manager), deploying the osac umbrella chart (osac-operator +
# fulfillment-service + osac-aap + CRDs, with routerPodScc.enabled and AAP pointed at
# this branch), and applying the OVN port-security RBAC. Run from the
# osac-workspace/osac mono-repo root.
#
# fulfillment-service is rebuilt too, not just osac-operator/osac-aap: server-side
# changes on this branch (e.g. Story 1.01's networking_type validation) must be in the
# deployed server image, or the --networking-type CLI flag is silently ignored by an
# older server rather than erroring (proto forward-compat drops unknown fields).
#
# Required env vars:
#   REGISTRY            Container registry to push images to, e.g. quay.io/youruser
#
# Optional env vars (defaults shown):
#   IMAGE_TAG            $(git rev-parse --abbrev-ref HEAD)
#   PLATFORM             kind    (also accepts "openshift" -- see AAP_LICENSE_FILE below)
#   PROFILE              dev
#   NAMESPACE            osac
#   GIT_REMOTE           fork
#   GIT_BRANCH           $(git rev-parse --abbrev-ref HEAD)
#   AAP_GIT_URL          derived from `git remote get-url $GIT_REMOTE` (SSH converted to HTTPS)
#   SKIP_GIT_PUSH        false
#   SKIP_ROUTER_AGENT    false   (set true if CI already published this branch's image)
#   NETWORK_CLASS_TITLE  cudn    title used when the test NetworkClass is created
#   NETWORK_CLASS_ENABLED true   create the chart-managed default NetworkClass
#   NETWORK_CLASS_K8S_MANAGER k8s_only  k8s manager for the chart-managed NetworkClass
#   NETWORK_CLASS_FABRIC_MANAGER ""      fabric manager for the chart-managed NetworkClass
#   ENABLE_NETRIS_MANAGER false           register the Netris fabric manager
#   ENABLE_CUDN_K8S_MANAGER false         register cudn_net as a k8s manager
#   FULFILLMENT_SERVICE_IMG derived       override the service image when the
#                                        branch has no service changes and the
#                                        cluster database is ahead of the branch
#   SKIP_FULFILLMENT_BUILD false          do not rebuild/push an overridden service image
#   SKIP_INFRA           true    (set false only for a genuinely fresh cluster -- none of this
#                                 feature's changes touch cert-manager/LVMS/Keycloak/Kafka/etc,
#                                 so re-running install-infra against an existing deployment is
#                                 unnecessary and can disrupt unrelated, already-running
#                                 infrastructure, e.g. LVMS storage still backing live PVs)
#   RESET_FULFILLMENT_DB false   (set true if fulfillment-service fails to start with a
#                                 "no migration found for version N" error -- happens when a
#                                 shared dev/CI Postgres was previously migrated by a build
#                                 with more migrations than this branch has, e.g. a default
#                                 upstream image. DESTROYS all fulfillment-service data --
#                                 tenants, clusters, VirtualNetworks, etc. Only works for
#                                 bundledPostgres (dev/CI) profiles.)
# (No RESET_AAP_DB flag -- steps 3c/4c/4d below prevent the AAP encryption-secret
#  mismatch automatically, resetting gateway/automationcontroller data only once,
#  the first time the stable secrets are created, not on every redeploy.)
#
# Required only when PLATFORM != kind (OpenShift):
#   AAP_LICENSE_FILE     Path to a Red Hat AAP license.zip (see osac-installer/Makefile).
#                         Also requires the active kubeconfig to reach the cluster; unlike
#                         the kind path, this script does not create the cluster for you.
#
# Example (kind, the default):
#   REGISTRY=quay.io/oamizur ./hack/deploy-secondary-virtualnetwork.sh
#
# Example (OpenShift):
#   PLATFORM=openshift AAP_LICENSE_FILE=/path/to/license.zip \
#     REGISTRY=quay.io/oamizur ./hack/deploy-secondary-virtualnetwork.sh

set -euo pipefail

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
die() { printf '\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

# ─── Configuration ────────────────────────────────────────────────────────────
REGISTRY="${REGISTRY:?REGISTRY must be set, e.g. REGISTRY=quay.io/youruser}"
GIT_BRANCH="${GIT_BRANCH:-$(git rev-parse --abbrev-ref HEAD)}"
IMAGE_TAG="${IMAGE_TAG:-$GIT_BRANCH}"
PLATFORM="${PLATFORM:-kind}"
PROFILE="${PROFILE:-dev}"
NAMESPACE="${NAMESPACE:-osac}"
GIT_REMOTE="${GIT_REMOTE:-fork}"
SKIP_GIT_PUSH="${SKIP_GIT_PUSH:-false}"
SKIP_ROUTER_AGENT="${SKIP_ROUTER_AGENT:-false}"
NETWORK_CLASS_TITLE="${NETWORK_CLASS_TITLE:-cudn}"
NETWORK_CLASS_ENABLED="${NETWORK_CLASS_ENABLED:-true}"
NETWORK_CLASS_K8S_MANAGER="${NETWORK_CLASS_K8S_MANAGER:-k8s_only}"
NETWORK_CLASS_FABRIC_MANAGER="${NETWORK_CLASS_FABRIC_MANAGER:-}"
ENABLE_NETRIS_MANAGER="${ENABLE_NETRIS_MANAGER:-false}"
ENABLE_CUDN_K8S_MANAGER="${ENABLE_CUDN_K8S_MANAGER:-false}"
SKIP_INFRA="${SKIP_INFRA:-true}"
RESET_FULFILLMENT_DB="${RESET_FULFILLMENT_DB:-false}"
SKIP_FULFILLMENT_BUILD="${SKIP_FULFILLMENT_BUILD:-false}"

if [[ "$PLATFORM" != "kind" ]]; then
	[[ -n "${AAP_LICENSE_FILE:-}" ]] || die "AAP_LICENSE_FILE must be set when PLATFORM=$PLATFORM (see osac-installer/Makefile)"
	[[ -f "$AAP_LICENSE_FILE" ]] || die "AAP_LICENSE_FILE not found at $AAP_LICENSE_FILE"
	command -v oc >/dev/null 2>&1 || die "'oc' not found in PATH (required for PLATFORM=$PLATFORM)"
	# Validate the active kubeconfig rather than requiring a separate oc login.
	# This also supports callers that provide a cluster-admin kubeconfig directly,
	# such as KUBECONFIG=/home/user/.kube/sno.kubeconfig.
	kubectl get --raw=/version >/dev/null 2>&1 || die "cannot reach the OpenShift API using the active kubeconfig"
	export AAP_LICENSE_FILE  # picked up automatically by osac-installer/Makefile via the environment
fi

if [[ -z "${AAP_GIT_URL:-}" ]]; then
	AAP_GIT_URL="$(git remote get-url "$GIT_REMOTE")"
	# Convert an SSH remote (git@github.com:org/repo.git) to HTTPS -- AAP's default
	# Project config assumes HTTPS and no SCM credential.
	AAP_GIT_URL="$(sed -E 's#^git@([^:]+):#https://\1/#' <<<"$AAP_GIT_URL")"
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# make install-osac runs from osac-installer, so keep the license path valid
# after that directory change even when the caller supplied a repo-relative path.
if [[ "$PLATFORM" != "kind" && "$AAP_LICENSE_FILE" != /* ]]; then
	AAP_LICENSE_FILE="$REPO_ROOT/$AAP_LICENSE_FILE"
	export AAP_LICENSE_FILE
fi

OPERATOR_IMG="${REGISTRY}/osac-operator:${IMAGE_TAG}"
ROUTER_AGENT_IMG="${REGISTRY}/osac-router-agent:${IMAGE_TAG}"
FULFILLMENT_SERVICE_IMG="${FULFILLMENT_SERVICE_IMG:-${REGISTRY}/fulfillment-service:${IMAGE_TAG}}"

NETWORK_MANAGER_HELM_ARGS=""
if [[ "$ENABLE_NETRIS_MANAGER" == "true" ]]; then
	NETWORK_MANAGER_HELM_ARGS+=" --set operator.networkManagers.fabricManagers.netris.enabled=true"
fi
if [[ "$ENABLE_CUDN_K8S_MANAGER" == "true" ]]; then
	# A separate k8s-manager registration is required for the dual-target
	# NetworkClass used by the Netris-backed Secondary-VN flow.
	NETWORK_MANAGER_HELM_ARGS+=" --set operator.networkManagers.k8sManagers.cudn_net.enabled=true"
	NETWORK_MANAGER_HELM_ARGS+=" --set-string operator.networkManagers.k8sManagers.cudn_net.capabilities=ipv4"
fi

NETWORK_CLASS_HELM_ARGS="--set networkClass.enabled=${NETWORK_CLASS_ENABLED}"
if [[ "$NETWORK_CLASS_ENABLED" == "true" ]]; then
	NETWORK_CLASS_HELM_ARGS+=" --set-string networkClass.title=${NETWORK_CLASS_TITLE}"
	NETWORK_CLASS_HELM_ARGS+=" --set-string networkClass.k8sManager=${NETWORK_CLASS_K8S_MANAGER}"
	if [[ -n "$NETWORK_CLASS_FABRIC_MANAGER" ]]; then
		NETWORK_CLASS_HELM_ARGS+=" --set-string networkClass.fabricManager=${NETWORK_CLASS_FABRIC_MANAGER}"
	fi
fi

log "Configuration"
cat <<EOF
  REPO_ROOT:        $REPO_ROOT
  PLATFORM/PROFILE:  $PLATFORM / $PROFILE
  NAMESPACE:        $NAMESPACE
  OPERATOR_IMG:     $OPERATOR_IMG
  ROUTER_AGENT_IMG: $ROUTER_AGENT_IMG
  FULFILLMENT_SERVICE_IMG: $FULFILLMENT_SERVICE_IMG
  GIT_REMOTE/BRANCH: $GIT_REMOTE / $GIT_BRANCH
  AAP_GIT_URL:      $AAP_GIT_URL
  NETWORK_CLASS_ENABLED: $NETWORK_CLASS_ENABLED
  NETWORK_CLASS_K8S_MANAGER: $NETWORK_CLASS_K8S_MANAGER
  NETWORK_CLASS_FABRIC_MANAGER: $NETWORK_CLASS_FABRIC_MANAGER
  ENABLE_NETRIS_MANAGER: $ENABLE_NETRIS_MANAGER
  ENABLE_CUDN_K8S_MANAGER: $ENABLE_CUDN_K8S_MANAGER
EOF

for tool in git go helm kubectl jq; do
	command -v "$tool" >/dev/null 2>&1 || die "required tool '$tool' not found in PATH"
done
if ! command -v podman >/dev/null 2>&1 && ! command -v docker >/dev/null 2>&1; then
	die "neither podman nor docker found in PATH"
fi
CONTAINER_TOOL="$(command -v podman >/dev/null 2>&1 && echo podman || echo docker)"

# ─── 0. Push the branch AAP will sync from ─────────────────────────────────────
if [[ "$SKIP_GIT_PUSH" != "true" ]]; then
	log "Pushing $GIT_BRANCH to $GIT_REMOTE"
	git push "$GIT_REMOTE" "$GIT_BRANCH"
else
	log "Skipping git push (SKIP_GIT_PUSH=true)"
fi

# ─── 1. Build and push osac-operator ────────────────────────────────────────────
log "Building osac-operator image: $OPERATOR_IMG"
(cd osac-operator && make image-build image-push IMG="$OPERATOR_IMG")

# ─── 1b. Build and push fulfillment-service ─────────────────────────────────────
# Required: the umbrella chart otherwise defaults to ghcr.io/osac-project/fulfillment-service:latest
# (upstream main), which does NOT include this branch's server-side changes (e.g. Story
# 1.01's networking_type validation) -- without this, --networking-type would be silently
# dropped by an older server (proto forward-compat ignores unknown fields) rather than erroring.
if [[ "$SKIP_FULFILLMENT_BUILD" == "true" ]]; then
	log "Skipping fulfillment-service build (SKIP_FULFILLMENT_BUILD=true); using $FULFILLMENT_SERVICE_IMG"
else
	log "Building fulfillment-service image: $FULFILLMENT_SERVICE_IMG"
	(cd fulfillment-service && make image-build image-push IMG="$FULFILLMENT_SERVICE_IMG")
fi

# ─── 2. Build and push osac-router-agent ────────────────────────────────────────
if [[ "$SKIP_ROUTER_AGENT" != "true" ]]; then
	log "Building osac-router-agent image: $ROUTER_AGENT_IMG"
	"$CONTAINER_TOOL" build -f osac-router-agent/Containerfile -t "$ROUTER_AGENT_IMG" .
	"$CONTAINER_TOOL" push "$ROUTER_AGENT_IMG"
else
	log "Skipping osac-router-agent build (SKIP_ROUTER_AGENT=true)"
fi

# ─── 3. Install infra prerequisites (cert-manager, trust-manager, envoy-gateway) ─
if [[ "$SKIP_INFRA" != "true" ]]; then
	log "Installing infra prerequisites"
	(cd osac-installer && make install-infra PLATFORM="$PLATFORM" PROFILE="$PROFILE" NS="$NAMESPACE")
else
	log "Skipping infra install (SKIP_INFRA=true)"
fi

# ─── 3b. Reset fulfillment-service's database ───────────────────────────────────
# A shared dev/CI Postgres instance (bundledPostgres) can end up with a schema_migrations
# version ahead of what this branch's migrations know about (e.g. a previous deploy used
# the default upstream image, which has since gained migrations this branch doesn't have,
# or never will if this branch and main have diverged on the same tables). There's no
# force/reset subcommand in fulfillment-service's own migrate tool (internal/database:
# only Migrate(ctx, desiredVersion) is exposed) -- "up to whatever the code knows" is the
# only mode, so a version-ahead database has no self-service recovery path. This resets
# the schema so migrations run cleanly from empty using this branch's own migration set.
# Only attempted for bundledPostgres (dev/CI) -- for an external Postgres profile, resetting
# would need real per-environment credentials/auth this script has no business assuming.
if [[ "$RESET_FULFILLMENT_DB" == "true" ]]; then
	log "Resetting fulfillment-service's database (RESET_FULFILLMENT_DB=true)"
	# shellcheck source=/dev/null
	source osac-installer/scripts/lib.sh
	instance_values="osac-installer/values/${PROFILE}/instance.yaml"
	if _bundled_postgres_enabled "$instance_values"; then
		db_url="$(kubectl get secret osac-db-config -n "$NAMESPACE" -o jsonpath='{.data.url}' | base64 -d)"
		db_host="$(_parse_db_host_from_url "$db_url")"
		db_name="${db_url%%\?*}"; db_name="${db_name##*/}"
		read -r pg_service pg_namespace <<<"$(_resolve_postgres_service "$db_host" "$NAMESPACE")"
		pg_pod="$(kubectl get pods -n "$pg_namespace" -l "$(kubectl get svc "$pg_service" -n "$pg_namespace" -o jsonpath='{.spec.selector}' | jq -r 'to_entries|map("\(.key)=\(.value)")|join(",")')" -o jsonpath='{.items[0].metadata.name}')"
		kubectl exec -n "$pg_namespace" "$pg_pod" -- psql -U postgres -d "$db_name" \
			-c "DROP SCHEMA public CASCADE; CREATE SCHEMA public AUTHORIZATION \"${db_name}\";"
	else
		log "Not using bundledPostgres for profile $PROFILE -- skipping automatic reset; if you're hitting a migration version mismatch, reset the database manually first"
	fi
else
	log "Skipping fulfillment-service database reset (RESET_FULFILLMENT_DB=false)"
fi

# ─── 3c. Pre-create stable AAP encryption secrets (idempotent) ─────────────────
# Prevents (not just reacts to) the gateway/controller crash-loop described below,
# entirely from this script -- no chart change needed. Root cause, confirmed by
# comparing resource creation timestamps and owner references: OSAC's
# AnsibleAutomationPlatform CR (charts/aap/templates/aap.yaml) doesn't specify
# db_fields_encryption_secret/controller.secret_key_secret, so the AAP operator
# auto-generates its own secret for each -- owned by the CR (confirmed via
# `kubectl get secret ... -o jsonpath='{.metadata.ownerReferences}'`), so it is
# garbage-collected whenever `uninstall-osac` deletes the CR and freshly
# regenerated on the next install. The Postgres data, however, lives in a
# StatefulSet PVC, which Kubernetes deliberately never deletes on its own. Every
# reinstall therefore pairs old, still-encrypted data with a brand-new secret that
# cannot decrypt it (cryptography.fernet.InvalidToken while decrypting a stored
# value). Pre-creating these secrets under the exact names the operator would
# otherwise auto-generate, before the CR is patched to reference them below (4c),
# makes the secret's lifecycle independent of the CR's, breaking the mismatch for
# every future redeploy, not just this one.
aap_gateway_secret="osac-aap-db-fields-encryption-secret"
aap_controller_secret="osac-aap-controller-secret-key"
aap_secrets_freshly_created="false"

# The AAP operator otherwise creates these secrets as children of the
# AnsibleAutomationPlatform CR. They are then garbage-collected on uninstall,
# while the AAP Postgres PVC survives, causing the next install to use a new key
# against old encrypted data. Create the namespace and secrets before Helm so
# they are independent of the AAP CR lifecycle. kubectl works for both Kind and
# OpenShift; the old `oc get nameapace` check was misspelled and silently skipped
# this entire block.
log "Ensuring target namespace exists"
kubectl get namespace "$NAMESPACE" >/dev/null 2>&1 || kubectl create namespace "$NAMESPACE"

for aap_secret in "$aap_gateway_secret" "$aap_controller_secret"; do
	if ! kubectl get secret "$aap_secret" -n "$NAMESPACE" >/dev/null 2>&1; then
		log "Creating stable AAP encryption secret: $aap_secret"
		kubectl create secret generic "$aap_secret" -n "$NAMESPACE" \
			--from-literal=secret_key="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
		aap_secrets_freshly_created="true"
	else
		log "Stable AAP encryption secret already exists, reusing: $aap_secret"
	fi
done

# The create-network-class hook resolves the configured k8s manager while Helm is
# still installing the release. On an existing cluster (or a release upgraded
# from the old default), that hook can run before the chart's ConfigMap becomes
# observable and fail with "no ConfigMap ... found". Seed the k8s-only manager
# only when it is the selected chart-managed manager; Netris mode registers its
# cudn_net manager through the operator chart values below.
if [[ "$NETWORK_CLASS_ENABLED" == "true" && "$NETWORK_CLASS_K8S_MANAGER" == "k8s_only" ]]; then
	log "Ensuring k8s-only network manager registration"
	kubectl apply --server-side --force-conflicts --validate=false \
		--field-manager=secondary-vn-deploy -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: osac-network-k8s-manager-k8s-only
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/instance: osac
    app.kubernetes.io/managed-by: Helm
    app.kubernetes.io/name: osac-operator
    osac.openshift.io/network-k8s-manager: "true"
  annotations:
    meta.helm.sh/release-name: osac
    meta.helm.sh/release-namespace: ${NAMESPACE}
data:
  name: k8s_only
  description: Composite k8s-native networking (CUDN, Kubernetes NetworkPolicy, MetalLB L2) with no separate physical fabric. Delegates each resource type to an existing k8s-native implementation — see osac-aap's osac.templates.k8s_only role.
  capabilities: ipv4,ipv6
EOF
fi

# ─── 4. Deploy the osac umbrella chart ─────────────────────────────────────────
# Deploys osac-operator + osac-operator-crds + osac-aap together. This single
# helm upgrade also re-fires osac-aap's post-install/post-upgrade bootstrap hook,
# which runs config-as-code and syncs the AAP Project from AAP_GIT_URL/GIT_BRANCH
# (set below) -- no separate AAP sync step is needed after this.
log "Deploying the osac umbrella chart"
# NOTE: osac-operator, fulfillment-service, and osac-aap are pulled in with Helm
# dependency aliases ("operator", "service", and "aap" respectively, per
# charts/osac/Chart.yaml) -- value overrides must use those aliases, not the chart
# names. fulfillment-service's chart takes a single combined "repo:tag" image string
# (images.service), unlike osac-operator's split image.repository/image.tag.
(cd osac-installer && make install-osac PLATFORM="$PLATFORM" PROFILE="$PROFILE" NS="$NAMESPACE" \
EXTRA_HELM_ARGS="--set routerPodScc.enabled=true \
	  --set routerPodScc.kubeletConfig.enabled=true \
	  --set hubAccess.enabled=true \
	  --set bmf.enabled=false \
	  --set operator.aap.tokenSecret.name=osac-aap-api-token \
	  --set operator.aap.tokenSecret.key=token \
	  --set bundledVault.enabled=false \
	  --set operator.image.repository=${REGISTRY}/osac-operator --set operator.image.tag=${IMAGE_TAG} \
--set operator.controllers.clusterOrder=false \
	  --set operator.networkManagers.enabled=true \
	  ${NETWORK_MANAGER_HELM_ARGS} \
	  --set service.images.service=${FULFILLMENT_SERVICE_IMG} \
	  --set service.images.pullPolicy=Always \
	  ${NETWORK_CLASS_HELM_ARGS} \
	  --set aap.configAsCode.routerPodImage=${ROUTER_AGENT_IMG} \
	  --set aap.configAsCode.projectGitUri=${AAP_GIT_URL} --set aap.configAsCode.projectGitBranch=${GIT_BRANCH}")

if [[ "$NETWORK_CLASS_ENABLED" == "true" ]]; then
	manager_configmap="osac-network-k8s-manager-${NETWORK_CLASS_K8S_MANAGER//_/-}"
	log "Verifying configured k8s network manager registration: $manager_configmap"
	kubectl get configmap "$manager_configmap" -n "$NAMESPACE" >/dev/null
elif [[ "$ENABLE_CUDN_K8S_MANAGER" == "true" ]]; then
	log "Verifying Netris-mode cudn_net network manager registration"
	kubectl get configmap "osac-network-k8s-manager-cudn-net" -n "$NAMESPACE" >/dev/null
fi

# ─── 4c. Point the AnsibleAutomationPlatform CR at the stable secrets ──────────
# The chart's aap.yaml doesn't reference db_fields_encryption_secret/
# controller.secret_key_secret, so helm's own install just created (or updated)
# the CR without them -- the operator would otherwise auto-generate its own the
# moment it reconciles. Patch the CR immediately (racing the operator, hence the
# retry loop) to point it at the stable secrets from 3c instead. Runs on every
# deploy, not just the first: the CR itself gets deleted and recreated by
# `uninstall-osac`, so this reference has to be re-applied each time, even though
# the secrets underneath it stay the same.
log "Pointing the AnsibleAutomationPlatform CR at the stable encryption secrets"
aap_cr_name="osac-aap"
aap_patch=$(printf '{"spec":{"db_fields_encryption_secret":"%s","controller":{"secret_key_secret":"%s"}}}' \
	"$aap_gateway_secret" "$aap_controller_secret")
aap_patch_ok="false"
for _ in $(seq 1 30); do
	if kubectl patch ansibleautomationplatforms.aap.ansible.com "$aap_cr_name" -n "$NAMESPACE" \
		--type=merge -p "$aap_patch" >/dev/null 2>&1; then
		aap_patch_ok="true"
		break
	fi
	sleep 2
done
if [[ "$aap_patch_ok" != "true" ]]; then
	log "WARNING: could not patch the AnsibleAutomationPlatform CR (not found after 60s) -- it may still auto-generate its own secrets this run"
fi

# ─── 4d. One-time reset of AAP's own databases, only when transitioning to the
#         stable secrets for the first time ─────────────────────────────────────
# Only needed once: any data already in these databases was encrypted with
# whatever secret was active before 3c/4c existed (or before this cluster's
# first-ever run of this script), which the newly-stable secret cannot decrypt.
# After this one alignment, the secret no longer changes across reinstalls (4c
# re-applies the same one every time), so the data and the key that can decrypt
# it never diverge again -- no reset needed on subsequent runs.
if [[ "$aap_secrets_freshly_created" == "true" ]]; then
	log "Stable AAP secrets were just created -- resetting gateway/automationcontroller databases once to align with them"
	aap_pg_pod="osac-aap-postgres-15-0"
	for _ in $(seq 1 60); do
		kubectl get pod "$aap_pg_pod" -n "$NAMESPACE" >/dev/null 2>&1 && break
		sleep 5
	done
	for aap_db in gateway automationcontroller; do
		if kubectl exec -n "$NAMESPACE" "$aap_pg_pod" -- psql -U postgres -lqt 2>/dev/null | cut -d'|' -f1 | grep -qw "$aap_db"; then
			kubectl exec -n "$NAMESPACE" "$aap_pg_pod" -- psql -U postgres -d "$aap_db" \
				-c "DROP SCHEMA public CASCADE; CREATE SCHEMA public AUTHORIZATION ${aap_db};"
		else
			log "Database '$aap_db' not found on $aap_pg_pod yet -- nothing to reset (fine on a genuinely fresh install)"
		fi
	done
	# Force any already-crash-looping gateway pod to retry immediately against the
	# now-empty schema and the newly-patched CR, rather than waiting out its
	# CrashLoopBackOff delay.
	gateway_pod="$(kubectl get pods -n "$NAMESPACE" -l app.kubernetes.io/name=gateway -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
	if [[ -n "$gateway_pod" ]]; then
		kubectl delete pod -n "$NAMESPACE" "$gateway_pod" --ignore-not-found
	fi
else
	log "Stable AAP secrets already existed -- no database reset needed"
fi

# ─── 5. OVN port-security RBAC ──────────────────────────────────────────────────
# Currently redundant under osac-sa's existing cluster-admin binding (single-cluster
# deployment) -- applied anyway so this capability is already correctly scoped. For
# multi-cluster (remote VMaaS cluster) deployments, apply the equivalent
# Role/RoleBinding to the REMOTE cluster instead -- see the YAML block in
# osac-operator/docs/vmaas-dedicated-cluster/README.md.
log "Applying OVN port-security RBAC (adjust the ServiceAccount namespace in this file first if osac-sa isn't in 'default')"
kubectl apply -f osac-aap/config/base/osac-sa-ovn-port-security.yaml

# ─── 6. Build the osac CLI locally ──────────────────────────────────────────────
log "Building the osac CLI"
(cd fulfillment-service && go build -o "$REPO_ROOT/osac" ./cmd/osac)
echo "Built: $REPO_ROOT/osac (install it onto your PATH, e.g. sudo install $REPO_ROOT/osac /usr/local/bin/osac)"

log "Done."
cat <<EOF

Next: to force an immediate AAP re-sync later (e.g. after pushing a new commit,
without redoing this whole script), either re-run this script or launch the
config-as-code Job Template directly, e.g.:
  awx job_templates launch "<aap_prefix>-config-as-code" --wait

Suggested verification order: (1) a Primary VirtualNetwork as a regression check,
(2) a Secondary VirtualNetwork + one subnet -- watch whether the OVN port-security
patch task passes, (3) a Secondary-VN-only VM, (4) a dual-attached VM.
EOF
