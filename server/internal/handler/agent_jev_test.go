package handler

import (
	"net/http"
	"strings"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestCreateAgent_JevQuestions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	jevID := dbfx.Runtime(t, "jev-questions", testutil.Cols{"provider": "jev"})
	for _, tc := range []struct {
		name       string
		runtimeCfg map[string]any
		want       int
		wantErr    string
	}{
		{
			name:       "no spec yet is allowed",
			runtimeCfg: map[string]any{},
			want:       http.StatusCreated,
		},
		{
			name:       "null spec is allowed",
			runtimeCfg: map[string]any{"jev_questions": nil},
			want:       http.StatusCreated,
		},
		{
			name: "valid spec",
			runtimeCfg: map[string]any{"jev_questions": map[string]any{
				"next_action": map[string]any{
					"type":     "choice",
					"criteria": map[string]any{"observe": "wait", "advance": "move on"},
				},
				"urgent": map[string]any{"type": "noul"},
			}},
			want: http.StatusCreated,
		},
		{
			name:       "explicitly empty spec",
			runtimeCfg: map[string]any{"jev_questions": map[string]any{}},
			want:       http.StatusBadRequest,
			wantErr:    "at least one question",
		},
		{
			name:       "unknown question type",
			runtimeCfg: map[string]any{"jev_questions": map[string]any{"q": map[string]any{"type": "box"}}},
			want:       http.StatusBadRequest,
			wantErr:    "choice, score, noul",
		},
		{
			name:       "choice without criteria",
			runtimeCfg: map[string]any{"jev_questions": map[string]any{"q": map[string]any{"type": "choice"}}},
			want:       http.StatusBadRequest,
			wantErr:    "at least one option",
		},
		{
			name:       "spec outside runtime_config key is not the spec",
			runtimeCfg: map[string]any{"model_hint": map[string]any{"type": "box"}},
			want:       http.StatusCreated,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "jev-questions-create-" + tc.name
			dbfx.Cleanup(t, `DELETE FROM agent WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, name)
			w := testutil.Call(t, testHandler.CreateAgent, newRequest(http.MethodPost, "/api/agents", map[string]any{
				"name": name, "runtime_id": jevID, "runtime_config": tc.runtimeCfg,
			})).Want(tc.want)
			if tc.want == http.StatusBadRequest {
				if !strings.Contains(w.Body.String(), tc.wantErr) {
					t.Fatalf("error must name the rule: %s", w.Body.String())
				}
			}
		})
	}
}

func TestUpdateAgent_JevQuestions(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	jevID := dbfx.Runtime(t, "jev-questions", testutil.Cols{"provider": "jev"})

	// Saving a valid spec succeeds.
	id := dbfx.Agent(t, "jev-questions-update", jevID, testutil.Cols{
		"runtime_config": []byte(`{}`),
	})
	spec := map[string]any{"jev_questions": map[string]any{
		"tone": map[string]any{"type": "noul"},
	}}
	testutil.Call(t, testHandler.UpdateAgent, withURLParam(newRequest(http.MethodPut, "/api/agents/"+id, map[string]any{
		"runtime_config": spec,
	}), "id", id)).Want(http.StatusOK)

	// Saving a malformed spec is rejected with the validator's message.
	w := testutil.Call(t, testHandler.UpdateAgent, withURLParam(newRequest(http.MethodPut, "/api/agents/"+id, map[string]any{
		"runtime_config": map[string]any{"jev_questions": map[string]any{"tone": map[string]any{"type": "box"}}},
	}), "id", id)).Want(http.StatusBadRequest)
	if !strings.Contains(w.Body.String(), "choice, score, noul") {
		t.Fatalf("error must name the supported types: %s", w.Body.String())
	}
}

func TestCreateAgent_JevThinkingRejected(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	jevID := dbfx.Runtime(t, "jev-questions", testutil.Cols{"provider": "jev"})
	name := "jev-questions-thinking"
	dbfx.Cleanup(t, `DELETE FROM agent WHERE workspace_id = $1 AND name = $2`, testWorkspaceID, name)
	w := testutil.Call(t, testHandler.CreateAgent, newRequest(http.MethodPost, "/api/agents", map[string]any{
		"name": name, "runtime_id": jevID, "thinking_level": "high",
	})).Want(http.StatusBadRequest)
	if !strings.Contains(strings.ToLower(w.Body.String()), "thinking") {
		t.Fatalf("error must name thinking_level: %s", w.Body.String())
	}
}
