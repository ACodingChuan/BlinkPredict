#!/usr/bin/env bash
set -euo pipefail

# Run the BlinkPredict Banckend API locally with env vars loaded from Banckend/.env.
# Usage:
#   bash Banckend/scripts/dev.sh
#   ENV_FILE=/abs/path/to/.env bash Banckend/scripts/dev.sh

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd -- "${SCRIPT_DIR}/.." && pwd)"

ENV_FILE_DEFAULT="${ROOT_DIR}/.env"
ENV_FILE="${ENV_FILE:-${ENV_FILE_DEFAULT}}"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "Env file not found: ${ENV_FILE}" >&2
  echo "Create one based on: ${ROOT_DIR}/.env.example" >&2
  exit 1
fi

set -a
# shellcheck disable=SC1090
source "${ENV_FILE}"
set +a

if [[ -z "${REDIS_URL:-}" ]]; then
  echo "Missing required env var: REDIS_URL" >&2
  echo "Writer/read-model stages now require Redis. Set REDIS_URL in ${ENV_FILE}." >&2
  exit 1
fi

cd "${ROOT_DIR}"

echo "Starting BlinkPredict Banckend..."
echo "- PORT=${PORT:-8080}"
echo "- LOG_LEVEL=${LOG_LEVEL:-info}"
echo "- SOLANA_RPC_URL=${SOLANA_RPC_URL:-}"
echo "- PROGRAM_ID=${PROGRAM_ID:-}"
echo "- VUSDC_MINT=${VUSDC_MINT:-}"
echo "- DB_HOST=${DB_HOST:-} DB_PORT=${DB_PORT:-} APP_DB=${APP_DB:-} APP_DB_USER=${APP_DB_USER:-}"
echo "- NATS_URL=${NATS_URL:-} NATS_STREAM_CMD=${NATS_STREAM_CMD:-AP_CMD} NATS_STREAM_EVT=${NATS_STREAM_EVT:-AP_EVT}"
echo "- REDIS_URL=${REDIS_URL:-}"
echo "- MATCHER_TICK_INTERVAL=${MATCHER_TICK_INTERVAL:-1s}"
echo "- HELIUS_API_KEY=${HELIUS_API_KEY:-}${HELIUS_API_KEY:+ (truncated)}"
echo "- HELIUS_WEBHOOK_SECRET=${HELIUS_WEBHOOK_SECRET:+(SET)}"
echo "- HELIUS_WEBHOOK_ENABLED=${HELIUS_WEBHOOK_ENABLED:-false}"
echo "- ALCHEMY_SIGNING_KEY=${ALCHEMY_SIGNING_KEY:+(SET)}"
echo

# By default, also write logs to a file so you can always "tail -f" them (useful in IDE tasks
# where the terminal output is not visible/scrolling).
LOG_DIR_DEFAULT="${ROOT_DIR}/.logs"
LOG_FILE_DEFAULT="${LOG_DIR_DEFAULT}/banckend-dev.log"
LOG_FILE="${LOG_FILE:-${LOG_FILE_DEFAULT}}"

mkdir -p "$(dirname -- "${LOG_FILE}")"
echo "Logging:"
echo "- file: ${LOG_FILE}"
echo "- follow: tail -f \"${LOG_FILE}\""
echo

echo "Generating Swagger docs..."
go run github.com/swaggo/swag/cmd/swag@v1.16.4 init \
  -g main.go \
  -d cmd/api,internal/http,internal/orders,internal/markets,internal/matching \
  --parseInternal \
  -o docs
echo

# Keep stdout/stderr in the current terminal AND persist to disk.
go run ./cmd/api 2>&1 | tee -a "${LOG_FILE}"
