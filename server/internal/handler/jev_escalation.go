package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/multica-ai/multica/server/internal/service"
	"github.com/multica-ai/multica/server/internal/util"
	db "github.com/multica-ai/multica/server/pkg/db/generated"

	"github.com/jackc/pgx/v5/pgtype"
)

// Jev never receives image information: the System One request state is text
// by design, and a judgment made without an image it cannot see is silently
// incomplete evidence. When a claimed task for a jev agent involves images —
// attachment rows with an image content type, or image markdown embedded in
// the judgment input — the claim is not delivered.
//
// Issue-bound tasks escalate instead of silently judging the text: the server
// cancels the jev run, posts an escalation comment authored by the jev agent,
// and enqueues the configured LLM agent through the same mention path a human
// @ would take (EnqueueTaskForMention, so attribution, dedup and triage rules
// apply unchanged). The escalation target lives on the jev agent's
// runtime_config as jev_escalation_agent_id; a missing or invalid target
// cancels the run with a visible comment explaining what to configure.
//
// Chat tasks have no issue to comment on and no cross-agent chat-handoff
// machinery, so they keep filter semantics: jev judges the message text and
// image attachments are simply never part of its input. Run-only autopilots
// (no issue) fall under the same filter semantics — there is nothing to
// escalate onto.

// jevEscalationAgentConfigKey is the runtime_config key naming the LLM agent
// that image-involving jev tasks are escalated to.
const jevEscalationAgentConfigKey = "jev_escalation_agent_id"

// jevImageMarkdownPattern matches embedded images in markdown state text —
// the one way an image can ride inside text the jev backend would judge.
var jevImageMarkdownPattern = regexp.MustCompile(`!\[[^\]]*\]\([^)\s]*(?:\s+"[^"]*")?\)`)

// jevQuotedMentionPattern matches mention links in the state being quoted
// into the escalation comment. Quoted links must not survive verbatim: the
// comment is scanned for mentions like any other, so a link inside the
// original task text would enqueue that agent alongside the intended target.
var jevQuotedMentionPattern = regexp.MustCompile(`\[[^\]]*\]\(mention://agent/[^)]*\)`)

// guardJevImageEscalation inspects a claimed task for image involvement and,
// for jev agents, reroutes the run. It returns a claimBuildFailure when the
// claim must not be delivered (escalated, cancelled, or failed), and nil when
// the task is not jev, involves no images, or is a chat/run-only task that
// keeps filter semantics. The context is request-scoped: the task has been
// claimed by this call, so every terminal path here must settle it (cancel)
// before returning.
func (h *Handler) guardJevImageEscalation(r *http.Request, task *db.AgentTaskQueue, runtime db.AgentRuntime, agent db.Agent) *claimBuildFailure {
	if runtime.Provider != "jev" || !task.IssueID.Valid || task.ChatSessionID.Valid {
		return nil
	}
	ctx := r.Context()
	issue, err := h.Queries.GetIssue(ctx, task.IssueID)
	if err != nil {
		// The claim's own issue branch rejects this task shortly after; the
		// guard has nothing authoritative to scan without the issue row.
		slog.Error("jev image guard: issue load failed; requeueing claim",
			"task_id", uuidToString(task.ID), "error", err)
		if _, requeueErr := h.TaskService.RequeueTaskAfterClaimFailure(ctx, *task); requeueErr != nil {
			slog.Error("jev image guard: requeue after issue load failure failed", "task_id", uuidToString(task.ID), "error", requeueErr)
		}
		return &claimBuildFailure{
			outcome: "error_jev_image_scan_failed",
			status:  http.StatusInternalServerError,
			message: "failed to load issue for jev image guard",
		}
	}

	involved, imageAtts, stateText, err := h.jevImageInvolvement(ctx, task, issue)
	if err != nil {
		// Detection itself failed (source rows unreadable). Requeue rather than
		// risk either a silent text-only judgment or a wrong escalation: the
		// stale-reclaim machinery redelivers once the store is healthy.
		slog.Error("jev image guard: attachment scan failed; requeueing claim",
			"task_id", uuidToString(task.ID), "error", err)
		if _, requeueErr := h.TaskService.RequeueTaskAfterClaimFailure(ctx, *task); requeueErr != nil {
			slog.Error("jev image guard: requeue after scan failure failed", "task_id", uuidToString(task.ID), "error", requeueErr)
		}
		return &claimBuildFailure{
			outcome: "error_jev_image_scan_failed",
			status:  http.StatusInternalServerError,
			message: "failed to inspect attachments for jev image guard",
		}
	}
	if !involved {
		return nil
	}

	target, why := h.resolveJevEscalationTarget(ctx, task, agent, issue.WorkspaceID)
	if target == nil {
		slog.Warn("jev image guard: no usable escalation target; cancelling task",
			"task_id", uuidToString(task.ID), "agent_id", uuidToString(agent.ID), "reason", why)
		h.cancelJevTaskWithNote(ctx, task, agent, jevEscalationNoTargetComment(why))
		return &claimBuildFailure{
			outcome: "error_jev_image_no_escalation_target",
			status:  http.StatusConflict,
			message: "jev image task has no usable escalation agent configured",
		}
	}

	if failure := h.escalateJevTaskToAgent(ctx, task, &agent, target, issue, stateText, imageAtts); failure != nil {
		return failure
	}
	return &claimBuildFailure{
		outcome: "error_jev_image_escalated",
		status:  http.StatusConflict,
		message: "jev image task escalated to LLM agent " + uuidToString(target.ID),
	}
}

// jevImageInvolvement reports whether the task's judgment input involves
// images, returning the image attachments found and the composed state text
// (the same input the jev backend would judge) for the escalation comment.
func (h *Handler) jevImageInvolvement(ctx context.Context, task *db.AgentTaskQueue, issue db.Issue) (bool, []db.Attachment, string, error) {
	var imageAtts []db.Attachment
	collectImages := func(atts []db.Attachment) {
		for _, att := range atts {
			if strings.HasPrefix(strings.ToLower(att.ContentType), "image/") {
				imageAtts = append(imageAtts, att)
			}
		}
	}

	issueAtts, err := h.Queries.ListAttachmentsByIssue(ctx, db.ListAttachmentsByIssueParams{
		IssueID:     task.IssueID,
		WorkspaceID: issue.WorkspaceID,
	})
	if err != nil {
		return false, nil, "", fmt.Errorf("list issue attachments: %w", err)
	}
	collectImages(issueAtts)

	commentIDs := make([]pgtype.UUID, 0, 1+len(task.CoalescedCommentIds))
	if task.TriggerCommentID.Valid {
		commentIDs = append(commentIDs, task.TriggerCommentID)
	}
	commentIDs = append(commentIDs, task.CoalescedCommentIds...)
	if len(commentIDs) > 0 {
		commentAtts, err := h.Queries.ListAttachmentsByCommentIDs(ctx, db.ListAttachmentsByCommentIDsParams{
			Column1:     commentIDs,
			WorkspaceID: issue.WorkspaceID,
		})
		if err != nil {
			return false, nil, "", fmt.Errorf("list comment attachments: %w", err)
		}
		collectImages(commentAtts)
	}

	var triggerComment *db.Comment
	if task.TriggerCommentID.Valid {
		comment, err := h.Queries.GetComment(ctx, task.TriggerCommentID)
		if err != nil {
			return false, nil, "", fmt.Errorf("load trigger comment: %w", err)
		}
		if comment.IssueID == task.IssueID {
			triggerComment = &comment
		}
	}
	stateText := jevTaskStateText(ctx, h, task, triggerComment)

	involved := len(imageAtts) > 0 || jevImageMarkdownPattern.MatchString(stateText)
	return involved, imageAtts, stateText, nil
}

// jevTaskStateText mirrors the daemon's buildJevStatePrompt composition for
// issue-bound triggers: the text the jev backend would receive as System One
// state. Chat and quick-create inputs are absent — those tasks never reach
// this guard (no issue to escalate on).
func jevTaskStateText(ctx context.Context, h *Handler, task *db.AgentTaskQueue, triggerComment *db.Comment) string {
	switch {
	case triggerComment != nil:
		return triggerComment.Content
	case task.AutopilotRunID.Valid:
		// Same rows the claim's autopilot branch loads: instructions plus the
		// trigger payload are the whole text state an autopilot dispatch
		// would judge over.
		run, err := h.Queries.GetAutopilotRun(ctx, task.AutopilotRunID)
		if err != nil {
			slog.Warn("jev image guard: autopilot run load failed; scanning handoff only",
				"task_id", uuidToString(task.ID), "error", err)
			return task.HandoffNote.String
		}
		ap, err := h.Queries.GetAutopilot(ctx, run.AutopilotID)
		if err != nil {
			slog.Warn("jev image guard: autopilot load failed; scanning handoff only",
				"task_id", uuidToString(task.ID), "error", err)
			return task.HandoffNote.String
		}
		var b strings.Builder
		if ap.Description.Valid && strings.TrimSpace(ap.Description.String) != "" {
			b.WriteString(ap.Description.String)
		} else {
			b.WriteString(ap.Title)
		}
		if len(run.TriggerPayload) > 0 {
			b.WriteString("\n\n")
			b.WriteString(string(run.TriggerPayload))
		}
		return b.String()
	default:
		return task.HandoffNote.String
	}
}

// resolveJevEscalationTarget loads and validates the configured escalation
// agent. A nil return means unusable; why explains it for the operator-facing
// comment and logs.
func (h *Handler) resolveJevEscalationTarget(ctx context.Context, task *db.AgentTaskQueue, agent db.Agent, workspaceID pgtype.UUID) (*db.Agent, string) {
	raw := jevEscalationAgentIDFromRuntimeConfig(agent.RuntimeConfig)
	if raw == "" {
		return nil, "runtime_config has no " + jevEscalationAgentConfigKey
	}
	targetID, err := util.ParseUUID(raw)
	if err != nil {
		return nil, jevEscalationAgentConfigKey + " is not a valid agent id"
	}
	target, err := h.Queries.GetAgent(ctx, targetID)
	if err != nil {
		return nil, jevEscalationAgentConfigKey + " does not reference an existing agent"
	}
	if target.WorkspaceID != workspaceID {
		return nil, jevEscalationAgentConfigKey + " references an agent from another workspace"
	}
	if target.ArchivedAt.Valid {
		return nil, "escalation agent " + target.Name + " is archived"
	}
	if target.ID == agent.ID {
		return nil, "escalation agent is the jev agent itself"
	}
	if !target.RuntimeID.Valid {
		return nil, "escalation agent has no runtime"
	}
	targetRuntime, err := h.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
		ID:          target.RuntimeID,
		WorkspaceID: target.WorkspaceID,
	})
	if err != nil {
		return nil, "escalation agent runtime could not be loaded"
	}
	// Loop guard: escalating to another jev agent would re-enter this guard
	// and cancel again without any LLM ever judging.
	if targetRuntime.Provider == "jev" {
		return nil, "escalation agent must run on a non-jev runtime"
	}
	return &target, ""
}

// escalateJevTaskToAgent cancels the jev run and hands the judgment to the
// target agent: an escalation comment carrying the original state and the
// image attachments, enqueued as the target's trigger via the mention path.
// The jev task is cancelled last so a failed enqueue leaves the task visible
// rather than silently dropped.
func (h *Handler) escalateJevTaskToAgent(ctx context.Context, task *db.AgentTaskQueue, agent, target *db.Agent, issue db.Issue, stateText string, imageAtts []db.Attachment) *claimBuildFailure {
	content := jevEscalationComment(agent.Name, target, stateText, imageAtts)
	var parentID pgtype.UUID
	if task.TriggerCommentID.Valid {
		parentID = task.TriggerCommentID
	}
	comment, err := h.TaskService.CreateAgentComment(ctx, task.IssueID, agent.ID, content, "comment", parentID, task.ID)
	if err != nil {
		slog.Error("jev image guard: escalation comment failed", "task_id", uuidToString(task.ID), "error", err)
		return &claimBuildFailure{
			outcome: "error_jev_image_escalation_failed",
			status:  http.StatusInternalServerError,
			message: "failed to post jev escalation comment",
		}
	}

	if _, err := h.TaskService.EnqueueTaskForMention(ctx, issue, target.ID, comment.ID, service.OriginNamed); err != nil {
		slog.Error("jev image guard: escalation enqueue failed; cancelling jev task anyway", "task_id", uuidToString(task.ID), "target_agent", uuidToString(target.ID), "error", err)
		h.cancelJevTaskQuietly(ctx, task)
		return &claimBuildFailure{
			outcome: "error_jev_image_escalation_failed",
			status:  http.StatusInternalServerError,
			message: "failed to enqueue escalation agent for jev image task",
		}
	}

	h.cancelJevTaskQuietly(ctx, task)
	slog.Info("jev image guard: task escalated to LLM agent",
		"task_id", uuidToString(task.ID),
		"jev_agent", uuidToString(agent.ID),
		"target_agent", uuidToString(target.ID),
		"escalation_comment", uuidToString(comment.ID),
		"issue", uuidToString(task.IssueID),
		"image_attachments", len(imageAtts))
	return nil
}

// cancelJevTaskWithNote posts an operator-facing explanation authored by the
// jev agent, then cancels the task. Used when escalation cannot proceed so
// the surface shows why nothing ran.
func (h *Handler) cancelJevTaskWithNote(ctx context.Context, task *db.AgentTaskQueue, agent db.Agent, note string) {
	if _, err := h.TaskService.CreateAgentComment(ctx, task.IssueID, agent.ID, note, "system", pgtype.UUID{}, task.ID); err != nil {
		slog.Warn("jev image guard: cancel note comment failed", "task_id", uuidToString(task.ID), "error", err)
	}
	h.cancelJevTaskQuietly(ctx, task)
}

func (h *Handler) cancelJevTaskQuietly(ctx context.Context, task *db.AgentTaskQueue) {
	if _, err := h.TaskService.CancelTask(ctx, task.ID); err != nil {
		slog.Error("jev image guard: cancel claimed jev task failed; stale reclaim will settle it",
			"task_id", uuidToString(task.ID), "error", err)
	}
}

func jevEscalationAgentIDFromRuntimeConfig(runtimeConfig []byte) string {
	if len(runtimeConfig) == 0 {
		return ""
	}
	var rc struct {
		EscalationAgentID string `json:"jev_escalation_agent_id"`
	}
	if json.Unmarshal(runtimeConfig, &rc) != nil {
		return ""
	}
	return strings.TrimSpace(rc.EscalationAgentID)
}

// validateJevEscalationTarget is the save-time gate for
// runtime_config.jev_escalation_agent_id on jev agents. An absent or empty
// value is allowed (image tasks then cancel with a visible note until an
// operator configures one); a present value must name an existing, unarchived
// agent of the same workspace running on a non-jev runtime — the same shape
// the claim-time guard needs, enforced where the operator can fix it. The
// agent being created has no id yet, so selfID is nil on the create path.
func (h *Handler) validateJevEscalationTarget(ctx context.Context, workspaceID pgtype.UUID, runtimeConfig []byte, selfID *pgtype.UUID) error {
	raw := jevEscalationAgentIDFromRuntimeConfig(runtimeConfig)
	if raw == "" {
		return nil
	}
	targetID, err := util.ParseUUID(raw)
	if err != nil {
		return fmt.Errorf("%s is not a valid agent id", jevEscalationAgentConfigKey)
	}
	target, err := h.Queries.GetAgent(ctx, targetID)
	if err != nil {
		return fmt.Errorf("%s references an agent that does not exist", jevEscalationAgentConfigKey)
	}
	if target.WorkspaceID != workspaceID {
		return fmt.Errorf("%s must reference an agent from the same workspace", jevEscalationAgentConfigKey)
	}
	if target.ArchivedAt.Valid {
		return fmt.Errorf("%s references archived agent %q", jevEscalationAgentConfigKey, target.Name)
	}
	if selfID != nil && target.ID == *selfID {
		return fmt.Errorf("%s cannot reference the jev agent itself", jevEscalationAgentConfigKey)
	}
	if !target.RuntimeID.Valid {
		return fmt.Errorf("%s references agent %q which has no runtime", jevEscalationAgentConfigKey, target.Name)
	}
	targetRuntime, err := h.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
		ID:          target.RuntimeID,
		WorkspaceID: target.WorkspaceID,
	})
	if err != nil {
		return fmt.Errorf("%s references agent %q whose runtime could not be loaded", jevEscalationAgentConfigKey, target.Name)
	}
	if targetRuntime.Provider == "jev" {
		return fmt.Errorf("%s must reference an agent on a non-jev runtime; a jev runtime cannot judge images", jevEscalationAgentConfigKey)
	}
	return nil
}

// jevEscalationComment builds the escalation comment: the mention that
// triggers the target agent's run, the original judgment input, and the image
// attachments listed with download ids so the LLM agent can actually see the
// images this run is about.
func jevEscalationComment(jevAgentName string, target *db.Agent, stateText string, imageAtts []db.Attachment) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[jev 升级] 本任务涉及图片。Jev 智能体（%s）不接收图片信息，已按配置升级给 %s 判断：[%s](mention://agent/%s)\n\n",
		jevAgentName, target.Name, target.Name, uuidToString(target.ID))
	b.WriteString("**原任务内容**：\n\n")
	// Neutralize mention links in the quoted state so only the intended
	// escalation target is enqueued by this comment.
	stateText = jevQuotedMentionPattern.ReplaceAllString(stateText, "[提及链接]")
	if strings.TrimSpace(stateText) == "" {
		b.WriteString("（无文本输入）\n")
	} else {
		for _, line := range strings.Split(strings.TrimRight(stateText, "\n"), "\n") {
			fmt.Fprintf(&b, "> %s\n", line)
		}
	}
	if len(imageAtts) > 0 {
		b.WriteString("\n**图片附件**（用 `multica attachment download <id>` 获取后判断）：\n")
		for _, att := range imageAtts {
			fmt.Fprintf(&b, "- %s（id: %s）\n", att.Filename, uuidToString(att.ID))
		}
	}
	return b.String()
}

func jevEscalationNoTargetComment(why string) string {
	var b strings.Builder
	b.WriteString("[jev 升级失败] 本任务涉及图片，Jev 智能体不接收图片信息，本次判定已取消。\n\n")
	b.WriteString("原因：" + why + "。请在该 Jev 智能体的设置 → Jev 问题中配置「升级目标智能体」（一个非 Jev 运行时的 LLM 智能体），图片任务会自动 mention 它完成判断。\n")
	return b.String()
}
