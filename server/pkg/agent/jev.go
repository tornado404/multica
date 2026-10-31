package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Jev runs prompts through the TypeSafe System One HTTP API. Unlike every
// other backend it spawns no CLI: the prompt is the request's `state`, the
// agent's configured question spec becomes `questions`, and the response is a
// typed judgment (choice / noul / score) with probabilities — not generated
// prose. See https://docs.typesafe.ai/api.md for the wire contract.
//
// The question spec (`jev_questions` in agent runtime_config) is authored once
// per agent; automations dispatch ordinary text and the same agent answers the
// same questions over whatever state they send.
const (
	// JevAPIKeyEnv carries the TypeSafe API key, read from the agent's env.
	JevAPIKeyEnv = "TYPESAFE_API_KEY"
	// JevBaseURLEnv overrides the API origin (tests, proxies).
	JevBaseURLEnv = "TYPESAFE_BASE_URL"
	// JevDefaultModel is the request `model` when agent.model is empty.
	JevDefaultModel = "jev-latest"
	// JevDefaultBaseURL is the API origin when JevBaseURLEnv is unset.
	JevDefaultBaseURL = "https://api.typesafe.ai"

	jevSystemOnePath = "/v1/systemone"
	// System One judgments are fast, but long context and provider load exist;
	// this bounds the call independently of the daemon's task timeout.
	jevHTTPTimeout = 2 * time.Minute
)

// jevBackend implements Backend against the TypeSafe System One API.
type jevBackend struct {
	cfg Config
}

// jevRequest mirrors the SystemOneRequest schema: model, state, questions.
type jevRequest struct {
	Model     string         `json:"model"`
	State     string         `json:"state"`
	Questions map[string]any `json:"questions"`
}

// jevResponse mirrors SystemOneResponse. Answers are kept raw here so the
// result can echo the exact judgment, and decoded per answer for formatting.
type jevResponse struct {
	Model   string                     `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   *jevUsage                  `json:"usage,omitempty"`
}

type jevUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// jevAnswer decodes one typed answer. Choice/score/noul are the three types
// this integration supports; box (image bounding boxes) is rejected at spec
// validation because it needs the image danger-zone header and assets.
type jevAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Noul          *float64           `json:"noul,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

func (b *jevBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	questions, err := parseJevQuestions(opts.JevQuestions)
	if err != nil {
		return nil, err
	}
	apiKey := strings.TrimSpace(b.cfg.Env[JevAPIKeyEnv])
	if apiKey == "" {
		return nil, fmt.Errorf("jev runtime requires %s in the agent's environment", JevAPIKeyEnv)
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("jev run has no state text — dispatch the task with an instruction describing the situation to judge")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(b.cfg.Env[JevBaseURLEnv]), "/")
	if baseURL == "" {
		baseURL = JevDefaultBaseURL
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		model = JevDefaultModel
	}

	messages := make(chan Message, 8)
	result := make(chan Result, 1)
	session := &Session{Messages: messages, Result: result}
	go func() {
		defer close(messages)
		b.run(ctx, model, apiKey, baseURL, prompt, questions, messages, result)
	}()
	return session, nil
}

func (b *jevBackend) run(ctx context.Context, model, apiKey, baseURL, prompt string, questions map[string]any, messages chan<- Message, result chan<- Result) {
	defer close(result)
	trySend(messages, Message{Type: MessageStatus, Status: "running"})

	body, err := json.Marshal(jevRequest{Model: model, State: prompt, Questions: questions})
	if err != nil {
		result <- Result{Status: "failed", Error: fmt.Sprintf("jev: encode request: %v", err)}
		return
	}

	reqCtx, cancel := context.WithTimeout(ctx, jevHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+jevSystemOnePath, bytes.NewReader(body))
	if err != nil {
		result <- Result{Status: "failed", Error: fmt.Sprintf("jev: build request: %v", err)}
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	logger := b.cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	started := time.Now()
	resp, err := (&http.Client{Timeout: jevHTTPTimeout}).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			result <- Result{Status: "cancelled", Error: "cancelled"}
			return
		}
		result <- Result{Status: "failed", Error: fmt.Sprintf("jev: call %s: %v", baseURL+jevSystemOnePath, err)}
		return
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, jevMaxResponseBytes))
	if err != nil {
		result <- Result{Status: "failed", Error: fmt.Sprintf("jev: read response: %v", err)}
		return
	}
	logger.Info("jev api call finished", "status", resp.StatusCode, "elapsed", time.Since(started), "model", model)

	if resp.StatusCode != http.StatusOK {
		result <- Result{Status: "failed", Error: jevAPIError(resp.StatusCode, raw)}
		return
	}

	var parsed jevResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		result <- Result{Status: "failed", Error: fmt.Sprintf("jev: malformed response: %v", err)}
		return
	}
	formatted := formatJevResponse(&parsed, raw)
	trySend(messages, Message{Type: MessageText, Content: formatted})

	usage := map[string]TokenUsage{}
	if parsed.Usage != nil {
		usage[parsed.Model] = TokenUsage{InputTokens: parsed.Usage.InputTokens, OutputTokens: parsed.Usage.OutputTokens}
	}
	result <- Result{Status: "completed", Output: formatted, Usage: usage}
}

// jevMaxResponseBytes bounds a judgment response; answers are small, so this
// only stops a hostile or broken endpoint from exhausting memory.
const jevMaxResponseBytes = 4 << 20

// jevAPIError renders a non-200 response. The API documents `detail` as a
// string or object on 400 and a detail array on 422, so fall back to the
// truncated raw body when neither shape decodes.
func jevAPIError(status int, raw []byte) string {
	var structured struct {
		Detail json.RawMessage `json:"detail"`
	}
	detail := ""
	if json.Unmarshal(raw, &structured) == nil && len(structured.Detail) > 0 {
		detail = strings.TrimSpace(string(structured.Detail))
	}
	if detail == "" {
		detail = strings.TrimSpace(string(raw))
	}
	if len(detail) > 500 {
		detail = detail[:500] + "…"
	}
	if detail == "" {
		return fmt.Sprintf("jev API returned %d", status)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Sprintf("jev API returned %d (%s) — check %s", status, detail, JevAPIKeyEnv)
	}
	return fmt.Sprintf("jev API returned %d: %s", status, detail)
}

// parseJevQuestions validates the agent-authored question spec and returns it
// for the request. Validation is shared with the handler's save-time gate via
// ValidateJevQuestionsSpec so both sides reject the same shapes with the same
// messages.
func parseJevQuestions(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, errors.New("jev agent has no jev_questions configured — set a question spec on the agent's runtime config")
	}
	if err := ValidateJevQuestionsSpec(raw); err != nil {
		return nil, err
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("jev_questions must be a JSON object: %w", err)
	}
	return spec, nil
}

// jevMaxSpecBytes bounds the stored question spec; questions are small and a
// pathological spec would ride into every request.
const jevMaxSpecBytes = 64 << 10

// jevQuestionsKey is the runtime_config key carrying the question spec.
const jevQuestionsKey = "jev_questions"

// ValidateJevQuestionsRuntimeConfig validates the `jev_questions` entry of an
// agent runtime_config (the handler's save-time gate). The key may be absent
// or null — an agent can exist before its questions are authored, and the run
// then fails with a config error naming the gap. A present spec is validated
// by ValidateJevQuestionsSpec.
func ValidateJevQuestionsRuntimeConfig(runtimeConfig []byte) error {
	spec, err := extractJevQuestionsRuntimeConfig(runtimeConfig)
	if err != nil || spec == nil {
		return err
	}
	return ValidateJevQuestionsSpec(spec)
}

func extractJevQuestionsRuntimeConfig(runtimeConfig []byte) ([]byte, error) {
	if len(bytes.TrimSpace(runtimeConfig)) == 0 {
		return nil, nil
	}
	var rc map[string]json.RawMessage
	if err := json.Unmarshal(runtimeConfig, &rc); err != nil {
		return nil, fmt.Errorf("runtime_config must be a JSON object: %w", err)
	}
	spec := rc[jevQuestionsKey]
	if len(spec) == 0 || bytes.Equal(bytes.TrimSpace(spec), []byte("null")) {
		return nil, nil
	}
	return spec, nil
}

// ValidateJevQuestionsSpec checks an agent-authored jev question spec
// (the `questions` object of the System One request). An absent spec is
// allowed — an agent can be created before its questions are authored, and
// the run then fails with a config error naming the gap. A present spec must
// be an object of question objects with a supported `type` and the criteria
// shape the API requires.
func ValidateJevQuestionsSpec(raw []byte) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if len(raw) > jevMaxSpecBytes {
		return fmt.Errorf("jev_questions exceeds %d bytes", jevMaxSpecBytes)
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(raw, &spec); err != nil {
		return fmt.Errorf("jev_questions must be a JSON object: %w", err)
	}
	if len(spec) == 0 {
		return errors.New("jev_questions is empty — a judgment needs at least one question")
	}
	ids := make([]string, 0, len(spec))
	for id := range spec {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := validateJevQuestion(id, spec[id]); err != nil {
			return err
		}
	}
	return nil
}

func validateJevQuestion(id string, raw json.RawMessage) error {
	var q struct {
		Type     string          `json:"type"`
		Criteria json.RawMessage `json:"criteria"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return fmt.Errorf("jev_questions.%s must be an object", id)
	}
	switch q.Type {
	case "choice":
		var criteria map[string]json.RawMessage
		if json.Unmarshal(q.Criteria, &criteria) != nil || len(criteria) == 0 {
			return fmt.Errorf("jev_questions.%s: a choice question needs criteria with at least one option", id)
		}
	case "score":
		var criteria []json.RawMessage
		if json.Unmarshal(q.Criteria, &criteria) != nil || len(criteria) == 0 {
			return fmt.Errorf("jev_questions.%s: a score question needs criteria with 1-10 ordered levels", id)
		}
		if len(criteria) > 10 {
			return fmt.Errorf("jev_questions.%s: a score question allows at most 10 levels, got %d", id, len(criteria))
		}
	case "noul":
		// criteria is optional; when present it must be an object.
		if len(q.Criteria) > 0 {
			var criteria map[string]json.RawMessage
			if json.Unmarshal(q.Criteria, &criteria) != nil {
				return fmt.Errorf("jev_questions.%s: noul criteria must be an object with true/false descriptions", id)
			}
		}
	default:
		return fmt.Errorf("jev_questions.%s has type %q; supported types are choice, score, noul", id, q.Type)
	}
	return nil
}

// formatJevResponse renders the judgment as the run output: one line per
// question with the decision, confidence and full distribution, then the raw
// response as JSON for downstream automations to parse.
func formatJevResponse(resp *jevResponse, raw []byte) string {
	ids := make([]string, 0, len(resp.Answers))
	for id := range resp.Answers {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var b strings.Builder
	b.WriteString("## Jev judgment\n\n")
	for _, id := range ids {
		var answer jevAnswer
		if err := json.Unmarshal(resp.Answers[id], &answer); err != nil {
			fmt.Fprintf(&b, "- **%s**: (undecodable answer)\n", id)
			continue
		}
		fmt.Fprintf(&b, "- **%s**: %s\n", id, formatJevAnswer(&answer))
	}
	b.WriteString("\n```json\n")
	b.Write(raw)
	b.WriteString("\n```\n")
	return b.String()
}

func formatJevAnswer(a *jevAnswer) string {
	switch a.Type {
	case "noul":
		if a.Noul == nil {
			return "noul (missing probability)"
		}
		return fmt.Sprintf("p(yes) = %.2f", *a.Noul)
	case "score":
		if a.Score == nil {
			return "score (missing value)"
		}
		if a.Confidence != nil {
			return fmt.Sprintf("score %.2f (confidence %.2f)", *a.Score, *a.Confidence)
		}
		return fmt.Sprintf("score %.2f", *a.Score)
	case "choice":
		var out strings.Builder
		if a.Choice != "" {
			fmt.Fprintf(&out, "`%s`", a.Choice)
		}
		if a.Confidence != nil {
			fmt.Fprintf(&out, " (confidence %.2f)", *a.Confidence)
		}
		if len(a.Probabilities) > 0 {
			out.WriteString(" — probabilities:")
			for _, name := range sortedKeys(a.Probabilities) {
				fmt.Fprintf(&out, " %s %.2f;", name, a.Probabilities[name])
			}
		}
		if out.Len() == 0 {
			return "choice (missing selection)"
		}
		return out.String()
	default:
		return fmt.Sprintf("type %q (unsupported)", a.Type)
	}
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
