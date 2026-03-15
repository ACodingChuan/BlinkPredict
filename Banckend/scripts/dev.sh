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

# Keep stdout/stderr in the current terminal AND persist to disk.
go run ./cmd/api 2>&1 | tee -a "${LOG_FILE}"
