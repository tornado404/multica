package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// jevQuestionsFromRuntimeConfig extracts the agent-authored jev question spec
// from the agent's runtime_config. Empty when absent — the backend then fails
// the run with a config error naming the missing spec, which is the visible
// signal an operator can act on.
func jevQuestionsFromRuntimeConfig(agent *AgentData) json.RawMessage {
	if agent == nil || len(agent.RuntimeConfig) == 0 {
		return nil
	}
	var rc struct {
		JevQuestions json.RawMessage `json:"jev_questions"`
	}
	if json.Unmarshal(agent.RuntimeConfig, &rc) != nil {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(rc.JevQuestions), []byte("null")) {
		return nil
	}
	return rc.JevQuestions
}

// buildJevStatePrompt renders the task's dispatch payload as the System One
// request state. Unlike CLI providers, this prompt must stay pure judgment
// input: jev answers the agent's configured questions over this state, so no
// workflow brief, CLI pointers, or per-turn context blocks are appended —
// BuildPrompt returns the body directly for jev.
func buildJevStatePrompt(task Task) string {
	switch {
	case task.ChatMessage != "":
		return task.ChatMessage
	case task.TriggerCommentContent != "":
		return task.TriggerCommentContent
	case task.AutopilotRunID != "":
		var b strings.Builder
		if desc := strings.TrimSpace(task.AutopilotDescription); desc != "" {
			b.WriteString(desc)
		} else if task.AutopilotTitle != "" {
			b.WriteString(task.AutopilotTitle)
		}
		if payload := strings.TrimSpace(string(task.AutopilotTriggerPayload)); payload != "" {
			b.WriteString("\n\n")
			b.WriteString(payload)
		}
		return b.String()
	case task.QuickCreatePrompt != "":
		return task.QuickCreatePrompt
	}
	if note := strings.TrimSpace(task.HandoffNote); note != "" {
		return note
	}
	if task.IssueIdentifier != "" {
		return fmt.Sprintf("Issue %s", task.IssueIdentifier)
	}
	return ""
}
