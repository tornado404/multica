#!/usr/bin/env bash
set -euo pipefail

# ==========================================================================
# auto-update-selfhost.sh — keep the cloud self-host deployment on the latest
# upstream multica release. Scheduled weekly by install-autoupdate.sh
# (launchd); also fine to run by hand.
#
# Loop (never builds on the server, never touches origin):
#   1. git fetch upstream --tags; latest v* release reachable from
#      upstream/main vs. LAST_SYNCED_TAG in the state file → exit if current
#   2. sync-upstream.sh --base <tag> --skip-tests --skip-cli --skip-desktop
#      (rebase + go build only; conflicts → exit 3)
#   3. deploy-selfhost.sh --tag <tag>-zcode.<sha> --auto-rollback
#      (build on Mac → ssh ship → compose up -d → health check → roll back
#      to the previous tag if unhealthy)
#
# Rebase conflicts cannot be resolved unattended (conventions live in
# .agents/skills/sync-upstream/SKILL.md), so on exit 3 this script marks
# STATUS=NEEDS_MANUAL_SYNC, sends a macOS notification, and later runs only
# remind — they never pile up. After resolving (`git rebase --continue` until
# done), finish the cycle with:
#
#   bash scripts/auto-update-selfhost.sh --resume
#
# Flags:
#   --resume    finish a conflict-stopped cycle: re-run sync checks with
#               --skip-rebase, then build+deploy the resolved HEAD
#   --force     deploy even when LAST_SYNCED_TAG already matches the newest
#               upstream release (retry a failed deploy / redeploy HEAD)
#   --no-notify skip macOS notifications
#
# State: ~/.multica-fork-sync/state.env (shared with deploy-selfhost.sh)
#        STATUS, LAST_SYNCED_TAG, LAST_SYNC_ATTEMPT_TAG,
#        LAST_DEPLOYED_TAG, PREV_DEPLOYED_TAG
# Log:   ~/.multica-fork-sync/update.log
# ==========================================================================

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

RESUME=0 FORCE=0 NOTIFY=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --resume) RESUME=1; shift ;;
    --force) FORCE=1; shift ;;
    --no-notify) NOTIFY=0; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

STATE_DIR="${MULTICA_FORK_STATE_DIR:-$HOME/.multica-fork-sync}"
STATE_FILE="$STATE_DIR/state.env"
mkdir -p "$STATE_DIR"

# Console when run by hand, file-only when launched by launchd.
if [[ -t 1 ]]; then
  exec > >(tee -a "$STATE_DIR/update.log") 2>&1
else
  exec >> "$STATE_DIR/update.log" 2>&1
fi

log()  { printf '%s [auto-update] %s\n' "$(date '+%F %T')" "$*"; }
die()  { printf '%s [auto-update][error] %s\n' "$(date '+%F %T')" "$*" >&2; exit 1; }

notify() { # title message
  [[ "$NOTIFY" -eq 1 ]] || return 0
  [[ "$(uname)" = "Darwin" ]] || return 0
  osascript -e "display notification \"$2\" with title \"$1\"" >/dev/null 2>&1 || true
}

state_get() {
  [[ -f "$STATE_FILE" ]] || return 0
  grep -E "^$1=" "$STATE_FILE" 2>/dev/null | tail -n 1 | cut -d= -f2- || true
}
state_set() {
  mkdir -p "$STATE_DIR"; touch "$STATE_FILE"
  if grep -qE "^$1=" "$STATE_FILE" 2>/dev/null; then
    if [[ "$(uname)" = "Darwin" ]]; then sed -i '' "s#^$1=.*#$1=$2#" "$STATE_FILE"
    else sed -i "s#^$1=.*#$1=$2#" "$STATE_FILE"; fi
  else
    printf '%s=%s\n' "$1" "$2" >> "$STATE_FILE"
  fi
}

rebase_in_progress() {
  local p
  p="$(git rev-parse --git-path rebase-merge 2>/dev/null)"; [[ -d "$p" ]] && return 0
  p="$(git rev-parse --git-path rebase-apply 2>/dev/null)"; [[ -d "$p" ]]
}

latest_upstream_tag() {
  git tag --list 'v[0-9]*' --sort=-v:refname --merged upstream/main 2>/dev/null | head -n 1 || true
}

# One run at a time (launchd + manual runs can overlap).
LOCK="$STATE_DIR/lock"
if ! mkdir "$LOCK" 2>/dev/null; then
  log "another update run holds the lock — exiting"
  exit 0
fi
trap 'rmdir "$LOCK" 2>/dev/null || true' EXIT

derive_deploy_tag() {
  local t
  t="$(git tag --list 'v[0-9]*' --sort=-v:refname --merged HEAD 2>/dev/null | head -n 1 || true)"
  [[ -n "$t" ]] || t="$(git tag --list 'v[0-9]*' --sort=-v:refname 2>/dev/null | head -n 1 || true)"
  [[ -n "$t" ]] || { log "no v* tag reachable from HEAD — cannot derive deploy tag"; return 1; }
  printf '%s-zcode.%s' "$t" "$(git rev-parse --short HEAD)"
}

deploy_current_head() { # $1 = upstream release tag for bookkeeping
  local deploy_tag
  deploy_tag="$(derive_deploy_tag)" || return 1
  log "deploying $deploy_tag (auto-rollback enabled)…"
  if bash scripts/deploy-selfhost.sh --tag "$deploy_tag" --auto-rollback; then
    state_set LAST_SYNCED_TAG "$1"
    state_set STATUS "OK"
    notify "Multica 自托管已更新" "✓ $deploy_tag 部署成功（服务器 $MULTICA_SSH_HOST）"
    log "✓ deployed $deploy_tag"
    return 0
  fi
  notify "Multica 部署失败" "✗ $deploy_tag 部署失败，已尝试回滚；日志: $STATE_DIR/update.log"
  die "deploy of $deploy_tag failed (auto-rollback attempted; see log above)"
}

# ---- resume: finish a conflict-stopped cycle --------------------------------
if [[ "$RESUME" -eq 1 ]]; then
  log "--resume: finishing the interrupted sync on branch $(git branch --show-current)"
  if rebase_in_progress; then
    notify "Multica resume 受阻" "rebase 仍未完成；解决并 git rebase --continue 后再 --resume"
    die "rebase still in progress — resolve, 'git rebase --continue' until done, then rerun --resume"
  fi
  if ! bash scripts/sync-upstream.sh --skip-rebase --skip-tests --skip-cli --skip-desktop; then
    notify "Multica resume 失败" "sync 检查未通过，见 $STATE_DIR/update.log"
    die "sync-upstream.sh --skip-rebase failed"
  fi
  ATTEMPT="$(state_get LAST_SYNC_ATTEMPT_TAG)"
  [[ -n "$ATTEMPT" ]] || ATTEMPT="$(latest_upstream_tag)"
  deploy_current_head "${ATTEMPT:-unknown}"
  exit 0
fi

# ---- scheduled/manual check ---------------------------------------------------
if rebase_in_progress; then
  notify "Multica 更新待处理" "存在未完成的 rebase；解决冲突后运行 scripts/auto-update-selfhost.sh --resume"
  log "rebase in progress — needs manual resolution; exiting"
  exit 0
fi

if [[ "$(state_get STATUS)" = "NEEDS_MANUAL_SYNC" ]]; then
  notify "Multica 更新待处理" "上次同步有 rebase 冲突（$(state_get LAST_SYNC_ATTEMPT_TAG)）；解决后运行 scripts/auto-update-selfhost.sh --resume"
  log "STATUS=NEEDS_MANUAL_SYNC — reminding, exiting"
  exit 0
fi

docker info >/dev/null 2>&1 || {
  notify "Multica 自动更新跳过" "Docker Desktop 未运行"
  log "Docker not running — skipping this run"
  exit 0
}

log "fetching upstream (tags)…"
if ! git fetch upstream --tags --quiet; then
  git fetch upstream --quiet || { notify "Multica 自动更新失败" "git fetch upstream 失败"; die "git fetch upstream failed"; }
fi

LATEST="$(latest_upstream_tag)"
[[ -n "$LATEST" ]] || { log "no v* release tag found on upstream/main — nothing to do"; exit 0; }
CURRENT="$(state_get LAST_SYNCED_TAG)"
log "latest upstream release: $LATEST (last synced: ${CURRENT:-none})"

# Up-to-date = same release AND the deployed image already matches HEAD.
# Anything else proceeds, so a moved HEAD (manual main-tip sync) or a
# previously failed deploy retries without waiting for a new release.
HEAD_TAG_PREVIEW="$(derive_deploy_tag 2>/dev/null || true)"
if [[ "$LATEST" == "$CURRENT" && "$FORCE" -eq 0 && -n "$HEAD_TAG_PREVIEW" && "$HEAD_TAG_PREVIEW" == "$(state_get LAST_DEPLOYED_TAG)" ]]; then
  log "already synced to $LATEST and deployed ($HEAD_TAG_PREVIEW) — up to date"
  exit 0
fi

if [[ -z "${MULTICA_SSH_HOST:-}" ]]; then
  notify "Multica 自动更新受阻" "MULTICA_SSH_HOST 未设置 — 请在环境或 launchd plist 中配置"
  die "MULTICA_SSH_HOST is not set (needed by deploy-selfhost.sh)"
fi

state_set LAST_SYNC_ATTEMPT_TAG "$LATEST"

# If HEAD already contains the release tag (branch rebased onto main tip past
# it, or a previous sync landed it), rebase would REWIND the branch — skip it.
if git merge-base --is-ancestor "$LATEST" HEAD 2>/dev/null; then
  log "HEAD already contains $LATEST — rebase skipped"
  SYNC_RC=0
else
  log "syncing to $LATEST (rebase + go build only)…"
  set +e
  bash scripts/sync-upstream.sh --base "$LATEST" --skip-tests --skip-cli --skip-desktop
  SYNC_RC=$?
  set -e
fi

case "$SYNC_RC" in
  0)
    log "sync to $LATEST complete"
    ;;
  3)
    state_set STATUS "NEEDS_MANUAL_SYNC"
    notify "Multica 更新需手动处理" "rebase 到 $LATEST 有冲突。解决后运行: bash scripts/auto-update-selfhost.sh --resume（约定见 sync-upstream SKILL.md）"
    log "rebase conflict on $LATEST — marked NEEDS_MANUAL_SYNC, stopping"
    exit 0
    ;;
  *)
    notify "Multica 自动更新失败" "sync-upstream.sh 退出码 $SYNC_RC；见 $STATE_DIR/update.log"
    die "sync-upstream.sh exited with $SYNC_RC"
    ;;
esac

deploy_current_head "$LATEST"
