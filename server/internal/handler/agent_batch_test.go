package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

// The Allocator GUI's pour action is one request that changes the (runtime,
// model) binding of many agents at once. These tests pin the two properties
// that make it safe to point a batch UI at:
//
//   - Partial success is real. One unmanageable row must not discard the rows
//     the caller may change — during a provider rate-limit the whole point of
//     the batch is to move what can be moved, now.
//   - Per-row semantics are UpdateAgent's, not a second weaker copy. The
//     cross-provider model clear and the thinking_level / service_tier
//     validation run through planAgentRuntimeSwap; a batch that skipped them
//     would leave an agent holding a model id its new runtime cannot serve.

type batchResponse struct {
	Updated []struct {
		ID        string `json:"id"`
		RuntimeID string `json:"runtime_id"`
		Model     string `json:"model"`
	} `json:"updated"`
	Skipped []struct {
		AgentID string `json:"agent_id"`
		Reason  string `json:"reason"`
		Error   string `json:"error"`
	} `json:"skipped"`
}

func (b batchResponse) updatedIDs() []string {
	ids := make([]string, 0, len(b.Updated))
	for _, a := range b.Updated {
		ids = append(ids, a.ID)
	}
	return ids
}

func (b batchResponse) skippedReason(agentID string) string {
	for _, s := range b.Skipped {
		if s.AgentID == agentID {
			return s.Reason
		}
	}
	return ""
}

func callBatchUpdate(t *testing.T, body any) (testutil.Response, batchResponse) {
	t.Helper()
	var out batchResponse
	rec := testutil.Call(t, testHandler.BatchUpdateAgents,
		newRequest(http.MethodPost, "/api/agents/batch-update", body))
	if rec.Code == http.StatusOK {
		rec.JSON(&out)
	}
	return *rec, out
}

func agentColumns(t *testing.T, agentID string) (runtimeID, model string) {
	t.Helper()
	var rt pgtype.UUID
	dbfx.QueryRow(t, `SELECT runtime_id, model FROM agent WHERE id = $1`, agentID).Scan(&rt, &model)
	return uuidToString(rt), model
}

// TestBatchUpdateAgents_MovesEveryRowForWorkspaceOwner covers the admin sweep:
// re-pointing a batch of agents — including agents other members own — at a
// new runtime in one request.
func TestBatchUpdateAgents_MovesEveryRowForWorkspaceOwner(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	claude := createClaudeProviderRuntime(t)
	codex := createCodexProviderRuntime(t)
	agents := []string{
		createAgentOnRuntimeWithModel(t, "batch-owner-a", claude, "claude-sonnet-4-6"),
		createAgentOnRuntimeWithModel(t, "batch-owner-b", claude, "claude-sonnet-4-6"),
		createAgentOnRuntimeWithModel(t, "batch-owner-c", claude, "claude-sonnet-4-6"),
	}

	rec, out := callBatchUpdate(t, map[string]any{
		"updates": []map[string]any{
			{"agent_id": agents[0], "runtime_id": codex},
			{"agent_id": agents[1], "runtime_id": codex},
			{"agent_id": agents[2], "runtime_id": codex},
		},
	})
	rec.Want(http.StatusOK)

	if len(out.Updated) != 3 || len(out.Skipped) != 0 {
		t.Fatalf("expected 3 updated / 0 skipped, got %d / %d", len(out.Updated), len(out.Skipped))
	}
	for i, agentID := range agents {
		gotRuntime, gotModel := agentColumns(t, agentID)
		if gotRuntime != codex {
			t.Errorf("agent %d (%s): runtime_id = %q, want %q", i, agentID, gotRuntime, codex)
		}
		// Claude -> Codex is a known family switch and the request named no
		// replacement model, so the foreign id must be gone rather than
		// reaching the Codex CLI.
		if gotModel != "" {
			t.Errorf("agent %d (%s): model = %q, want cleared", i, agentID, gotModel)
		}
	}
}

// TestBatchUpdateAgents_PartialSuccessIsTheContract is the behaviour the pour
// UX depends on: a member may move their own agents and not their peers', and
// the unmanageable row must land in `skipped` while the manageable one commits.
func TestBatchUpdateAgents_PartialSuccessIsTheContract(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	claude := createClaudeProviderRuntime(t)
	codex := createCodexProviderRuntime(t)

	// A plain member: owns one agent, may not touch the fixture owner's.
	// Emails carry a per-run suffix: the shared test database outlives any
	// run that died before cleanup, and user_email is unique.
	otherID := dbfx.User(t, "Batch Plain Member",
		fmt.Sprintf("batch-plain-member-%d@multica.test", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, otherID, "member")
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, otherID)
	})

	theirs := createAgentOnRuntimeWithModel(t, "batch-member-own", claude, "claude-sonnet-4-6")
	dbfx.Exec(t, `UPDATE agent SET owner_id = $1 WHERE id = $2`, otherID, theirs)
	notTheirs := createAgentOnRuntimeWithModel(t, "batch-owner-other", claude, "claude-sonnet-4-6")
	// The pour target must be a runtime the member may use. The provider
	// runtimes are owner-private fixtures, so open this one to the workspace.
	dbfx.Exec(t, `UPDATE agent_runtime SET visibility = 'public' WHERE id = $1`, codex)

	rec := testutil.Call(t, testHandler.BatchUpdateAgents, newRequestAs(
		otherID, http.MethodPost, "/api/agents/batch-update",
		map[string]any{
			"updates": []map[string]any{
				{"agent_id": theirs, "runtime_id": codex},
				{"agent_id": notTheirs, "runtime_id": codex},
			},
		},
	))
	var out batchResponse
	rec.Want(http.StatusOK).JSON(&out)

	if len(out.Updated) != 1 || out.Updated[0].ID != theirs {
		t.Fatalf("expected only the member's own agent updated, got %+v / skipped %+v", out.Updated, out.Skipped)
	}
	if got := out.skippedReason(notTheirs); got != patchReasonForbidden {
		t.Errorf("skipped reason for another member's agent = %q, want %q", got, patchReasonForbidden)
	}

	// The decisive half: the successful row committed even though its sibling
	// was refused, i.e. the batch was not rolled back wholesale.
	if gotRuntime, _ := agentColumns(t, theirs); gotRuntime != codex {
		t.Errorf("permitted agent runtime_id = %q, want %q (partial success must commit)", gotRuntime, codex)
	}
	if gotRuntime, _ := agentColumns(t, notTheirs); gotRuntime != claude {
		t.Errorf("refused agent moved anyway: runtime_id = %q, want %q", gotRuntime, claude)
	}
}

// TestBatchUpdateAgents_RejectsAnotherMembersPrivateRuntime pins the lateral
// movement guard. A workspace owner is NOT exempt: a private runtime is usable
// only by its own owner, so a batch cannot be used to park agents on someone
// else's machine. Same truth table as CreateAgent / UpdateAgent.
func TestBatchUpdateAgents_RejectsAnotherMembersPrivateRuntime(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	claude := createClaudeProviderRuntime(t)
	agentID := createAgentOnRuntimeWithModel(t, "batch-private-target", claude, "claude-sonnet-4-6")

	otherID := dbfx.User(t, "Batch Runtime Owner",
		fmt.Sprintf("batch-runtime-owner-%d@multica.test", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, otherID, "member")
	privateRuntime := dbfx.Runtime(t, "Batch Private Runtime", testutil.Cols{
		"provider": "codex",
		"owner_id": otherID,
	})
	t.Cleanup(func() {
		testPool.Exec(ctx, `DELETE FROM "user" WHERE id = $1`, otherID)
	})

	// testUserID is the workspace owner, and still may not bind onto it.
	rec, out := callBatchUpdate(t, map[string]any{
		"updates": []map[string]any{
			{"agent_id": agentID, "runtime_id": privateRuntime},
		},
	})
	rec.Want(http.StatusOK)

	if len(out.Updated) != 0 {
		t.Fatalf("agent bound onto another member's private runtime: %+v", out.Updated)
	}
	if got := out.skippedReason(agentID); got != patchReasonRuntimePrivate {
		t.Errorf("skipped reason = %q, want %q", got, patchReasonRuntimePrivate)
	}
	if gotRuntime, _ := agentColumns(t, agentID); gotRuntime != claude {
		t.Errorf("agent moved despite refusal: runtime_id = %q, want %q", gotRuntime, claude)
	}
}

// TestBatchUpdateAgents_RefusesRuntimeUnbind protects the one field the SQL
// cannot express. UpdateAgent writes `runtime_id = COALESCE($7, runtime_id)`,
// so a NULL would be silently dropped and the caller would believe the agent
// was unbound — a lie is worse than an error, especially when the UI then
// paints the agent as unassigned.
func TestBatchUpdateAgents_RefusesRuntimeUnbind(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	claude := createClaudeProviderRuntime(t)
	agentID := createAgentOnRuntimeWithModel(t, "batch-no-unbind", claude, "claude-sonnet-4-6")

	t.Run("empty string is refused, not ignored", func(t *testing.T) {
		rec, out := callBatchUpdate(t, map[string]any{
			"updates": []map[string]any{
				{"agent_id": agentID, "runtime_id": ""},
			},
		})
		rec.Want(http.StatusOK)
		if got := out.skippedReason(agentID); got != patchReasonRuntimeRequired {
			t.Errorf("skipped reason = %q, want %q", got, patchReasonRuntimeRequired)
		}
		if len(out.Updated) != 0 {
			t.Errorf("expected nothing updated, got %+v", out.Updated)
		}
		if gotRuntime, _ := agentColumns(t, agentID); gotRuntime != claude {
			t.Errorf("runtime_id changed to %q; an unbind request must change nothing", gotRuntime)
		}
	})

	t.Run("omitted runtime_id leaves the binding alone", func(t *testing.T) {
		rec, out := callBatchUpdate(t, map[string]any{
			"updates": []map[string]any{
				{"agent_id": agentID, "model": "claude-opus-4-7"},
			},
		})
		rec.Want(http.StatusOK)
		if len(out.Updated) != 1 {
			t.Fatalf("expected a model-only update to apply, got %+v", out)
		}
		gotRuntime, gotModel := agentColumns(t, agentID)
		if gotRuntime != claude {
			t.Errorf("runtime_id = %q, want it preserved as %q", gotRuntime, claude)
		}
		if gotModel != "claude-opus-4-7" {
			t.Errorf("model = %q, want claude-opus-4-7", gotModel)
		}
	})
}

// TestBatchUpdateAgents_ValidatesPerRow covers the refusals that come from the
// same planner the single-agent dialog uses, plus the row states the batch must
// not act on at all.
func TestBatchUpdateAgents_ValidatesPerRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	ctx := context.Background()
	claude := createClaudeProviderRuntime(t)
	codex := createCodexProviderRuntime(t)
	agentID := createAgentOnRuntimeWithModel(t, "batch-validation", claude, "claude-sonnet-4-6")

	foreignWorkspace := dbfx.Workspace(t, "Batch Foreign", "batch-foreign-ws")
	foreignUser := dbfx.User(t, "Batch Foreign User", "batch-foreign-user@multica.test")
	dbfx.Member(t, foreignWorkspace, foreignUser, "owner")
	foreignRuntime := dbfx.Runtime(t, "Batch Foreign Runtime", testutil.Cols{
		"workspace_id": foreignWorkspace,
		"owner_id":     foreignUser,
	})
	foreignAgent := dbfx.Agent(t, "batch-foreign-agent", foreignRuntime, testutil.Cols{
		"workspace_id": foreignWorkspace,
		"owner_id":     foreignUser,
	})

	archived := createAgentOnRuntimeWithModel(t, "batch-archived", claude, "claude-sonnet-4-6")
	dbfx.Exec(t, `UPDATE agent SET archived_at = now() WHERE id = $1`, archived)
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE agent SET archived_at = NULL WHERE id = $1`, archived)
	})

	systemKind := createAgentOnRuntimeWithModel(t, "batch-system-kind", claude, "claude-sonnet-4-6")
	dbfx.Exec(t, `UPDATE agent SET kind = 'system' WHERE id = $1`, systemKind)
	t.Cleanup(func() {
		testPool.Exec(ctx, `UPDATE agent SET kind = 'user' WHERE id = $1`, systemKind)
	})

	rec, out := callBatchUpdate(t, map[string]any{
		"updates": []map[string]any{
			// A codex-only token is literal-invalid for Claude.
			{"agent_id": agentID, "runtime_id": claude, "thinking_level": "none"},
			// Another workspace's agent: reported as absent, never as "found but
			// private to someone else", so existence outside your scope leaks.
			{"agent_id": foreignAgent, "runtime_id": codex},
			{"agent_id": archived, "runtime_id": codex},
			{"agent_id": systemKind, "runtime_id": codex},
			{"agent_id": "not-a-uuid", "runtime_id": codex},
		},
	})
	rec.Want(http.StatusOK)

	if len(out.Updated) != 0 {
		t.Fatalf("expected zero applied rows, got %+v", out.Updated)
	}
	cases := map[string]string{
		agentID:      patchReasonThinkingInvalid,
		foreignAgent: patchReasonNotFound,
		archived:     patchReasonArchived,
		systemKind:   patchReasonNotFound,
		"not-a-uuid": patchReasonNotFound,
	}
	for id, want := range cases {
		if got := out.skippedReason(id); got != want {
			t.Errorf("skipped reason for %s = %q, want %q", id, got, want)
		}
	}
	if gotRuntime, _ := agentColumns(t, agentID); gotRuntime != claude {
		t.Errorf("invalid thinking_level still moved the agent: runtime_id = %q", gotRuntime)
	}
}

// TestBatchUpdateAgents_RejectsMalformedEnvelopes keeps a client bug a loud
// failure. Silently dropping an unsupported field would let a UI believe it
// rewrote MCP config or rotated a secret.
func TestBatchUpdateAgents_RejectsMalformedEnvelopes(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	claude := createClaudeProviderRuntime(t)
	agentID := createAgentOnRuntimeWithModel(t, "batch-envelope", claude, "claude-sonnet-4-6")

	cases := []struct {
		name string
		body any
		want string
	}{
		{
			name: "empty updates",
			body: map[string]any{"updates": []map[string]any{}},
			want: "must not be empty",
		},
		{
			name: "missing agent id",
			body: map[string]any{"updates": []map[string]any{{"model": "x"}}},
			want: "agent_id is required",
		},
		{
			// The batch surface is (runtime, model) only. Env has its own
			// audited endpoint (MUL-2600) and must never ride along.
			name: "custom env rejected",
			body: map[string]any{"updates": []map[string]any{
				{"agent_id": agentID, "runtime_id": claude, "custom_env": map[string]string{"K": "V"}},
			}},
			want: "custom_env",
		},
		{
			name: "unsupported field rejected",
			body: map[string]any{"updates": []map[string]any{
				{"agent_id": agentID, "runtime_id": claude, "mcp_config": map[string]any{"server": "x"}},
			}},
			want: "mcp_config",
		},
		{
			name: "duplicate agent id",
			body: map[string]any{"updates": []map[string]any{
				{"agent_id": agentID, "model": "a"},
				{"agent_id": agentID, "model": "b"},
			}},
			want: "duplicate agent_id",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := testutil.Call(t, testHandler.BatchUpdateAgents,
				newRequest(http.MethodPost, "/api/agents/batch-update", tc.body))
			rec.Want(http.StatusBadRequest)
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body %q does not mention %q", rec.Body.String(), tc.want)
			}
		})
	}

	// None of the rejected requests may have changed the agent.
	if _, gotModel := agentColumns(t, agentID); gotModel != "claude-sonnet-4-6" {
		t.Errorf("model = %q after rejected requests, want it unchanged", gotModel)
	}
}

// TestBatchUpdateAgents_OverLimitRejected bounds the worst case: the row locks
// are taken sequentially inside one transaction.
func TestBatchUpdateAgents_OverLimitRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	updates := make([]map[string]any, maxBatchUpdateAgents+1)
	for i := range updates {
		updates[i] = map[string]any{"agent_id": testWorkspaceID, "model": "x"}
	}
	rec := testutil.Call(t, testHandler.BatchUpdateAgents,
		newRequest(http.MethodPost, "/api/agents/batch-update", map[string]any{"updates": updates}))
	rec.Want(http.StatusBadRequest)
	if !strings.Contains(rec.Body.String(), "exceeds the limit") {
		t.Errorf("body %q does not explain the limit", rec.Body.String())
	}
}

// TestBatchUpdateAgents_EmitsOneStatusEventPerAppliedRow keeps the realtime
// contract: clients invalidate their agent caches from a per-agent status
// event, so a pour must look like N ordinary edits and needs no new
// client-side sync path.
func TestBatchUpdateAgents_EmitsOneStatusEventPerAppliedRow(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}

	claude := createClaudeProviderRuntime(t)
	codex := createCodexProviderRuntime(t)
	moving := createAgentOnRuntimeWithModel(t, "batch-event-a", claude, "claude-sonnet-4-6")
	refused := createAgentOnRuntimeWithModel(t, "batch-event-b", claude, "claude-sonnet-4-6")
	otherID := dbfx.User(t, "Batch Event Other",
		fmt.Sprintf("batch-event-other-%d@multica.test", time.Now().UnixNano()))
	dbfx.Member(t, testWorkspaceID, otherID, "member")
	// A row the caller — the workspace owner — still cannot apply: nobody is
	// exempt from the private-runtime gate (see the dedicated test below), so
	// pointing this entry at another member's private runtime is the one
	// refusal that survives an owner caller.
	privateRuntime := dbfx.Runtime(t, "Batch Event Private Runtime", testutil.Cols{
		"provider": "codex",
		"owner_id": otherID,
	})

	statusEvents := make(chan events.Event, 8)
	testHandler.Bus.Subscribe(protocol.EventAgentStatus, func(e events.Event) {
		select {
		case statusEvents <- e:
		default:
		}
	})

	rec, out := callBatchUpdate(t, map[string]any{
		"updates": []map[string]any{
			{"agent_id": moving, "runtime_id": codex},
			{"agent_id": refused, "runtime_id": privateRuntime},
		},
	})
	rec.Want(http.StatusOK)
	if len(out.Updated) != 1 || len(out.Skipped) != 1 {
		t.Fatalf("expected 1 updated / 1 skipped, got %+v", out)
	}

	timeout := time.After(2 * time.Second)
	var seen []string
	for range out.Updated {
		select {
		case e := <-statusEvents:
			payload, _ := json.Marshal(e.Payload)
			if !strings.Contains(string(payload), moving) {
				t.Errorf("event payload does not name the moved agent: %s", payload)
			}
			seen = append(seen, moving)
		case <-timeout:
			t.Fatalf("timed out waiting for the status event for %s", moving)
		}
	}

	// A refused row must be silent: broadcasting it would tell every client an
	// update landed that never happened.
	select {
	case e := <-statusEvents:
		blob, _ := json.Marshal(e.Payload)
		if strings.Contains(string(blob), refused) {
			t.Errorf("refused agent %s was broadcast: %s", refused, blob)
		}
	case <-time.After(200 * time.Millisecond):
	}
	if len(seen) != len(out.Updated) {
		t.Errorf("saw %d events for %d updated rows", len(seen), len(out.Updated))
	}
}
