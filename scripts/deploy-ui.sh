#!/usr/bin/env bash
# Build the hosts plugin UI and rsync it to .57.
#
# Pairs with the nginx vhost change that points `hosts.4950.store` at
# `/Users/local/var/polar-ui/hosts/`. Cross-page sidebar links (e.g.
# /dashboard.html) fall back to dock's /ui/dist via nginx try_files, so
# this dir only needs to carry hosts's own files.
#
# Required env:
#   GITHUB_TOKEN  read:packages scope, for npm install of
#                 @networkextension/polar-ui-common from GitHub Packages.
# Optional env:
#   DEPLOY_USER   default: local
#   DEPLOY_HOST   default: 127.0.0.1 (assumes ssh tunnel on :5722)
#   DEPLOY_PORT   default: 5722
#   DEPLOY_PATH   default: /Users/local/var/polar-ui/hosts
#
# Usage from the polar-hosts repo root:
#   GITHUB_TOKEN=... ./scripts/deploy-ui.sh

set -euo pipefail

DEPLOY_USER="${DEPLOY_USER:-local}"
DEPLOY_HOST="${DEPLOY_HOST:-127.0.0.1}"
DEPLOY_PORT="${DEPLOY_PORT:-5722}"
DEPLOY_PATH="${DEPLOY_PATH:-/Users/local/var/polar-ui/hosts}"

: "${GITHUB_TOKEN:?GITHUB_TOKEN not set — needed to install polar-ui-common from GitHub Packages}"

repo_root="$(cd "$(dirname "$0")/.." && pwd)"
ui_dir="${repo_root}/ui"

cd "$ui_dir"
echo "[deploy-ui] npm install"
npm install --no-audit --no-fund
echo "[deploy-ui] npm run build"
npm run build

echo "[deploy-ui] ensuring ${DEPLOY_PATH} exists on ${DEPLOY_HOST}"
ssh -p "$DEPLOY_PORT" "${DEPLOY_USER}@${DEPLOY_HOST}" "mkdir -p '${DEPLOY_PATH}'"

echo "[deploy-ui] rsync ui/dist/ -> ${DEPLOY_USER}@${DEPLOY_HOST}:${DEPLOY_PATH}"
rsync -avz --delete -e "ssh -p ${DEPLOY_PORT}" \
    "${ui_dir}/dist/" \
    "${DEPLOY_USER}@${DEPLOY_HOST}:${DEPLOY_PATH}/"

echo "[deploy-ui] done. Files served from ${DEPLOY_PATH} on ${DEPLOY_HOST}."
