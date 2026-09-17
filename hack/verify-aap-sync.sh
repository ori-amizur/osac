#!/usr/bin/env bash
#
# Verify that AAP's Project actually synced the branch/commit you expect, rather
# than assuming it did because the deploy script ran without errors. Checks three
# independent things, any of which can silently diverge from what you intended:
#   1. What AAP is CONFIGURED to pull from (config-as-code-ig Secret).
#   2. What actually got CHECKED OUT on disk in the controller-task pod.
#   3. Whether that checked-out commit matches your local branch tip.
#
# Optional env vars (defaults shown):
#   NAMESPACE     osac
#   GIT_REMOTE    fork
#   GIT_BRANCH    $(git rev-parse --abbrev-ref HEAD)
#   PROJECT_NAME  osac      (the AAP Project name, per config-as-code's aap_prefix)
#   CHECK_FILE    (optional) a path, relative to the mono-repo root, to also
#                 confirm exists in the checked-out project -- e.g. a file you
#                 just added and want to confirm actually made it in.
#
# Example:
#   ./hack/verify-aap-sync.sh
#   CHECK_FILE=osac-aap/collections/ansible_collections/osac/templates/roles/cudn_net/tasks/patch_router_pod_lsp.yaml \
#     ./hack/verify-aap-sync.sh

set -euo pipefail

log() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
ok() { printf '\033[1;32m  OK: %s\033[0m\n' "$*"; }
fail() { printf '\033[1;31m  FAIL: %s\033[0m\n' "$*"; FAILED="true"; }
die() { printf '\033[1;31mERROR: %s\033[0m\n' "$*" >&2; exit 1; }

FAILED="false"

NAMESPACE="${NAMESPACE:-osac}"
GIT_REMOTE="${GIT_REMOTE:-fork}"
GIT_BRANCH="${GIT_BRANCH:-$(git rev-parse --abbrev-ref HEAD)}"
PROJECT_NAME="${PROJECT_NAME:-osac}"
CHECK_FILE="${CHECK_FILE:-}"

if [[ -z "${AAP_GIT_URL:-}" ]]; then
	AAP_GIT_URL="$(git remote get-url "$GIT_REMOTE")"
	AAP_GIT_URL="$(sed -E 's#^git@([^:]+):#https://\1/#' <<<"$AAP_GIT_URL")"
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

LOCAL_COMMIT="$(git rev-parse "$GIT_BRANCH")"
LOCAL_SUBJECT="$(git log -1 --format=%s "$GIT_BRANCH")"

log "Expected"
cat <<EOF
  AAP_GIT_URL:    $AAP_GIT_URL
  GIT_BRANCH:     $GIT_BRANCH
  LOCAL_COMMIT:   $LOCAL_COMMIT ($LOCAL_SUBJECT)
EOF

# ─── 1. What AAP is configured to pull from ─────────────────────────────────────
log "1. Checking config-as-code-ig Secret (what AAP is told to pull from)"
if ! kubectl get secret config-as-code-ig -n "$NAMESPACE" >/dev/null 2>&1; then
	die "Secret config-as-code-ig not found in namespace $NAMESPACE -- is AAP deployed here?"
fi
configured_uri="$(kubectl get secret config-as-code-ig -n "$NAMESPACE" -o jsonpath='{.data.AAP_PROJECT_GIT_URI}' 2>/dev/null | base64 -d || true)"
configured_branch="$(kubectl get secret config-as-code-ig -n "$NAMESPACE" -o jsonpath='{.data.AAP_PROJECT_GIT_BRANCH}' 2>/dev/null | base64 -d || true)"
echo "  AAP_PROJECT_GIT_URI:    ${configured_uri:-<not set -- defaults to upstream osac-project/osac>}"
echo "  AAP_PROJECT_GIT_BRANCH: ${configured_branch:-<not set -- defaults to main>}"

if [[ "${configured_uri:-}" == "$AAP_GIT_URL" ]]; then
	ok "configured URI matches $GIT_REMOTE ($AAP_GIT_URL)"
else
	fail "configured URI ('${configured_uri:-<unset>}') does not match $GIT_REMOTE ($AAP_GIT_URL) -- AAP may be syncing the wrong repo"
fi
if [[ "${configured_branch:-}" == "$GIT_BRANCH" ]]; then
	ok "configured branch matches ($GIT_BRANCH)"
else
	fail "configured branch ('${configured_branch:-<unset>}') does not match '$GIT_BRANCH' -- AAP may be syncing the wrong branch"
fi

# ─── 2. What actually got checked out on disk ───────────────────────────────────
log "2. Finding the controller-task pod and its Project checkout"
task_pod="$(kubectl get pods -n "$NAMESPACE" -o name 2>/dev/null | grep 'controller-task' | head -1 | cut -d/ -f2 || true)"
if [[ -z "$task_pod" ]]; then
	die "No controller-task pod found in namespace $NAMESPACE -- is the AutomationController up?"
fi
echo "  Pod: $task_pod"

project_dir="$(kubectl exec -n "$NAMESPACE" "$task_pod" -c osac-aap-controller-task -- \
	sh -c "find /var/lib/awx/projects -maxdepth 1 -type d -iname '*${PROJECT_NAME}*' | head -1" 2>/dev/null || true)"
if [[ -z "$project_dir" ]]; then
	die "No checked-out Project directory matching '*${PROJECT_NAME}*' found under /var/lib/awx/projects in $task_pod"
fi
echo "  Project checkout: $project_dir"

checked_out_commit="$(kubectl exec -n "$NAMESPACE" "$task_pod" -c osac-aap-controller-task -- \
	sh -c "cd '$project_dir' && git rev-parse HEAD" 2>/dev/null || true)"
checked_out_subject="$(kubectl exec -n "$NAMESPACE" "$task_pod" -c osac-aap-controller-task -- \
	sh -c "cd '$project_dir' && git log -1 --format=%s" 2>/dev/null || true)"
echo "  Checked-out commit: ${checked_out_commit:-<unable to read>} (${checked_out_subject:-?})"

# ─── 3. Does the checked-out commit match your local branch tip? ───────────────
log "3. Comparing checked-out commit to local branch tip"
if [[ "$checked_out_commit" == "$LOCAL_COMMIT" ]]; then
	ok "checked-out commit matches local $GIT_BRANCH tip exactly ($LOCAL_COMMIT)"
else
	fail "checked-out commit ($checked_out_commit) does NOT match local $GIT_BRANCH tip ($LOCAL_COMMIT) -- push your latest commits and re-trigger a Project sync"
fi

# ─── Optional: confirm a specific file made it into the checkout ───────────────
if [[ -n "$CHECK_FILE" ]]; then
	log "4. Confirming $CHECK_FILE exists in the checkout"
	if kubectl exec -n "$NAMESPACE" "$task_pod" -c osac-aap-controller-task -- \
		test -f "${project_dir}/${CHECK_FILE}" 2>/dev/null; then
		ok "$CHECK_FILE found in the checkout"
	else
		fail "$CHECK_FILE NOT found in the checkout"
	fi
fi

log "Summary"
if [[ "$FAILED" == "true" ]]; then
	echo "One or more checks failed -- see above. Common fix: re-run the deploy script" \
		"(it re-fires osac-aap's bootstrap hook, which re-syncs the Project), or launch" \
		"the \"<aap_prefix>-config-as-code\" Job Template directly for an immediate sync."
	exit 1
else
	echo "All checks passed -- AAP is running exactly the code on your local $GIT_BRANCH branch."
fi
