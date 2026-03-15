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

cd "${ROOT_DIR}"

echo "Starting BlinkPredict Banckend..."
echo "- PORT=${PORT:-8080}"
echo "- SOLANA_RPC_URL=${SOLANA_RPC_URL:-}"
echo "- VUSDC_MINT=${VUSDC_MINT:-}"
echo "- DB_HOST=${DB_HOST:-} DB_PORT=${DB_PORT:-} APP_DB=${APP_DB:-} APP_DB_USER=${APP_DB_USER:-}"
echo

exec go run ./cmd/api

