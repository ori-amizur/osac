#!/bin/bash
set -euo pipefail

# OSAC CLI Login Script
# Auto-detects parameters from Kubernetes cluster, matching pytest fixture defaults

usage() {
    cat <<EOF
Usage: $0 [OPTIONS]

Login to OSAC CLI via Keycloak (public API) or OpenShift service account (private API).
Auto-detects addresses and Keycloak URL from Kubernetes cluster if not explicitly provided.

OPTIONS:
  --public                 Login to public API (default)
  --private                Login to private API (requires oc and kubectl)
  --username USER          Keycloak username (default: tenant1_admin)
  --password PASS          Keycloak password (default: foobar)
  --namespace NS           Kubernetes namespace (default: osac)
  --service-account SA     Service account for private API (default: admin)
  --insecure               Skip certificate verification (for dev/test)
  --config-dir DIR         Config directory (default: ~/.config/osac)
  --address ADDR           API address (auto-detected if not provided)
  --keycloak-url URL       Keycloak URL (auto-detected if not provided)
  --help                   Show this help message

ENVIRONMENT VARIABLES (override defaults):
  OSAC_JWT_USERNAME                Keycloak username (default: tenant1_admin)
  OSAC_JWT_PASSWORD                Keycloak password (default: foobar)
  OSAC_NAMESPACE                   Kubernetes namespace (default: osac)
  OSAC_SERVICE_ACCOUNT             Service account name (default: admin)
  OSAC_FULFILLMENT_ADDRESS         Public API address (overrides auto-detection)
  OSAC_FULFILLMENT_PRIVATE_ADDRESS Private API address (overrides auto-detection)
  OSAC_KEYCLOAK_URL                Keycloak URL (overrides auto-detection)
  OSAC_CLI_PATH                    Path to osac binary (default: osac)

EXAMPLES:
  # Login to public API with auto-detection (requires kubectl access)
  $0

  # Login to private API
  $0 --private

  # Override username
  $0 --username myuser

  # Custom namespace
  $0 --namespace osac-prod

  # Override via environment variables
  OSAC_JWT_USERNAME=myuser OSAC_JWT_PASSWORD=mypass $0

  # Skip cert verification (dev/test)
  $0 --insecure
EOF
}

# Defaults matching pytest fixtures
USERNAME="${OSAC_JWT_USERNAME:-tenant1_admin}"
PASSWORD="${OSAC_JWT_PASSWORD:-foobar}"
NAMESPACE="${OSAC_NAMESPACE:-osac}"
SERVICE_ACCOUNT="${OSAC_SERVICE_ACCOUNT:-admin}"
API_TYPE="public"
INSECURE_FLAG=""
CONFIG_DIR="${HOME}/.config/osac"
OSAC_BINARY="${OSAC_CLI_PATH:-osac}"
ADDRESS=""
KEYCLOAK_URL=""

# Parse arguments
while [[ $# -gt 0 ]]; do
    case "$1" in
        --public)
            API_TYPE="public"
            shift
            ;;
        --private)
            API_TYPE="private"
            shift
            ;;
        --username)
            USERNAME="$2"
            shift 2
            ;;
        --password)
            PASSWORD="$2"
            shift 2
            ;;
        --namespace)
            NAMESPACE="$2"
            shift 2
            ;;
        --service-account)
            SERVICE_ACCOUNT="$2"
            shift 2
            ;;
        --insecure)
            INSECURE_FLAG="--insecure"
            shift
            ;;
        --config-dir)
            CONFIG_DIR="$2"
            shift 2
            ;;
        --address)
            ADDRESS="$2"
            shift 2
            ;;
        --keycloak-url)
            KEYCLOAK_URL="$2"
            shift 2
            ;;
        --help)
            usage
            exit 0
            ;;
        *)
            echo "Unknown option: $1" >&2
            usage
            exit 1
            ;;
    esac
done

# Auto-detect cluster domain from Kubernetes (matching pytest fixture)
get_cluster_domain() {
    kubectl get ingress.config.openshift.io cluster -o jsonpath='{.spec.domain}' 2>/dev/null || true
}

# Resolve cluster domain once
CLUSTER_DOMAIN=$(get_cluster_domain)
if [[ -z "$CLUSTER_DOMAIN" ]]; then
    echo "Error: Could not auto-detect cluster domain. Ensure you have kubectl access." >&2
    echo "Alternatively, provide --address and --keycloak-url explicitly." >&2
    exit 1
fi

# Resolve addresses (matching pytest fixture logic)
if [[ -z "$ADDRESS" ]]; then
    if [[ "$API_TYPE" == "public" ]]; then
        # Matching: env("OSAC_FULFILLMENT_ADDRESS", f"fulfillment-api-{namespace}.{cluster_domain}:443")
        ADDRESS="${OSAC_FULFILLMENT_ADDRESS:-fulfillment-api-${NAMESPACE}.${CLUSTER_DOMAIN}:443}"
    else
        # Matching: env("OSAC_FULFILLMENT_PRIVATE_ADDRESS", f"fulfillment-internal-api-{namespace}.{cluster_domain}:443")
        ADDRESS="${OSAC_FULFILLMENT_PRIVATE_ADDRESS:-fulfillment-internal-api-${NAMESPACE}.${CLUSTER_DOMAIN}:443}"
    fi
fi

# Resolve Keycloak URL (matching pytest fixture)
if [[ -z "$KEYCLOAK_URL" ]]; then
    # Matching: env("OSAC_KEYCLOAK_URL", f"https://keycloak-keycloak.{cluster_domain}")
    KEYCLOAK_URL="${OSAC_KEYCLOAK_URL:-https://keycloak-keycloak.${CLUSTER_DOMAIN}}"
fi

# Remove port for HTTPS URL construction
ADDRESS_NO_PORT="${ADDRESS%:*}"

mkdir -p "$CONFIG_DIR"

if [[ "$API_TYPE" == "public" ]]; then
    echo "╔════════════════════════════════════════════════════════╗"
    echo "║  OSAC CLI Login (Public API)                           ║"
    echo "╚════════════════════════════════════════════════════════╝"
    echo ""
    echo "  Address:     https://${ADDRESS_NO_PORT}"
    echo "  Username:    ${USERNAME}"
    echo "  Keycloak:    ${KEYCLOAK_URL}"
    echo "  Namespace:   ${NAMESPACE}"
    echo "  Config:      ${CONFIG_DIR}"
    echo ""

    # Create JWT token script (matching _make_jwt_token_script from conftest.py)
    TOKEN_SCRIPT="curl -sk -X POST ${KEYCLOAK_URL}/realms/osac/protocol/openid-connect/token \
        -d grant_type=password -d client_id=osac-cli \
        -d username=${USERNAME} -d password=${PASSWORD} -d 'scope=openid organization' \
        | python3 -c \"import sys,json;print(json.load(sys.stdin)['access_token'])\""

    login_args=(
        "login"
        "--address" "https://${ADDRESS_NO_PORT}"
        "--token-script" "$TOKEN_SCRIPT"
        "--config" "$CONFIG_DIR"
    )
else
    echo "╔════════════════════════════════════════════════════════╗"
    echo "║  OSAC CLI Login (Private API)                          ║"
    echo "╚════════════════════════════════════════════════════════╝"
    echo ""
    echo "  Address:        https://${ADDRESS_NO_PORT}"
    echo "  Service Acct:   ${SERVICE_ACCOUNT}"
    echo "  Namespace:      ${NAMESPACE}"
    echo "  Config:         ${CONFIG_DIR}"
    echo ""

    # Create service account token script (matching private_cli fixture)
    TOKEN_SCRIPT="oc create token -n ${NAMESPACE} ${SERVICE_ACCOUNT} --as system:admin"

    login_args=(
        "login"
        "--address" "https://${ADDRESS_NO_PORT}"
        "--token-script" "$TOKEN_SCRIPT"
        "--private"
        "--config" "$CONFIG_DIR"
    )
fi

if [[ -n "$INSECURE_FLAG" ]]; then
    login_args+=("$INSECURE_FLAG")
    echo "  ⚠️  Certificate verification DISABLED"
    echo ""
fi

echo "Authenticating..."
echo ""

if $OSAC_BINARY "${login_args[@]}"; then
    echo ""
    echo "╔════════════════════════════════════════════════════════╗"
    echo "║  ✓ Login successful!                                   ║"
    echo "╚════════════════════════════════════════════════════════╝"
    echo ""
    echo "Config saved to: ${CONFIG_DIR}"
    echo ""
    echo "Try:"
    echo "  $OSAC_BINARY --config $CONFIG_DIR get clusters"
    echo "  $OSAC_BINARY --config $CONFIG_DIR get tenants"
    echo ""
else
    echo ""
    echo "╔════════════════════════════════════════════════════════╗"
    echo "║  ✗ Login failed                                        ║"
    echo "╚════════════════════════════════════════════════════════╝"
    exit 1
fi
