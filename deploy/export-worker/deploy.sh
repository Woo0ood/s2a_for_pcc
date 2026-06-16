#!/usr/bin/env bash
# Standalone deploy for the payload-audit export-worker.
# NOT part of sub2api's release: this script builds + ships + runs the worker on its own.
#
# Usage:
#   deploy/export-worker/deploy.sh [path/to/export-worker.env]
# The env file (default ./export-worker.env next to this script) holds BOTH the
# deploy-time SSH info and the worker runtime config. DEPLOY_* / WORKER_* (deploy)
# vars are used locally; the rest are shipped to the server as the container env-file.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
ENV_FILE="${1:-$SCRIPT_DIR/export-worker.env}"

[ -f "$ENV_FILE" ] || { echo "✗ env file not found: $ENV_FILE (copy export-worker.env.example)"; exit 1; }
# shellcheck disable=SC1090
set -a; source "$ENV_FILE"; set +a

# --- required ---
: "${DEPLOY_SSH:?set DEPLOY_SSH (an ssh-config alias, e.g. newdc, or user@host)}"
: "${WORKER_DOCKER_NETWORK:?set WORKER_DOCKER_NETWORK (the docker net clickhouse is on)}"
: "${EXPORT_WORKER_TOKEN:?}" ; : "${CH_DSN:?}" ; : "${BLOB_S3_BUCKET:?}" ; : "${BLOB_S3_SECRET_ACCESS_KEY:?}"

DEPLOY_REMOTE_DIR="${DEPLOY_REMOTE_DIR:-/opt/s2a-export-worker}"
WORKER_PUBLISH="${WORKER_PUBLISH:-0.0.0.0:8088:8088}"
WORKER_MEM_LIMIT="${WORKER_MEM_LIMIT:-2g}"
CONTAINER_NAME="${CONTAINER_NAME:-s2a-export-worker}"
TAG="${WORKER_IMAGE_TAG:-$(cd "$REPO_ROOT" && git rev-parse --short HEAD 2>/dev/null || echo latest)}"
IMAGE="s2a-export-worker:${TAG}"
BUILD_DIR="$SCRIPT_DIR/build"

echo "==> [1/6] cross-compiling static binary (linux/amd64)"
mkdir -p "$BUILD_DIR"
( cd "$REPO_ROOT/backend" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags='-s -w' -o "$BUILD_DIR/export-worker" ./cmd/export-worker )
echo "    binary: $(ls -lh "$BUILD_DIR/export-worker" | awk '{print $5}')"

echo "==> [2/6] writing container env-file (runtime vars only; DEPLOY_*/WORKER_* excluded)"
RUNTIME_VARS=(EXPORT_WORKER_LISTEN EXPORT_WORKER_TOKEN CH_DSN CH_TABLE \
  BLOB_S3_ENDPOINT BLOB_S3_REGION BLOB_S3_BUCKET BLOB_S3_ACCESS_KEY_ID BLOB_S3_SECRET_ACCESS_KEY \
  BLOB_S3_PREFIX BLOB_S3_FORCE_PATH_STYLE EXPORT_RESULT_PREFIX RENDER_TIMEOUT_MINUTES CONV_WINDOW_DAYS)
: > "$BUILD_DIR/worker.env"
for v in "${RUNTIME_VARS[@]}"; do
  if [ -n "${!v:-}" ]; then printf '%s=%s\n' "$v" "${!v}" >> "$BUILD_DIR/worker.env"; fi
done

echo "==> [3/6] shipping binary + Dockerfile + env to $DEPLOY_SSH:$DEPLOY_REMOTE_DIR"
ssh "$DEPLOY_SSH" "mkdir -p '$DEPLOY_REMOTE_DIR'"
scp -q "$BUILD_DIR/export-worker" "$SCRIPT_DIR/../../backend/cmd/export-worker/Dockerfile" "$BUILD_DIR/worker.env" \
    "$DEPLOY_SSH:$DEPLOY_REMOTE_DIR/"

echo "==> [4/6] building image on the server (native amd64): $IMAGE"
ssh "$DEPLOY_SSH" "cd '$DEPLOY_REMOTE_DIR' && docker build -q -t '$IMAGE' . >/dev/null && echo built"

echo "==> [5/6] (re)starting container '$CONTAINER_NAME'"
ssh "$DEPLOY_SSH" "docker rm -f '$CONTAINER_NAME' >/dev/null 2>&1 || true; \
  docker run -d --name '$CONTAINER_NAME' --restart unless-stopped \
    --memory '$WORKER_MEM_LIMIT' --network '$WORKER_DOCKER_NETWORK' \
    --env-file '$DEPLOY_REMOTE_DIR/worker.env' -p '$WORKER_PUBLISH' '$IMAGE' >/dev/null && echo started"

echo "==> [6/6] health check"
PORT="${WORKER_PUBLISH##*:}"; PORT="${PORT%%/*}"
for i in 1 2 3 4 5; do
  if ssh "$DEPLOY_SSH" "curl -fsS http://127.0.0.1:${PORT}/healthz" 2>/dev/null; then
    echo; echo "✓ export-worker up (image $IMAGE, mem $WORKER_MEM_LIMIT, net $WORKER_DOCKER_NETWORK)"
    echo "  → set sub2api payload-audit 'export_worker_url' to your public URL fronting :${PORT}"
    exit 0
  fi
  sleep 2
done
echo "✗ health check failed; logs: ssh $DEPLOY_SSH 'docker logs --tail 50 $CONTAINER_NAME'"; exit 1
