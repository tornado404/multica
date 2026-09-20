#!/usr/bin/env bash
# Post this workflow run's CI result back to the linked Multica issue.
#
# CI jobs take 10-30 minutes, and Multica only consumes the check-suite
# webhooks to refresh the PR card — nothing notifies the people and agents
# waiting on the result. This script closes that loop: it finds the issue
# identifier (e.g. "ZOYEN-123") in the branch name or PR title, comments the
# conclusion on that issue, and @-mentions the owner agent so Multica
# enqueues a follow-up run (the owner then dispatches the PR reviewer per
# the expert-review conclusion).
#
# Required environment:
#   MULTICA_URL           Multica server base URL
#   MULTICA_TOKEN         personal access token ("mul_...")
#   MULTICA_WORKSPACE_ID  workspace UUID, sent as X-Workspace-ID
# Optional:
#   MULTICA_NOTIFY_AGENT  agent name to @-mention (default 项目负责人;
#                         resolved by name per run — agent UUIDs rotate)
#   MULTICA_DRY_RUN       when set, print the request instead of posting
#   JOB_RESULTS           space-separated needs.*.result values
#   HEAD_BRANCH           branch under test (github.head_ref || github.ref_name)
#   PR_TITLE              pull request title (empty outside pull_request)
#   RUN_URL, COMMIT_SHA   presentation fields for the comment body
#
# Best-effort by contract: every failure path logs and exits 0 so the
# notification can never turn a green CI red.

set -uo pipefail

log() { printf '[notify-multica] %s\n' "$*"; }

skip() { log "skip: $*"; exit 0; }

for v in MULTICA_URL MULTICA_TOKEN MULTICA_WORKSPACE_ID; do
	if [ -z "${!v:-}" ]; then
		skip "$v is not configured (add it as a repo secret/variable to enable notifications)"
	fi
done
for cmd in curl jq; do
	command -v "$cmd" >/dev/null 2>&1 || skip "$cmd is not available on this runner"
done

# Aggregate gate results: the two `needs` jobs use `!cancelled()` gates, so a
# scope-filtered skip already reports success here. `skipped` stays a pass.
conclusion=success
for result in ${JOB_RESULTS:-}; do
	case "$result" in
	failure | cancelled) conclusion=failure ;;
	esac
done

# Same identifier shape the server parses from PR titles and branches
# (identifierRe in server/internal/handler/github.go). Keep this pattern
# equivalent to identifierRe: if the server tightens its regex later, update
# this one in lockstep or notifications silently stop matching. The API accepts it
# case-insensitively; normalize to the server's lowercase form anyway.
identifier=""
for source in "${HEAD_BRANCH:-}" "${PR_TITLE:-}"; do
	[ -n "$source" ] || continue
	match="$(printf '%s\n' "$source" | grep -oEi '[a-z][a-z0-9]{0,9}-[0-9]+' | head -n1 || true)"
	if [ -n "$match" ]; then
		identifier="$(printf '%s\n' "$match" | tr '[:upper:]' '[:lower:]')"
		break
	fi
done
[ -n "$identifier" ] || skip "no issue identifier (e.g. zoyen-123) found in branch '${HEAD_BRANCH:-}' or PR title"

api() {
	curl -sS --max-time 20 --retry 2 \
		-H "Authorization: Bearer $MULTICA_TOKEN" \
		-H "X-Workspace-ID: $MULTICA_WORKSPACE_ID" \
		-H "Content-Type: application/json" \
		"$@"
}

# Resolve the mention target by display name at run time; agent UUIDs rotate
# whenever an agent is recreated, so a hardcoded ID would silently break.
mention=""
if [ -n "${MULTICA_NOTIFY_AGENT:-}" ]; then
	agent_id="$(api "$MULTICA_URL/api/agents/" |
		jq -r --arg name "$MULTICA_NOTIFY_AGENT" '
			(map(select(.name == $name))
			// map(select((.name | gsub("\\s+"; "")) == ($name | gsub("\\s+"; ""))))
			| first | .id) // empty' 2>/dev/null || true)"
	if [ -n "$agent_id" ]; then
		mention="[@${MULTICA_NOTIFY_AGENT}](mention://agent/${agent_id})"
		log "mention target resolved: $MULTICA_NOTIFY_AGENT -> $agent_id"
	else
		log "agent '$MULTICA_NOTIFY_AGENT' not found in workspace; posting without mention"
	fi
fi

status_label="通过 ✅"
[ "$conclusion" = "failure" ] && status_label="失败 ❌"
short_sha="${COMMIT_SHA:-}"
short_sha="${short_sha:0:10}"

content="🤖 CI 完成：**${status_label}**

- 分支：\`${HEAD_BRANCH:-unknown}\`
- 提交：\`${short_sha:-unknown}\`
- [查看 CI 运行](${RUN_URL:-})"
if [ -n "$mention" ]; then
	content="${content}
${mention} CI 已跑完，请结合评审专家的评审结论推进：通过则派「PR 审核员」执行门禁合并，未通过则安排修复。"
fi

payload="$(jq -n --arg content "$content" '{content: $content, type: "comment"}')"

if [ -n "${MULTICA_DRY_RUN:-}" ]; then
	log "dry run: POST $MULTICA_URL/api/issues/$identifier/comments"
	printf '%s\n' "$payload"
	exit 0
fi

body_file="$(mktemp)"
http_code="$(api -o "$body_file" -w '%{http_code}' -X POST -d "$payload" \
	"$MULTICA_URL/api/issues/$identifier/comments")" || http_code="000"
case "$http_code" in
2*)
	log "posted to $identifier: $(head -c 400 "$body_file")"
	;;
*)
	log "warning: POST returned HTTP $http_code: $(head -c 400 "$body_file")"
	;;
esac
rm -f "$body_file"
exit 0
