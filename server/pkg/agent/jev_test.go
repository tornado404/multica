package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const jevValidQuestions = `{
	"tone": {"type": "choice", "instructions": "What is the tone?", "criteria": {"angry": "hostile", "calm": "polite"}},
	"refund_requested": {"type": "noul", "instructions": "Does the ticket request a refund?"},
	"urgency": {"type": "score", "instructions": "How urgent?", "criteria": ["calm", "concerned", "critical"]}
}`

const jevSuccessResponse = `{
	"model": "jev-latest",
	"answers": {
		"tone": {"type": "choice", "choice": "calm", "confidence": 0.7, "probabilities": {"angry": 0.15, "calm": 0.8, "excited": 0.05}},
		"refund_requested": {"type": "noul", "noul": 0.92},
		"urgency": {"type": "score", "score": 2.4, "confidence": 0.61, "probabilities": {"0": 0.1, "1": 0.2, "2": 0.5, "3": 0.2}}
	},
	"usage": {"input_tokens": 120, "output_tokens": 12}
}`

// jevTestConfig builds a Config whose base URL points at the test server, with
// the API key delivered the way the daemon delivers it: through the agent env.
func jevTestConfig(t *testing.T, server *httptest.Server) Config {
	t.Helper()
	return Config{
		Logger:         discardLogger(),
		Env:            map[string]string{JevAPIKeyEnv: "test-key", JevBaseURLEnv: server.URL},
		ExecutablePath: "unused",
	}
}

// drainJevResult consumes the session to its terminal result. The backend
// closes Messages before sending Result (the Session contract), so draining
// Messages to completion first keeps the read ordered.
func drainJevResult(t *testing.T, s *Session) Result {
	t.Helper()
	for range s.Messages {
	}
	select {
	case res, ok := <-s.Result:
		if !ok {
			t.Fatal("result channel closed without a value")
		}
		return res
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for jev result")
	}
	return Result{}
}

func TestJevExecuteSuccess(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody jevRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(jevSuccessResponse))
	}))
	defer server.Close()

	backend, err := New("jev", jevTestConfig(t, server))
	if err != nil {
		t.Fatalf("New(jev): %v", err)
	}
	session, err := backend.Execute(context.Background(), "The customer was charged twice.", ExecOptions{
		JevQuestions: json.RawMessage(jevValidQuestions),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	result := drainJevResult(t, session)

	if result.Status != "completed" {
		t.Fatalf("status = %q, want completed (error: %q)", result.Status, result.Error)
	}
	if gotPath != "/v1/systemone" {
		t.Errorf("request path = %q, want /v1/systemone", gotPath)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("authorization = %q, want bearer key from agent env", gotAuth)
	}
	if gotBody.Model != JevDefaultModel {
		t.Errorf("request model = %q, want default %q", gotBody.Model, JevDefaultModel)
	}
	if gotBody.State != "The customer was charged twice." {
		t.Errorf("request state = %q, want the prompt verbatim", gotBody.State)
	}
	if len(gotBody.Questions) != 3 {
		t.Errorf("request questions = %d entries, want 3 forwarded from the spec", len(gotBody.Questions))
	}

	// The human-readable summary must carry choice, confidence and the full
	// distribution — downstream automations parse these lines.
	for _, want := range []string{"`calm`", "confidence 0.70", "angry 0.15", "p(yes) = 0.92", "score 2.40"} {
		if !strings.Contains(result.Output, want) {
			t.Errorf("output missing %q:\n%s", want, result.Output)
		}
	}
	if !strings.Contains(result.Output, "```json") || !strings.Contains(result.Output, "\"choice\": \"calm\"") {
		t.Errorf("output missing raw JSON block:\n%s", result.Output)
	}

	usage, ok := result.Usage["jev-latest"]
	if !ok {
		t.Fatalf("usage missing model key jev-latest: %+v", result.Usage)
	}
	if usage.InputTokens != 120 || usage.OutputTokens != 12 {
		t.Errorf("usage = %+v, want input 120 output 12", usage)
	}
}

func TestJevExecuteModelOverride(t *testing.T) {
	var gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body jevRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		_, _ = w.Write([]byte(jevSuccessResponse))
	}))
	defer server.Close()

	backend, _ := New("jev", jevTestConfig(t, server))
	session, err := backend.Execute(context.Background(), "state", ExecOptions{
		Model:        "jev-preview",
		JevQuestions: json.RawMessage(jevValidQuestions),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result := drainJevResult(t, session); result.Status != "completed" {
		t.Fatalf("status = %q, error %q", result.Status, result.Error)
	}
	if gotModel != "jev-preview" {
		t.Errorf("request model = %q, want explicit agent model jev-preview", gotModel)
	}
}

func TestJevExecuteConfigErrors(t *testing.T) {
	cases := []struct {
		name      string
		env       map[string]string
		questions json.RawMessage
		prompt    string
		wantErr   string
	}{
		{
			name:      "missing api key",
			env:       map[string]string{},
			questions: json.RawMessage(jevValidQuestions),
			prompt:    "state",
			wantErr:   JevAPIKeyEnv,
		},
		{
			name:      "no questions spec",
			env:       map[string]string{JevAPIKeyEnv: "k"},
			questions: nil,
			prompt:    "state",
			wantErr:   "jev_questions",
		},
		{
			name:      "empty questions spec",
			env:       map[string]string{JevAPIKeyEnv: "k"},
			questions: json.RawMessage(`{}`),
			prompt:    "state",
			wantErr:   "at least one question",
		},
		{
			name:      "unsupported question type",
			env:       map[string]string{JevAPIKeyEnv: "k"},
			questions: json.RawMessage(`{"box_q": {"type": "box"}}`),
			prompt:    "state",
			wantErr:   "choice, score, noul",
		},
		{
			name:      "empty state",
			env:       map[string]string{JevAPIKeyEnv: "k"},
			questions: json.RawMessage(jevValidQuestions),
			prompt:    "   ",
			wantErr:   "no state text",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No server: config errors must fail before any network call.
			backend, err := New("jev", Config{Env: tc.env})
			if err != nil {
				t.Fatalf("New(jev): %v", err)
			}
			_, err = backend.Execute(context.Background(), tc.prompt, ExecOptions{JevQuestions: tc.questions})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute error = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

func TestJevAPIErrorSurfacesDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail": "questions.tone.criteria exceeds 255 options"}`))
	}))
	defer server.Close()

	backend, _ := New("jev", jevTestConfig(t, server))
	session, err := backend.Execute(context.Background(), "state", ExecOptions{
		JevQuestions: json.RawMessage(jevValidQuestions),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	result := drainJevResult(t, session)
	if result.Status != "failed" {
		t.Fatalf("status = %q, want failed", result.Status)
	}
	if !strings.Contains(result.Error, "400") || !strings.Contains(result.Error, "exceeds 255 options") {
		t.Errorf("error = %q, want status and API detail", result.Error)
	}
}

func TestJevAPIErrorAuthNamesKeyEnv(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail": "invalid api key"}`))
	}))
	defer server.Close()

	backend, _ := New("jev", jevTestConfig(t, server))
	session, _ := backend.Execute(context.Background(), "state", ExecOptions{
		JevQuestions: json.RawMessage(jevValidQuestions),
	})
	result := drainJevResult(t, session)
	if !strings.Contains(result.Error, JevAPIKeyEnv) {
		t.Errorf("auth error %q should point at %s", result.Error, JevAPIKeyEnv)
	}
}

func TestJevContextCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	backend, _ := New("jev", jevTestConfig(t, server))
	ctx, cancel := context.WithCancel(context.Background())
	session, err := backend.Execute(ctx, "state", ExecOptions{
		JevQuestions: json.RawMessage(jevValidQuestions),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	cancel()
	result := drainJevResult(t, session)
	if result.Status != "cancelled" {
		t.Fatalf("status = %q, want cancelled (error: %q)", result.Status, result.Error)
	}
}

func TestJevListModelsStaticCatalog(t *testing.T) {
	catalog, err := ListModels(context.Background(), "jev", Command{})
	if err != nil {
		t.Fatalf("ListModels(jev): %v", err)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].ID != JevDefaultModel {
		t.Fatalf("models = %+v, want single %q entry", catalog.Models, JevDefaultModel)
	}
	if catalog.Models[0].Thinking != nil {
		t.Errorf("jev model advertises thinking levels; judgment models have none")
	}
}

func TestValidateJevQuestionsSpec(t *testing.T) {
	valid := []string{
		"",
		"  ",
		`{"tone": {"type": "noul"}}`,
		jevValidQuestions,
		`{"a": {"type": "noul", "criteria": {"true": "wants it", "false": "does not"}}}`,
	}
	for _, raw := range valid {
		if err := ValidateJevQuestionsSpec([]byte(raw)); err != nil {
			t.Errorf("ValidateJevQuestionsSpec(%s) = %v, want nil", raw, err)
		}
	}

	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"not an object", `["tone"]`, "must be a JSON object"},
		{"explicitly empty", `{}`, "at least one question"},
		{"question not an object", `{"tone": "calm"}`, "jev_questions.tone"},
		{"unknown type", `{"q": {"type": "box"}}`, "choice, score, noul"},
		{"missing type", `{"q": {"instructions": "?"}}`, "choice, score, noul"},
		{"choice without criteria", `{"q": {"type": "choice"}}`, "at least one option"},
		{"choice empty criteria", `{"q": {"type": "choice", "criteria": {}}}`, "at least one option"},
		{"score without criteria", `{"q": {"type": "score"}}`, "1-10 ordered levels"},
		{"score object criteria", `{"q": {"type": "score", "criteria": {"low": "x"}}}`, "1-10 ordered levels"},
		{"score too many levels", `{"q": {"type": "score", "criteria": ["1","2","3","4","5","6","7","8","9","10","11"]}}`, "at most 10 levels"},
		{"noul non-object criteria", `{"q": {"type": "noul", "criteria": ["yes"]}}`, "must be an object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateJevQuestionsSpec([]byte(tc.raw))
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateJevQuestionsSpec(%s) = %v, want it to contain %q", tc.raw, err, tc.wantErr)
			}
		})
	}
}
