package daemon

import (
	"encoding/json"
	"testing"
)

func TestBuildJevStatePrompt(t *testing.T) {
	cases := []struct {
		name string
		task Task
		want string
	}{
		{
			name: "chat message",
			task: Task{ChatMessage: "What tone is this message?"}, want: "What tone is this message?",
		},
		{
			name: "trigger comment",
			task: Task{TriggerCommentContent: "Please judge the ticket."}, want: "Please judge the ticket.",
		},
		{
			name: "autopilot instructions with payload",
			task: Task{
				AutopilotRunID:          "run-1",
				AutopilotDescription:    "Judge the queue.",
				AutopilotTriggerPayload: []byte(`{"issue":"ZOYEN-1"}`),
			},
			want: "Judge the queue.\n\n{\"issue\":\"ZOYEN-1\"}",
		},
		{
			name: "autopilot falls back to title",
			task: Task{AutopilotRunID: "run-1", AutopilotTitle: "Patrol", AutopilotDescription: "  "},
			want: "Patrol",
		},
		{
			name: "quick create",
			task: Task{QuickCreatePrompt: "Summarize this input."}, want: "Summarize this input.",
		},
		{
			name: "handoff note",
			task: Task{HandoffNote: "Judge whether this issue is done."}, want: "Judge whether this issue is done.",
		},
		{
			name: "issue identifier fallback",
			task: Task{IssueIdentifier: "ZOYEN-42"}, want: "Issue ZOYEN-42",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildJevStatePrompt(tc.task); got != tc.want {
				t.Errorf("buildJevStatePrompt = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBuildPromptOmitsPerTurnBlocksForJev(t *testing.T) {
	// A comment-triggered CLI task carries per-turn context blocks; the same
	// task on a jev agent must produce the state text alone.
	task := Task{TriggerCommentContent: "Judge this.", TriggerCommentID: "c1"}
	if cli := BuildPrompt(task, "claude"); cli == "Judge this." {
		t.Fatalf("claude prompt unexpectedly equals the bare comment; the test lost its contrast")
	}
	if got := BuildPrompt(task, "jev"); got != "Judge this." {
		t.Errorf("jev prompt = %q, want the comment content alone", got)
	}
}

func TestJevQuestionsFromRuntimeConfig(t *testing.T) {
	cases := []struct {
		name  string
		agent *AgentData
		want  string
	}{
		{name: "nil agent", agent: nil, want: ""},
		{name: "empty runtime config", agent: &AgentData{RuntimeConfig: nil}, want: ""},
		{
			name:  "spec extracted",
			agent: &AgentData{RuntimeConfig: json.RawMessage(`{"jev_questions":{"tone":{"type":"choice"}}}`)},
			want:  `{"tone":{"type":"choice"}}`,
		},
		{
			name:  "null spec treated as absent",
			agent: &AgentData{RuntimeConfig: json.RawMessage(`{"jev_questions":null}`)},
			want:  "",
		},
		{
			name:  "absent key",
			agent: &AgentData{RuntimeConfig: json.RawMessage(`{"model":"jev-latest"}`)},
			want:  "",
		},
		{
			name:  "malformed config",
			agent: &AgentData{RuntimeConfig: json.RawMessage(`not-json`)},
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jevQuestionsFromRuntimeConfig(tc.agent)
			if string(got) != tc.want {
				t.Errorf("jevQuestionsFromRuntimeConfig = %q, want %q", got, tc.want)
			}
		})
	}
}
