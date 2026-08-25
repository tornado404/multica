#!/usr/bin/env bash
set -euo pipefail

# ==========================================================================
# deploy-selfhost.sh — build the fork images on this Mac, ship them to the
# self-host server over SSH, and roll the Docker Compose stack to them.
#
# The cloud server is too weak to compile (builds OOM), so it NEVER builds:
#   1. preflight      docker/buildx/SSH reachable, not mid-rebase
#   2. build          docker buildx --platform linux/amd64 (cross-build from
#                     arm64 Mac; Go cross-compiles natively, Next.js runs
#                     under Rosetta/QEMU — keep npmmirror/GOPROXY tweaks)
#   3. ship           docker save | gzip | ssh 'gzip -d | docker load'
#   4. deploy         rsync compose file, pin .env image vars, compose up -d
#                     (migrations run inside the backend entrypoint)
#   5. health check   poll the published backend port for /health, then the
#                     frontend; on failure dump backend logs (and optionally
#                     roll back to the previous tag)
#
# Image tags are fork-local (no registry): multica-backend:<TAG> /
# multica-web:<TAG> where TAG = <upstream-release>-zcode.<short-sha>
# (e.g. v0.4.33-zcode.64e301d). Old tags are never pruned on the server, so
# `--rollback` is always available.
#
# Configuration (environment):
#   MULTICA_SSH_HOST     required — ssh alias from ~/.ssh/config
#   MULTICA_REMOTE_DIR   default ~/multica — compose project dir on the server
#   MULTICA_PLATFORM     default linux/amd64 — target platform
#   MULTICA_FORK_STATE_DIR  default ~/.multica-fork-sync — state/log directory
#
# Usage:
#   MULTICA_SSH_HOST=my-server bash scripts/deploy-selfhost.sh --init   # first deploy
#   MULTICA_SSH_HOST=my-server bash scripts/deploy-selfhost.sh          # redeploy
#   MULTICA_SSH_HOST=my-server bash scripts/deploy-selfhost.sh --tag v0.4.33-zcode.64e301d
#   MULTICA_SSH_HOST=my-server bash scripts/deploy-selfhost.sh --rollback
#
# Flags:
#   --tag <TAG>        image tag to build and deploy (default: derived from
#                      the newest reachable v* tag + short HEAD sha)
#   --init             bootstrap the remote project dir + generate .env with
#                      random secrets (same recipe as `make selfhost`)
#   --skip-build       reuse images already tagged locally
#   --backup           pg_dump before switching tags; keeps the newest 3 in
#                      <remote dir>/backups/ (off by default)
#   --auto-rollback    on failed health check, re-pin the previous deployed
#                      tag and `up -d` again (used by auto-update-selfhost.sh)
#   --rollback         re-deploy PREV_DEPLOYED_TAG from state, then health check
#   --dry-run          print every command without side effects
# ==========================================================================

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

TAG="" INIT=0 SKIP_BUILD=0 BACKUP=0 AUTO_ROLLBACK=0 ROLLBACK=0 DRY_RUN=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag) TAG="${2:?--tag needs a value}"; shift 2 ;;
    --init) INIT=1; shift ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --backup) BACKUP=1; shift ;;
    --auto-rollback) AUTO_ROLLBACK=1; shift ;;
    --rollback) ROLLBACK=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

SSH_HOST="${MULTICA_SSH_HOST:-}"
[[ -n "$SSH_HOST" ]] || { echo "MULTICA_SSH_HOST is required (ssh alias), e.g. MULTICA_SSH_HOST=my-server bash scripts/deploy-selfhost.sh" >&2; exit 1; }
REMOTE_DIR="${MULTICA_REMOTE_DIR:-~/multica}"
PLATFORM="${MULTICA_PLATFORM:-linux/amd64}"
STATE_DIR="${MULTICA_FORK_STATE_DIR:-$HOME/.multica-fork-sync}"
STATE_FILE="$STATE_DIR/state.env"
HEALTH_TIMEOUT="${MULTICA_DEPLOY_HEALTH_TIMEOUT:-300}"   # migrations run first on a slow box

[[ "$REMOTE_DIR" =~ [[:space:]\"] ]] && { echo "MULTICA_REMOTE_DIR must not contain spaces or quotes: $REMOTE_DIR" >&2; exit 1; }

log()  { printf '\033[1;34m[deploy]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

sed_i() {
  if [[ "$(uname)" = "Darwin" ]]; then sed -i '' "$@"; else sed -i "$@"; fi
}

# --dry-run: print instead of execute. run "cmd" args... / runsh 'shell string'
run() {
  printf '+ %s\n' "$*"
  [[ "$DRY_RUN" -eq 1 ]] || "$@"
}

SSH_OPTS=(-o BatchMode=yes -o ConnectTimeout=10)
remote() {
  printf '[ssh] %s\n' "$1"
  [[ "$DRY_RUN" -eq 1 ]] || ssh "${SSH_OPTS[@]}" "$SSH_HOST" "$1"
}
remote_quiet() { ssh "${SSH_OPTS[@]}" "$SSH_HOST" "$1"; }

# ---- state file -------------------------------------------------------------
state_get() {
  [[ -f "$STATE_FILE" ]] || return 0
  grep -E "^$1=" "$STATE_FILE" 2>/dev/null | tail -n 1 | cut -d= -f2- || true
}
state_set() { # key value
  [[ "$DRY_RUN" -eq 1 ]] && { printf '[state] %s=%s\n' "$1" "$2"; return 0; }
  mkdir -p "$STATE_DIR"; touch "$STATE_FILE"
  if grep -qE "^$1=" "$STATE_FILE" 2>/dev/null; then
    sed_i "s#^$1=.*#$1=$2#" "$STATE_FILE"
  else
    printf '%s=%s\n' "$1" "$2" >> "$STATE_FILE"
  fi
}

rebase_in_progress() {
  local p
  p="$(git rev-parse --git-path rebase-merge 2>/dev/null)"; [[ -d "$p" ]] && return 0
  p="$(git rev-parse --git-path rebase-apply 2>/dev/null)"; [[ -d "$p" ]]
}

latest_upstream_tag() { # [ref] — newest v* tag reachable from ref (fallback: newest v* overall)
  local t
  t="$(git tag --list 'v[0-9]*' --sort=-v:refname --merged "${1:-HEAD}" 2>/dev/null | head -n 1 || true)"
  [[ -n "$t" ]] || t="$(git tag --list 'v[0-9]*' --sort=-v:refname 2>/dev/null | head -n 1 || true)"
  printf '%s' "$t"
}

# ---- preflight --------------------------------------------------------------
rebase_in_progress && die "a rebase is in progress — finish it (git rebase --continue) or abort before deploying"
command -v docker >/dev/null 2>&1 || die "docker not found"
docker info >/dev/null 2>&1 || die "Docker is not running (start Docker Desktop)"

if [[ "$DRY_RUN" -eq 1 ]]; then
  remote_quiet "true" 2>/dev/null || warn "SSH $SSH_HOST not reachable in BatchMode — dry-run continues anyway"
else
  remote_quiet "true" 2>/dev/null || die "cannot ssh to $SSH_HOST (BatchMode — needs key-based auth)"
  remote_quiet "docker compose version --short >/dev/null 2>&1" || die "docker compose plugin missing on $SSH_HOST"
  remote_quiet "mkdir -p $REMOTE_DIR" || die "cannot create $REMOTE_DIR on $SSH_HOST"
fi

# ---- resolve tag / rollback target ------------------------------------------
BACKEND_IMAGE_NAME="multica-backend"
WEB_IMAGE_NAME="multica-web"

if [[ "$ROLLBACK" -eq 1 ]]; then
  TAG="$(state_get PREV_DEPLOYED_TAG)"
  [[ -n "$TAG" ]] || die "no PREV_DEPLOYED_TAG in $STATE_FILE — nothing to roll back to"
  LAST="$(state_get LAST_DEPLOYED_TAG)"
  [[ "$TAG" != "$LAST" ]] || die "PREV_DEPLOYED_TAG ($TAG) == LAST_DEPLOYED_TAG — already on it"
  log "rolling back to $TAG"
else
  if [[ -z "$TAG" ]]; then
    UPSTREAM_TAG="$(latest_upstream_tag HEAD)"
    [[ -n "$UPSTREAM_TAG" ]] || die "no v* release tag found — pass --tag explicitly"
    TAG="${UPSTREAM_TAG}-zcode.$(git rev-parse --short HEAD)"
  fi
  log "target: branch=$(git branch --show-current) HEAD=$(git rev-parse --short HEAD) tag=$TAG platform=$PLATFORM"
  log "server: $SSH_HOST:$REMOTE_DIR"
fi

# ---- build ------------------------------------------------------------------
if [[ "$ROLLBACK" -eq 0 && "$SKIP_BUILD" -eq 0 ]]; then
  docker buildx version >/dev/null 2>&1 || die "docker buildx not available"
  VERSION_ARG="${TAG%%-zcode.*}"
  COMMIT_SHA="$(git rev-parse --short HEAD)"
  log "building backend image ($PLATFORM)…"
  run docker buildx build --platform "$PLATFORM" -f Dockerfile \
    --build-arg "VERSION=$VERSION_ARG" --build-arg "COMMIT=$COMMIT_SHA" \
    --build-arg "DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -t "$BACKEND_IMAGE_NAME:$TAG" --load .
  log "building web image ($PLATFORM)…"
  run docker buildx build --platform "$PLATFORM" -f Dockerfile.web \
    --build-arg "NEXT_PUBLIC_APP_VERSION=$TAG" \
    -t "$WEB_IMAGE_NAME:$TAG" --load .
fi
if [[ "$ROLLBACK" -eq 0 ]]; then
  for img in "$BACKEND_IMAGE_NAME:$TAG" "$WEB_IMAGE_NAME:$TAG"; do
    run docker image inspect "$img" >/dev/null
  done
fi

# ---- ship -------------------------------------------------------------------
if [[ "$ROLLBACK" -eq 0 ]]; then
  log "shipping images to $SSH_HOST (docker save | gzip | ssh load)…"
  if [[ "$DRY_RUN" -eq 1 ]]; then
    printf '+ docker save %s:%s %s:%s | gzip | ssh %s "gzip -d | docker load"\n' \
      "$BACKEND_IMAGE_NAME" "$TAG" "$WEB_IMAGE_NAME" "$TAG" "$SSH_HOST"
  else
    docker save "$BACKEND_IMAGE_NAME:$TAG" "$WEB_IMAGE_NAME:$TAG" | gzip \
      | ssh "${SSH_OPTS[@]}" "$SSH_HOST" 'gzip -d | docker load'
  fi
fi

# ---- remote project dir / .env ----------------------------------------------
log "syncing docker-compose.selfhost.yml → $SSH_HOST:$REMOTE_DIR …"
if [[ "$DRY_RUN" -eq 0 ]]; then
  rsync -e "ssh ${SSH_OPTS[*]}" docker-compose.selfhost.yml "$SSH_HOST:$REMOTE_DIR/" >/dev/null \
    || die "rsync of compose file failed"
fi

if [[ "$INIT" -eq 1 ]]; then
  if [[ "$DRY_RUN" -eq 0 ]] && remote_quiet "test -f $REMOTE_DIR/.env" 2>/dev/null; then
    log "--init: remote .env already exists — left untouched"
  else
    log "--init: generating remote .env with random secrets…"
    TMP_ENV="$(mktemp)"
    trap 'rm -f "$TMP_ENV"' EXIT
    cp .env.example "$TMP_ENV"
    JWT="$(openssl rand -hex 32)"
    PGPASS="$(openssl rand -hex 24)"
    VCSKEY="$(openssl rand -base64 32)"
    sed_i "s/^JWT_SECRET=.*/JWT_SECRET=$JWT/" "$TMP_ENV"
    sed_i "s/^POSTGRES_PASSWORD=.*/POSTGRES_PASSWORD=$PGPASS/" "$TMP_ENV"
    sed_i -E "s#^(DATABASE_URL=postgres://[^:]+:)[^@]*(@.*)#\1$PGPASS\2#" "$TMP_ENV"
    sed_i "s#^MULTICA_VCS_SECRET_KEY=.*#MULTICA_VCS_SECRET_KEY=$VCSKEY#" "$TMP_ENV"
    if [[ "$DRY_RUN" -eq 1 ]]; then
      printf '+ rsync <generated .env> %s:%s/.env\n' "$SSH_HOST" "$REMOTE_DIR"
    else
      rsync -e "ssh ${SSH_OPTS[*]}" "$TMP_ENV" "$SSH_HOST:$REMOTE_DIR/.env" >/dev/null \
        || die "upload of generated .env failed"
    fi
    rm -f "$TMP_ENV"; trap - EXIT
    warn "edit $SSH_HOST:$REMOTE_DIR/.env — at minimum FRONTEND_ORIGIN (your nginx domain)"
  fi
fi

if [[ "$ROLLBACK" -eq 0 && "$DRY_RUN" -eq 0 ]]; then
  remote_quiet "test -f $REMOTE_DIR/.env" 2>/dev/null \
    || die "remote .env missing on $SSH_HOST:$REMOTE_DIR — run once with --init"
fi

# optional pre-upgrade DB dump (keeps newest 3 on the server)
if [[ "$BACKUP" -eq 1 && "$ROLLBACK" -eq 0 ]]; then
  log "--backup: dumping postgres on the server (keeps newest 3)…"
  remote "cd $REMOTE_DIR && mkdir -p backups && docker compose exec -T postgres \
sh -c 'pg_dump -U \"\${POSTGRES_USER:-multica}\" \"\${POSTGRES_DB:-multica}\"' | gzip \
> backups/multica-\$(date +%Y%m%d-%H%M%S).sql.gz && ls -1t backups/multica-*.sql.gz | tail -n +4 | xargs -r rm --"
fi

# ---- switch tag + up ---------------------------------------------------------
remote_env_set() { # key value — replace-or-append in the remote .env
  remote "cd $REMOTE_DIR && touch .env && if grep -qE '^$1=' .env; then \
sed -i -E 's#^$1=.*#$1=$2#' .env; else printf '%s=%s\n' '$1' '$2' >> .env; fi"
}

remote_env_set MULTICA_BACKEND_IMAGE "$BACKEND_IMAGE_NAME"
remote_env_set MULTICA_WEB_IMAGE "$WEB_IMAGE_NAME"
remote_env_set MULTICA_IMAGE_TAG "$TAG"

log "bringing up the stack on $SSH_HOST (migrations run first)…"
remote "cd $REMOTE_DIR && docker compose -f docker-compose.selfhost.yml up -d"

# ---- health check ------------------------------------------------------------
published_backend_port() {
  remote_quiet "cd $REMOTE_DIR && docker compose port backend 8080 2>/dev/null" 2>/dev/null | tail -n 1 || true
}
published_frontend_port() {
  remote_quiet "cd $REMOTE_DIR && docker compose port frontend 3000 2>/dev/null" 2>/dev/null | tail -n 1 || true
}

backend_healthy() {
  local pub; pub="$(published_backend_port)"
  [[ "$pub" == *:* ]] || return 1
  remote_quiet "curl -fsS --max-time 5 'http://$pub/health' >/dev/null" 2>/dev/null
}

frontend_reachable() {
  local pub code; pub="$(published_frontend_port)"
  [[ "$pub" == *:* ]] || return 1
  code="$(remote_quiet "curl -s -o /dev/null -w '%{http_code}' --max-time 10 'http://$pub/'" 2>/dev/null || echo 000)"
  [[ "$code" =~ ^[23] ]]
}

log "waiting for backend /health (budget ${HEALTH_TIMEOUT}s)…"
if [[ "$DRY_RUN" -eq 1 ]]; then
  log "dry-run: skipping health check"
else
  DEADLINE=$(( $(date +%s) + HEALTH_TIMEOUT ))
  until backend_healthy; do
    (( $(date +%s) < DEADLINE )) || break
    sleep 5
  done
  if ! backend_healthy; then
    warn "backend not healthy after ${HEALTH_TIMEOUT}s — last 100 backend log lines:"
    remote "cd $REMOTE_DIR && docker compose logs backend --tail 100 --no-color"
    if [[ "$AUTO_ROLLBACK" -eq 1 ]]; then
      PREV="$(state_get PREV_DEPLOYED_TAG)"
      if [[ -n "$PREV" && "$PREV" != "$TAG" ]]; then
        warn "auto-rollback: re-pinning $PREV and restarting…"
        remote_env_set MULTICA_IMAGE_TAG "$PREV"
        remote "cd $REMOTE_DIR && docker compose -f docker-compose.selfhost.yml up -d"
        sleep 10
        if backend_healthy; then warn "rolled back to $PREV (backend healthy)"; else warn "rollback to $PREV still unhealthy — inspect server logs"; fi
      else
        warn "auto-rollback: no PREV_DEPLOYED_TAG recorded — leaving stack as is"
      fi
    fi
    die "deployment of $TAG failed"
  fi
  FRONT_OK=0
  for _ in $(seq 1 12); do
    if frontend_reachable; then FRONT_OK=1; break; fi
    sleep 5
  done
  [[ "$FRONT_OK" -eq 1 ]] || warn "frontend did not answer in 60s — check: cd $REMOTE_DIR && docker compose logs frontend"
fi

# ---- record state ------------------------------------------------------------
if [[ "$ROLLBACK" -eq 0 ]]; then
  PREV="$(state_get LAST_DEPLOYED_TAG)"
  if [[ -n "$PREV" && "$PREV" != "$TAG" ]]; then
    state_set PREV_DEPLOYED_TAG "$PREV"
  fi
  state_set LAST_DEPLOYED_TAG "$TAG"
  state_set LAST_DEPLOYED_AT "$(date '+%Y-%m-%d %H:%M:%S')"
else
  state_set LAST_DEPLOYED_TAG "$TAG"
  state_set LAST_DEPLOYED_AT "$(date '+%Y-%m-%d %H:%M:%S') (rollback)"
fi

log "✓ deployed $BACKEND_IMAGE_NAME:$TAG + $WEB_IMAGE_NAME:$TAG to $SSH_HOST:$REMOTE_DIR"
if [[ "$DRY_RUN" -eq 0 ]]; then
  remote_quiet "cd $REMOTE_DIR && docker compose ps" 2>/dev/null || true
fi
