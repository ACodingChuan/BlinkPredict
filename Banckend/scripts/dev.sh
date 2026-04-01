#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"
ENV_FILE="${ENV_FILE:-${ROOT_DIR}/.env}"
LOG_FILE="${LOG_FILE:-${ROOT_DIR}/.logs/banckend-dev.log}"
SKIP_SWAGGER="${SKIP_SWAGGER:-0}"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "Env file not found: ${ENV_FILE}" >&2
  echo "Create one based on: ${ROOT_DIR}/.env.example" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

mask_secret() {
  local value="${1:-}"
  if [[ -z "${value}" ]]; then
    printf '(empty)'
    return
  fi
  printf '(set:%d chars)' "${#value}"
}

require_any_db_config() {
  if [[ -n "${DATABASE_URL:-}" ]]; then
    return 0
  fi
  [[ -n "${DB_HOST:-}" && -n "${APP_DB:-}" && -n "${APP_DB_USER:-}" ]]
}

if ! require_any_db_config; then
  echo "Missing database config. Set DATABASE_URL or DB_HOST/APP_DB/APP_DB_USER in ${ENV_FILE}." >&2
  exit 1
fi

if [[ -z "${REDIS_URL:-}" ]]; then
  echo "Missing required env var: REDIS_URL" >&2
  echo "Writer/read-model rebuild and hot query paths require Redis." >&2
  exit 1
fi

mkdir -p "$(dirname -- "${LOG_FILE}")"
cd "${ROOT_DIR}"

echo "Starting BlinkPredict Banckend"
echo "- ENV_FILE=${ENV_FILE}"
echo "- LOG_FILE=${LOG_FILE}"
echo "- PORT=${PORT:-8080}"
echo "- LOG_LEVEL=${LOG_LEVEL:-info}"
echo "- DB_HOST=${DB_HOST:-via DATABASE_URL} DB_PORT=${DB_PORT:-5432} APP_DB=${APP_DB:-} APP_DB_USER=${APP_DB_USER:-}"
echo "- NATS_URL=$(mask_secret "${NATS_URL:-}") NATS_STREAM_CMD=${NATS_STREAM_CMD:-AP_CMD} NATS_STREAM_EVT=${NATS_STREAM_EVT:-AP_EVT} NATS_STREAM_WHK=${NATS_STREAM_WHK:-AP_WHK}"
echo "- REDIS_URL=$(mask_secret "${REDIS_URL:-}")"
echo "- SOLANA_RPC_URL=${SOLANA_RPC_URL:-}"
echo "- PROGRAM_ID=${PROGRAM_ID:-}"
echo "- VUSDC_MINT=${VUSDC_MINT:-} GLOBAL_VAULT=${GLOBAL_VAULT:-}"
echo "- MATCHER_TICK_INTERVAL=${MATCHER_TICK_INTERVAL:-1s}"
echo "- SETTLEMENT_RELAYER_KEYPAIR=$(mask_secret "${SETTLEMENT_RELAYER_KEYPAIR:-}")"
echo "- HELIUS_WEBHOOK_ENABLED=${HELIUS_WEBHOOK_ENABLED:-false} HELIUS_WEBHOOK_SECRET=$(mask_secret "${HELIUS_WEBHOOK_SECRET:-}")"
echo "- ALCHEMY_SIGNING_KEY=$(mask_secret "${ALCHEMY_SIGNING_KEY:-}")"

if [[ -z "${NATS_URL:-}" ]]; then
  echo "Warning: NATS_URL is empty, command bus / matcher / settlement bootstrap will stay disabled." >&2
fi

if [[ -z "${SETTLEMENT_RELAYER_KEYPAIR:-}" ]]; then
  echo "Warning: SETTLEMENT_RELAYER_KEYPAIR is empty, settlement submission service will stay disabled." >&2
fi

if [[ "${SKIP_SWAGGER}" != "1" ]]; then
  echo "Generating Swagger docs..."
  go run github.com/swaggo/swag/cmd/swag@v1.16.4 init \
    -g main.go \
    -d cmd/api,internal/http,internal/orders,internal/markets,internal/matching \
    --parseInternal \
    -o docs
  echo
else
  echo "Skipping Swagger generation (SKIP_SWAGGER=1)"
  echo
fi

exec go run ./cmd/api 2>&1 | tee -a "${LOG_FILE}"
