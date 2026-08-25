#!/usr/bin/env bash
set -euo pipefail

# ==========================================================================
# sync-upstream.sh — sync this fork with multica-ai/multica and rebuild the
# local Desktop GUI + daemon CLI from the synced source.
#
# What it does (each phase is skippable, see FLAGS):
#   1. git fetch upstream (tags included)
#   2. git rebase --autostash <base> (default upstream/main)
#   3. sanity checks: fork migration numbering, go build/test, pnpm typecheck
#   4. make build → install server/bin/multica to:
#        ~/.multica/bin/multica            (Desktop runtime copy)
#        $(realpath $(command -v multica)) (this machine: brew Cellar path)
#   5. package the Electron app (mac arm64, ad-hoc signed) → swap
#      /Applications/Multica.app (previous copy kept as .bak-<version>)
#
# On rebase conflicts the script exits leaving the rebase mid-flight.
# Resolve the conflicts, `git rebase --continue`, then re-run with
# --skip-rebase to finish the build/install phases. See
# .agents/skills/sync-upstream/SKILL.md for the fork's conflict-resolution
# conventions (union lists, migration renumbering).
#
# Usage:
#   bash scripts/sync-upstream.sh [--base <ref>] [--skip-rebase] [--skip-tests]
#                                 [--skip-cli] [--skip-desktop] [--skip-install]
#
#   --base <ref>      Rebase target (default: upstream/main). Use a release
#                     tag (e.g. v0.4.33) to pin a release instead of main.
#   --skip-rebase     Don't touch git; just verify/build/install.
#   --skip-tests      Skip go test / pnpm typecheck (build still runs).
#   --skip-cli        Don't build/install the CLI binary.
#   --skip-desktop    Don't package/install the Desktop app.
#   --skip-install    Build everything but install nothing (smoke run).
# ==========================================================================

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BASE="upstream/main"
SKIP_REBASE=0 SKIP_TESTS=0 SKIP_CLI=0 SKIP_DESKTOP=0 SKIP_INSTALL=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --base) BASE="${2:?--base needs a ref}"; shift 2 ;;
    --skip-rebase) SKIP_REBASE=1; shift ;;
    --skip-tests) SKIP_TESTS=1; shift ;;
    --skip-cli) SKIP_CLI=1; shift ;;
    --skip-desktop) SKIP_DESKTOP=1; shift ;;
    --skip-install) SKIP_INSTALL=1; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

log() { printf '\033[1;34m[sync]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn]\033[0m %s\n' "$*"; }
die() { printf '\033[1;31m[error]\033[0m %s\n' "$*" >&2; exit 1; }

# pnpm/node live under nvm in non-interactive shells; fall back to the
# highest installed nvm node.
ensure_pnpm() {
  if command -v pnpm >/dev/null 2>&1; then return 0; fi
  local candidate
  candidate="$(ls -1 "$HOME/.nvm/versions/node" 2>/dev/null | sort -V | tail -1)"
  if [[ -n "$candidate" && -x "$HOME/.nvm/versions/node/$candidate/bin/pnpm" ]]; then
    export PATH="$HOME/.nvm/versions/node/$candidate/bin:$PATH"
    return 0
  fi
  die "pnpm not found on PATH or under ~/.nvm"
}

# ==========================================================================
# Phase 1: fetch upstream
# ==========================================================================
if [[ "$SKIP_REBASE" -eq 0 ]]; then
  log "fetching upstream (tags)…"
  git remote get-url upstream >/dev/null 2>&1 \
    || die "no 'upstream' remote; add it with: git remote add upstream https://github.com/multica-ai/multica.git"
  # Tag clobbers are non-fatal (local tags may legitimately differ).
  git fetch upstream --tags || git fetch upstream || true

  log "rebasing onto $BASE (autostash)…"
  if ! git rebase --autostash "$BASE"; then
    cat >&2 <<'EOF'

Rebase stopped on a conflict. Resolve it, then:
  git rebase --continue        # repeat until done
  bash scripts/sync-upstream.sh --skip-rebase   # finish verify/build/install

Fork conventions while resolving (details in .agents/skills/sync-upstream/SKILL.md):
  - runtime/provider lists: union of upstream entries + zcode
  - fork migrations must sit AFTER upstream's highest number (rename
    server/migrations/<n>_runtime_profile_add_zcode.* to max+1 and refresh
    the CHECK whitelist inside the .up.sql from upstream's latest)
EOF
    exit 3
  fi
  log "rebase complete: $(git log --oneline -1)"
fi

# ==========================================================================
# Phase 2: sanity checks
# ==========================================================================
# Fork-only migrations must not collide with upstream's numbering and must
# sort after upstream's highest number (golang-migrate applies by number).
log "checking fork migration numbering against ${BASE}…"
max_upstream="$(git ls-tree --name-only "$BASE" -- server/migrations/ 2>/dev/null | sed -nE 's#.*/([0-9]+)_.*#\1#p' | sort -n | tail -1 || true)"
max_upstream="${max_upstream:-0}"
max_local="$(ls server/migrations/ | sed -nE 's/^([0-9]+)_.*/\1/p' | sort -n | tail -1 || true)"
max_local="${max_local:-0}"
if [[ "$max_local" -le "$max_upstream" ]]; then
  warn "local migration max ($max_local) <= $BASE max ($max_upstream) — fork migrations were renumbered upstream-side or are missing; verify server/migrations/"
fi

if [[ "$SKIP_TESTS" -eq 0 ]]; then
  log "go build + tests (agent / daemon / cli packages)…"
  (cd server && go build ./... && go test ./pkg/agent/ ./internal/daemon/ ./cmd/multica/ -count=1)
  ensure_pnpm
  log "pnpm install (upstream may have added workspace packages)…"
  pnpm install --frozen-lockfile || pnpm install
  log "pnpm typecheck…"
  pnpm typecheck
else
  log "skipping tests (--skip-tests)"
  (cd server && go build ./...) # build is always cheap insurance
fi

# ==========================================================================
# Phase 3: CLI build + install
# ==========================================================================
if [[ "$SKIP_CLI" -eq 0 ]]; then
  log "building CLI (make build)…"
  make build
  NEW_CLI="$ROOT/server/bin/multica"
  [[ -x "$NEW_CLI" ]] || die "expected CLI at $NEW_CLI"
  log "built: $("$NEW_CLI" version | head -1)"

  if [[ "$SKIP_INSTALL" -eq 0 ]]; then
    STAMP="$(date +%Y%m%d-%H%M%S)"
    install_with_backup() {
      local dest="$1"
      [[ -f "$dest" ]] && cp "$dest" "$dest.bak-$STAMP" && log "backed up → $dest.bak-$STAMP"
      cp "$NEW_CLI" "$dest" && log "installed → $dest"
    }
    # Desktop runtime copy
    mkdir -p "$HOME/.multica/bin"
    install_with_backup "$HOME/.multica/bin/multica"
    # Whatever `multica` resolves to on PATH (symlink followed) — on this
    # machine that is the brew Cellar binary overwritten with fork builds.
    if command -v multica >/dev/null 2>&1; then
      CLI_TARGET="$(realpath "$(command -v multica)")"
      if [[ "$CLI_TARGET" == "$HOME/.multica/bin/multica" ]]; then
        : # same file, already handled
      elif [[ -w "$CLI_TARGET" ]]; then
        install_with_backup "$CLI_TARGET"
      else
        warn "PATH multica resolves to $CLI_TARGET (not writable without sudo) — skipped; sudo cp $NEW_CLI $CLI_TARGET"
      fi
    fi
  fi
else
  log "skipping CLI (--skip-cli)"
fi

# ==========================================================================
# Phase 4: Desktop app package + install
# ==========================================================================
if [[ "$SKIP_DESKTOP" -eq 0 ]]; then
  ensure_pnpm
  log "installing JS deps (lockfile may have moved upstream)…"
  pnpm install --frozen-lockfile || pnpm install
  log "packaging Desktop (mac arm64, ad-hoc signature)…"
  # package.mjs compiles the matching Go CLI into the app automatically.
  CSC_IDENTITY_AUTO_DISCOVERY=false pnpm --filter @multica/desktop run package -- --mac --arm64

  if [[ "$SKIP_INSTALL" -eq 0 ]]; then
    APP_SRC="$(find "$ROOT/apps/desktop/dist" -maxdepth 2 -name 'Multica.app' -type d -print -quit)"
    [[ -n "$APP_SRC" ]] || die "no Multica.app under apps/desktop/dist after packaging"
    APP_VERSION="$(defaults read "$APP_SRC/Contents/Info.plist" CFBundleShortVersionString)"
    log "packaged Multica.app ($APP_VERSION) at $APP_SRC"

    if pgrep -f '/Applications/Multica.app' >/dev/null 2>&1; then
      log "quitting running Multica.app…"
      osascript -e 'quit app "Multica"' || true
      for _ in $(seq 1 20); do
        pgrep -f '/Applications/Multica.app' >/dev/null 2>&1 || break
        sleep 1
      done
      if pgrep -f '/Applications/Multica.app' >/dev/null 2>&1; then
        die "Multica.app still running; close it and re-run with --skip-rebase --skip-cli"
      fi
    fi

    if [[ -d /Applications/Multica.app ]]; then
      OLD_VERSION="$(defaults read /Applications/Multica.app/Contents/Info.plist CFBundleShortVersionString)"
      rm -rf "/Applications/Multica.app.bak-$OLD_VERSION"
      mv /Applications/Multica.app "/Applications/Multica.app.bak-$OLD_VERSION"
      log "backed up old app → /Applications/Multica.app.bak-$OLD_VERSION"
    fi
    cp -R "$APP_SRC" /Applications/Multica.app
    log "installed → /Applications/Multica.app"
  fi
else
  log "skipping Desktop (--skip-desktop)"
fi

# ==========================================================================
# Phase 5: daemon restart (opt-in: may interrupt running agent tasks)
# ==========================================================================
if [[ "$SKIP_INSTALL" -eq 0 && "$SKIP_CLI" -eq 0 ]]; then
  log "daemon note: restart daemons to pick up the new binary, e.g.:"
  echo "    multica daemon restart                       # default profile"
  for profile_dir in "$HOME"/.multica/profiles/*/; do
    [[ -d "$profile_dir" ]] || continue
    profile="$(basename "$profile_dir")"
    echo "    multica --profile $profile daemon restart"
  done
  echo "  (check for active tasks first: multica [--profile <name>] daemon logs | tail)"
fi

log "done. Branch: $(git branch --show-current) @ $(git rev-parse --short HEAD)"
