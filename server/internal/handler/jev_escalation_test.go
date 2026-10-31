package handler

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

// jevEscalationFixture wires a jev runtime + agent and a claude runtime +
// agent in the test workspace, both with default empty runtime_config. Tests
// that need an escalation target UPDATE the jev agent's runtime_config after
// both agents exist (the config references the target's id).
func jevEscalationFixture(t *testing.T, name string) (jevRuntime, jevAgent, llmRuntime, llmAgent string) {
	t.Helper()
	jevRuntime = dbfx.Runtime(t, name+"-jev-rt", testutil.Cols{"provider": "jev"})
	llmRuntime = dbfx.Runtime(t, name+"-llm-rt", testutil.Cols{"provider": "claude"})
	llmAgent = dbfx.Agent(t, name+"-llm", llmRuntime)
	jevAgent = dbfx.Agent(t, name+"-jev", jevRuntime)
	return jevRuntime, jevAgent, llmRuntime, llmAgent
}

// jevEscalationIssueWithImage creates an issue and, when withImage is set, an
// image attachment on it.
func jevEscalationIssueWithImage(t *testing.T, ctx context.Context, name, ownerID string, withImage bool) string {
	t.Helper()
	var issueID string
	dbfx.QueryRow(t, `
		INSERT INTO issue (workspace_id, title, status, priority, creator_id, creator_type, number, position)
		VALUES (
			$1, $2, 'in_progress', 'none', $3, 'member',
			(SELECT COALESCE(MAX(number), 82649) + 1 FROM issue WHERE workspace_id = $1),
			0
		)
		RETURNING id
	`, testWorkspaceID, name+" issue", ownerID).Scan(&issueID)
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM issue WHERE id = $1`, issueID) })

	if withImage {
		dbfx.Insert(t, "attachment", testutil.Cols{
			"workspace_id":  testWorkspaceID,
			"issue_id":      issueID,
			"uploader_type": "member",
			"uploader_id":   ownerID,
			"filename":      name + ".png",
			"url":           "https://example.invalid/" + name + ".png",
			"content_type":  "image/png",
			"size_bytes":    10,
		})
	}
	return issueID
}

func TestJevImageGuardEscalatesToConfiguredAgent(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	name := "jev-esc-happy"
	jevRuntime, jevAgent, _, llmAgent := jevEscalationFixture(t, name)
	dbfx.Exec(t, `UPDATE agent SET runtime_config = $1::jsonb WHERE id = $2`,
		fmt.Sprintf(`{"jev_escalation_agent_id": %q}`, llmAgent), jevAgent)
	issueID := jevEscalationIssueWithImage(t, ctx, name, testUserID, true)
	taskID := seedQueuedIssueTask(t, ctx, jevAgent, jevRuntime, issueID)

	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, parseUUID(jevRuntime))
	if err != nil || claimed == nil {
		t.Fatalf("claim task: %v", err)
	}
	runtime, err := testHandler.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
		ID: parseUUID(jevRuntime), WorkspaceID: parseUUID(testWorkspaceID),
	})
	if err != nil {
		t.Fatalf("load runtime: %v", err)
	}
	req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+jevRuntime+"/tasks/claim", nil)
	_, _, _, _, _, failure := testHandler.buildClaimedTaskResponse(
		req, claimed, runtime, jevRuntime, testWorkspaceID,
	)
	if failure == nil || failure.outcome != "error_jev_image_escalated" {
		t.Fatalf("failure = %+v, want error_jev_image_escalated", failure)
	}

	// The jev task is cancelled, not delivered.
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("jev task status = %q, want cancelled", status)
	}

	// The escalation comment names the target through the mention format.
	var authorType, content string
	dbfx.QueryRow(t, `
		SELECT author_type, content FROM comment
		WHERE source_task_id = $1 AND type = 'comment'
		ORDER BY created_at DESC LIMIT 1
	`, taskID).Scan(&authorType, &content)
	if authorType != "agent" {
		t.Fatalf("escalation comment author_type = %q, want agent", authorType)
	}
	if !strings.Contains(content, "mention://agent/"+llmAgent) {
		t.Fatalf("escalation comment must mention the target agent:\n%s", content)
	}

	// The target agent has a queued run on the same issue, triggered by the
	// escalation comment.
	var queuedCount int
	dbfx.QueryRow(t, `
		SELECT count(*) FROM agent_task_queue
		WHERE agent_id = $1 AND issue_id = $2 AND status = 'queued'
	`, llmAgent, issueID).Scan(&queuedCount)
	if queuedCount != 1 {
		t.Fatalf("target agent queued runs = %d, want 1", queuedCount)
	}
}

func TestJevImageGuardCancelsWithoutTarget(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	name := "jev-esc-notarget"
	jevRuntime, jevAgent, _, _ := jevEscalationFixture(t, name)
	// Point the config at the jev agent itself: unusable by the self guard.
	cfg := fmt.Sprintf(`{"jev_escalation_agent_id": %q}`, jevAgent)
	dbfx.Exec(t, `UPDATE agent SET runtime_config = $1::jsonb WHERE id = $2`, cfg, jevAgent)
	issueID := jevEscalationIssueWithImage(t, ctx, name, testUserID, true)
	taskID := seedQueuedIssueTask(t, ctx, jevAgent, jevRuntime, issueID)

	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, parseUUID(jevRuntime))
	if err != nil || claimed == nil {
		t.Fatalf("claim task: %v", err)
	}
	runtime, _ := testHandler.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
		ID: parseUUID(jevRuntime), WorkspaceID: parseUUID(testWorkspaceID),
	})
	req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+jevRuntime+"/tasks/claim", nil)
	_, _, _, _, _, failure := testHandler.buildClaimedTaskResponse(
		req, claimed, runtime, jevRuntime, testWorkspaceID,
	)
	if failure == nil || failure.outcome != "error_jev_image_no_escalation_target" {
		t.Fatalf("failure = %+v, want error_jev_image_no_escalation_target", failure)
	}

	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status)
	if status != "cancelled" {
		t.Fatalf("jev task status = %q, want cancelled", status)
	}
	// No run was enqueued for anyone — the jev agent itself must not run.
	var runCount int
	dbfx.QueryRow(t, `
		SELECT count(*) FROM agent_task_queue
		WHERE issue_id = $1 AND status = 'queued'
	`, issueID).Scan(&runCount)
	if runCount != 0 {
		t.Fatalf("queued runs after failed escalation = %d, want 0", runCount)
	}
}

func TestJevImageGuardPassesTextOnlyTasksThrough(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	name := "jev-esc-textonly"
	jevRuntime, jevAgent, _, _ := jevEscalationFixture(t, name)
	issueID := jevEscalationIssueWithImage(t, ctx, name, testUserID, false)
	taskID := seedQueuedIssueTask(t, ctx, jevAgent, jevRuntime, issueID)

	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, parseUUID(jevRuntime))
	if err != nil || claimed == nil {
		t.Fatalf("claim task: %v", err)
	}
	runtime, _ := testHandler.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
		ID: parseUUID(jevRuntime), WorkspaceID: parseUUID(testWorkspaceID),
	})
	req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+jevRuntime+"/tasks/claim", nil)
	_, _, _, _, _, failure := testHandler.buildClaimedTaskResponse(
		req, claimed, runtime, jevRuntime, testWorkspaceID,
	)
	if failure != nil {
		t.Fatalf("failure = %+v, want nil for a text-only task", failure)
	}
	var status string
	dbfx.QueryRow(t, `SELECT status FROM agent_task_queue WHERE id = $1`, taskID).Scan(&status)
	if status != "dispatched" {
		t.Fatalf("task status = %q, want dispatched", status)
	}
}

func TestJevImageGuardEscalatesOnImageMarkdown(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	name := "jev-esc-md"
	jevRuntime, jevAgent, _, llmAgent := jevEscalationFixture(t, name)
	dbfx.Exec(t, `UPDATE agent SET runtime_config = $1::jsonb WHERE id = $2`,
		fmt.Sprintf(`{"jev_escalation_agent_id": %q}`, llmAgent), jevAgent)
	issueID := jevEscalationIssueWithImage(t, ctx, name, testUserID, false)
	commentID := dbfx.Comment(t, issueID, "判断这张截图 ![screenshot](https://example.invalid/a.png) 里的状态", testutil.Cols{})
	var taskID string
	dbfx.QueryRow(t, `
		INSERT INTO agent_task_queue (agent_id, runtime_id, issue_id, trigger_comment_id, status, priority)
		VALUES ($1, $2, $3, $4, 'queued', 0)
		RETURNING id
	`, jevAgent, jevRuntime, issueID, commentID).Scan(&taskID)
	t.Cleanup(func() { testPool.Exec(ctx, `DELETE FROM agent_task_queue WHERE id = $1`, taskID) })

	claimed, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, parseUUID(jevRuntime))
	if err != nil || claimed == nil {
		t.Fatalf("claim task: %v", err)
	}
	runtime, _ := testHandler.Queries.GetAgentRuntimeForWorkspace(ctx, db.GetAgentRuntimeForWorkspaceParams{
		ID: parseUUID(jevRuntime), WorkspaceID: parseUUID(testWorkspaceID),
	})
	req := newOwnerDaemonRequest(http.MethodPost, "/api/daemon/runtimes/"+jevRuntime+"/tasks/claim", nil)
	_, _, _, _, _, failure := testHandler.buildClaimedTaskResponse(
		req, claimed, runtime, jevRuntime, testWorkspaceID,
	)
	if failure == nil || failure.outcome != "error_jev_image_escalated" {
		t.Fatalf("failure = %+v, want error_jev_image_escalated for image markdown", failure)
	}
	var queuedCount int
	dbfx.QueryRow(t, `
		SELECT count(*) FROM agent_task_queue
		WHERE agent_id = $1 AND issue_id = $2 AND status = 'queued'
	`, llmAgent, issueID).Scan(&queuedCount)
	if queuedCount != 1 {
		t.Fatalf("target agent queued runs = %d, want 1", queuedCount)
	}
}

func TestCreateAgent_JevEscalationTarget(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	jevID := dbfx.Runtime(t, "jev-esc-validate", testutil.Cols{"provider": "jev"})
	claudeID := dbfx.Runtime(t, "jev-esc-validate-llm", testutil.Cols{"provider": "claude"})
	llmAgent := dbfx.Agent(t, "jev-esc-validate-llm-agent", claudeID)
	jevAgent := dbfx.Agent(t, "jev-esc-validate-jev-agent", jevID)

	for _, tc := range []struct {
		name    string
		target  string
		want    int
		wantErr string
	}{
		{"no target is allowed", "", http.StatusCreated, ""},
		{"valid llm target", llmAgent, http.StatusCreated, ""},
		{"jev target is rejected", jevAgent, http.StatusBadRequest, "non-jev runtime"},
		{"unknown target is rejected", "01a00000-0000-0000-0000-000000000000", http.StatusBadRequest, "does not exist"},
		{"malformed target is rejected", "not-a-uuid", http.StatusBadRequest, "not a valid agent id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "jev-esc-target-" + tc.name
			dbfx.Cleanup(t, `DELETE FROM agent WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, name)
			runtimeCfg := map[string]any{}
			if tc.target != "" {
				runtimeCfg["jev_escalation_agent_id"] = tc.target
			}
			w := testutil.Call(t, testHandler.CreateAgent, newRequest(http.MethodPost, "/api/agents", map[string]any{
				"name": name, "runtime_id": jevID, "runtime_config": runtimeCfg,
			})).Want(tc.want)
			if tc.want == http.StatusBadRequest && !strings.Contains(w.Body.String(), tc.wantErr) {
				t.Fatalf("error must name the rule: %s", w.Body.String())
			}
		})
	}
}
