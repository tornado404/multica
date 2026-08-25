#!/usr/bin/env bash
set -euo pipefail

# ==========================================================================
# install-autoupdate.sh — register (or remove) the weekly launchd agent that
# runs scripts/auto-update-selfhost.sh to keep the cloud self-host deployment
# on the latest upstream multica release.
#
# The plist embeds MULTICA_SSH_HOST (and MULTICA_REMOTE_DIR if set) because
# launchd agents do not inherit your shell environment. Re-running this
# script rewrites the plist and reloads the agent.
#
# Usage:
#   MULTICA_SSH_HOST=my-server bash scripts/install-autoupdate.sh
#   MULTICA_SSH_HOST=my-server bash scripts/install-autoupdate.sh --day 1 --hour 9
#   bash scripts/install-autoupdate.sh --uninstall
#
#   --day N     launchd Weekday: 1=Monday … 6=Saturday, 0/7=Sunday (default 6)
#   --hour H    24h hour (default 10)
#   --minute M  (default 0)
#
# After installing, test the full chain immediately:
#   launchctl kickstart -k gui/$(id -u)/com.multica.fork.autoupdate
#   tail -f ~/.multica-fork-sync/update.log
# ==========================================================================

LABEL="com.multica.fork.autoupdate"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
UPDATE_SCRIPT="$REPO_ROOT/scripts/auto-update-selfhost.sh"
STATE_DIR="${MULTICA_FORK_STATE_DIR:-$HOME/.multica-fork-sync}"

DAY=6 HOUR=10 MINUTE=0 UNINSTALL=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --day) DAY="${2:?--day needs 1-7}"; shift 2 ;;
    --hour) HOUR="${2:?--hour needs 0-23}"; shift 2 ;;
    --minute) MINUTE="${2:?--minute needs 0-59}"; shift 2 ;;
    --uninstall) UNINSTALL=1; shift ;;
    *) echo "unknown flag: $1" >&2; exit 2 ;;
  esac
done

UID_="$(id -u)"

bootout() { launchctl bootout "gui/$UID_/$LABEL" 2>/dev/null || true; }

if [[ "$UNINSTALL" -eq 1 ]]; then
  bootout
  rm -f "$PLIST"
  echo "✓ removed $LABEL (plist deleted, agent unloaded)"
  exit 0
fi

[[ -f "$UPDATE_SCRIPT" ]] || { echo "missing $UPDATE_SCRIPT" >&2; exit 1; }
SSH_HOST="${MULTICA_SSH_HOST:-}"
[[ -n "$SSH_HOST" ]] || { echo "MULTICA_SSH_HOST is required (ssh alias), e.g. MULTICA_SSH_HOST=my-server bash scripts/install-autoupdate.sh" >&2; exit 1; }

REMOTE_DIR_BLOCK=""
if [[ -n "${MULTICA_REMOTE_DIR:-}" ]]; then
  REMOTE_DIR_BLOCK="
    <key>MULTICA_REMOTE_DIR</key>
    <string>$MULTICA_REMOTE_DIR</string>"
fi

mkdir -p "$HOME/Library/LaunchAgents" "$STATE_DIR"

cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string>
    <string>$UPDATE_SCRIPT</string>
  </array>
  <key>WorkingDirectory</key>
  <string>$REPO_ROOT</string>
  <key>StartCalendarInterval</key>
  <dict>
    <key>Weekday</key>
    <integer>$DAY</integer>
    <key>Hour</key>
    <integer>$HOUR</integer>
    <key>Minute</key>
    <integer>$MINUTE</integer>
  </dict>
  <key>EnvironmentVariables</key>
  <dict>
    <key>MULTICA_SSH_HOST</key>
    <string>$SSH_HOST</string>$REMOTE_DIR_BLOCK
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>StandardOutPath</key>
  <string>$STATE_DIR/launchd.out.log</string>
  <key>StandardErrorPath</key>
  <string>$STATE_DIR/launchd.err.log</string>
</dict>
</plist>
EOF

bootout
if launchctl bootstrap "gui/$UID_" "$PLIST" 2>/dev/null; then
  :
else
  launchctl load "$PLIST"  # older fallback
fi

echo "✓ installed $LABEL"
echo "  schedule: weekly (launchd Weekday=$DAY, $HOUR:$(printf '%02d' "$MINUTE")) — Mac must be awake; launchd catches up after sleep"
echo "  server:   $SSH_HOST${MULTICA_REMOTE_DIR:+ ($MULTICA_REMOTE_DIR)}"
echo "  log:      $STATE_DIR/update.log"
echo ""
echo "Test the whole chain now (no waiting for the schedule):"
echo "  launchctl kickstart -k gui/$UID_/$LABEL"
echo "  tail -f $STATE_DIR/update.log"
