package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/OldEphraim/sibyl-hub/api/internal/types"
)

// quietRuntime returns a Runtime wired to:
//   - an Anthropic client pointed at the given httptest server
//   - a registry pre-populated with one schema (typeID -> schemaJSON)
//   - a discarded logger
func quietRuntime(t *testing.T, server *httptest.Server, typeID, schemaJSON string) *Runtime {
	t.Helper()
	c, err := NewAnthropicClient(AnthropicOptions{
		APIKey:  "test-key",
		BaseURL: server.URL,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("NewAnthropicClient: %v", err)
	}
	reg := buildRegistry(t, typeID, schemaJSON)
	return NewRuntime(c, reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// buildRegistry assembles a *types.Registry from an in-memory map. Avoids
// pulling in the DB-backed Load() path in unit tests.
func buildRegistry(t *testing.T, id, schemaJSON string) *types.Registry {
	t.Helper()
	parsed, err := jsonschema.UnmarshalJSON(strings.NewReader(schemaJSON))
	if err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("test://"+id, parsed); err != nil {
		t.Fatalf("add resource: %v", err)
	}
	sch, err := c.Compile("test://" + id)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	parts := strings.SplitN(id, ".", 2)
	def := &types.Definition{
		Name:      parts[0],
		Version:   parts[1],
		IsCore:    false,
		RawSchema: []byte(schemaJSON),
		Compiled:  sch,
	}
	reg := types.NewRegistryForTest(map[string]*types.Definition{id: def})
	return reg
}

// thesisV1Schema is small enough to inline. Mirrors TYPES.md.
const thesisV1Schema = `{
	"$schema": "https://json-schema.org/draft/2020-12/schema",
	"type": "object",
	"required": ["market_slug","direction","confidence","reasoning"],
	"properties": {
		"market_slug": { "type": "string" },
		"direction":   { "type": "string", "enum": ["YES","NO","ABSTAIN"] },
		"confidence":  { "type": "number", "minimum": 0, "maximum": 1 },
		"reasoning":   { "type": "string" },
		"evidence":    { "type": "array", "items": { "type": "string" } }
	},
	"additionalProperties": false
}`

// fakeAnthropic builds a httptest server that returns a sequence of canned
// responses, one per call. The Nth call (1-indexed) returns responses[N-1];
// past the end, the server returns 500.
//
// Each entry in `responses` is the literal text the assistant emits — wrapped
// in a Messages-API envelope here. usageIn/usageOut are constants we report
// per call.
func fakeAnthropic(t *testing.T, responses []string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if int(n) > len(responses) {
			http.Error(w, `{"type":"error","error":{"type":"server_error","message":"out of canned responses"}}`, 500)
			return
		}
		text := responses[n-1]
		body := map[string]any{
			"id":          fmt.Sprintf("msg_%d", n),
			"type":        "message",
			"role":        "assistant",
			"model":       "claude-sonnet-4-6",
			"stop_reason": "end_turn",
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
			"usage": map[string]int{"input_tokens": 100, "output_tokens": 50},
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	return srv, &calls
}

func TestInvokeHappyPath(t *testing.T) {
	srv, calls := fakeAnthropic(t, []string{
		`{"market_slug":"x","direction":"YES","confidence":0.7,"reasoning":"r"}`,
	})
	defer srv.Close()
	rt := quietRuntime(t, srv, "thesis.v1", thesisV1Schema)

	res, err := rt.Invoke(context.Background(), Agent{
		Name: "test", Model: "claude-sonnet-4-6", MaxTokens: 1000, Temperature: 0.7,
		SystemPrompt: "You write theses.", OutputType: "thesis.v1",
	}, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Output == nil {
		t.Fatalf("expected output")
	}
	if res.Attempts != 1 {
		t.Errorf("attempts: got %d want 1", res.Attempts)
	}
	if atomic.LoadInt32(calls) != 1 {
		t.Errorf("anthropic calls: got %d want 1", *calls)
	}
	// Cost: 100 input * 3/MTok + 50 output * 15/MTok = 0.0003 + 0.00075 = 0.00105.
	if res.CostUSD < 0.001 || res.CostUSD > 0.0011 {
		t.Errorf("cost: got %v want ~0.00105", res.CostUSD)
	}
	if res.PromptTokens != 100 || res.CompletionTokens != 50 {
		t.Errorf("tokens: %d/%d", res.PromptTokens, res.CompletionTokens)
	}
}

func TestInvokeStripsJSONFences(t *testing.T) {
	srv, _ := fakeAnthropic(t, []string{
		"```json\n" +
			`{"market_slug":"x","direction":"NO","confidence":0.4,"reasoning":"r"}` +
			"\n```",
	})
	defer srv.Close()
	rt := quietRuntime(t, srv, "thesis.v1", thesisV1Schema)

	res, err := rt.Invoke(context.Background(), Agent{
		Name: "t", Model: "claude-sonnet-4-6", MaxTokens: 1000, OutputType: "thesis.v1", SystemPrompt: "x",
	}, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var parsed map[string]any
	_ = json.Unmarshal(res.Output, &parsed)
	if parsed["direction"] != "NO" {
		t.Errorf("direction: got %v want NO", parsed["direction"])
	}
}

func TestInvokeRetriesOnParseFailure(t *testing.T) {
	srv, calls := fakeAnthropic(t, []string{
		"this is not JSON at all",
		`{"market_slug":"x","direction":"YES","confidence":0.7,"reasoning":"r"}`,
	})
	defer srv.Close()
	rt := quietRuntime(t, srv, "thesis.v1", thesisV1Schema)

	res, err := rt.Invoke(context.Background(), Agent{
		Name: "t", Model: "claude-sonnet-4-6", MaxTokens: 1000, OutputType: "thesis.v1", SystemPrompt: "x",
	}, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Attempts != 2 {
		t.Errorf("attempts: got %d want 2", res.Attempts)
	}
	if atomic.LoadInt32(calls) != 2 {
		t.Errorf("anthropic calls: got %d want 2", *calls)
	}
	// Token totals accumulate across attempts: 100/50 each → 200/100.
	if res.PromptTokens != 200 || res.CompletionTokens != 100 {
		t.Errorf("tokens: got %d/%d want 200/100", res.PromptTokens, res.CompletionTokens)
	}
}

func TestInvokeRetriesOnValidationFailure(t *testing.T) {
	// First attempt: parses fine but missing 'reasoning' (required) and
	// confidence is out of range (>1). Second attempt: clean.
	srv, calls := fakeAnthropic(t, []string{
		`{"market_slug":"x","direction":"YES","confidence":1.7}`,
		`{"market_slug":"x","direction":"YES","confidence":0.6,"reasoning":"because"}`,
	})
	defer srv.Close()
	rt := quietRuntime(t, srv, "thesis.v1", thesisV1Schema)

	res, err := rt.Invoke(context.Background(), Agent{
		Name: "t", Model: "claude-sonnet-4-6", MaxTokens: 1000, OutputType: "thesis.v1", SystemPrompt: "x",
	}, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Attempts != 2 {
		t.Errorf("attempts: got %d want 2", res.Attempts)
	}
	if atomic.LoadInt32(calls) != 2 {
		t.Errorf("anthropic calls: got %d want 2", *calls)
	}
	if res.Output == nil {
		t.Errorf("expected non-nil output after retry")
	}
}

func TestInvokeBothAttemptsFail(t *testing.T) {
	// Both attempts return invalid output; the runtime gives up.
	srv, calls := fakeAnthropic(t, []string{
		`{"market_slug":"x"}`,                                  // missing required
		`{"market_slug":"x","direction":"MAYBE","confidence":0.5,"reasoning":"r"}`, // direction not in enum
	})
	defer srv.Close()
	rt := quietRuntime(t, srv, "thesis.v1", thesisV1Schema)

	res, err := rt.Invoke(context.Background(), Agent{
		Name: "t", Model: "claude-sonnet-4-6", MaxTokens: 1000, OutputType: "thesis.v1", SystemPrompt: "x",
	}, nil)
	if err == nil {
		t.Fatalf("expected error after 2 failed attempts")
	}
	if res == nil {
		t.Fatalf("expected non-nil result with cost data even on failure")
	}
	if res.Output != nil {
		t.Errorf("expected nil Output on failure, got %s", res.Output)
	}
	if res.Attempts != 2 {
		t.Errorf("attempts: got %d want 2", res.Attempts)
	}
	if atomic.LoadInt32(calls) != 2 {
		t.Errorf("anthropic calls: got %d want 2", *calls)
	}
	// Cost still accumulates — we paid for those tokens.
	if res.PromptTokens != 200 || res.CompletionTokens != 100 {
		t.Errorf("tokens accumulated: got %d/%d want 200/100", res.PromptTokens, res.CompletionTokens)
	}
	if res.CostUSD == 0 {
		t.Errorf("cost should be > 0 on failure (already-paid-for tokens)")
	}
	if !strings.Contains(err.Error(), "after 2 attempts") {
		t.Errorf("error should mention attempts, got: %v", err)
	}
}

func TestInvokeUnknownModelLogsWARNAndZeroCost(t *testing.T) {
	srv, _ := fakeAnthropic(t, []string{
		`{"market_slug":"x","direction":"YES","confidence":0.5,"reasoning":"r"}`,
	})
	defer srv.Close()
	rt := quietRuntime(t, srv, "thesis.v1", thesisV1Schema)

	res, err := rt.Invoke(context.Background(), Agent{
		Name: "t", Model: "claude-fake-9-9", MaxTokens: 1000, OutputType: "thesis.v1", SystemPrompt: "x",
	}, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.CostUSD != 0 {
		t.Errorf("unknown model: cost = %v, want 0 (the WARN log goes to slog.Default)", res.CostUSD)
	}
	if res.PromptTokens != 100 {
		t.Errorf("tokens still recorded: got %d", res.PromptTokens)
	}
}

func TestInvokeUnknownOutputType(t *testing.T) {
	// Build a registry that knows about thesis.v1, then ask for fake.v1.
	srv, _ := fakeAnthropic(t, []string{`{}`})
	defer srv.Close()
	rt := quietRuntime(t, srv, "thesis.v1", thesisV1Schema)

	_, err := rt.Invoke(context.Background(), Agent{
		Name: "t", Model: "claude-sonnet-4-6", MaxTokens: 1000, OutputType: "fake.v1", SystemPrompt: "x",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "fake.v1") {
		t.Errorf("expected error mentioning fake.v1, got: %v", err)
	}
}

func TestInvokeAnthropicAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()
	rt := quietRuntime(t, srv, "thesis.v1", thesisV1Schema)

	_, err := rt.Invoke(context.Background(), Agent{
		Name: "t", Model: "claude-sonnet-4-6", MaxTokens: 1000, OutputType: "thesis.v1", SystemPrompt: "x",
	}, nil)
	if err == nil {
		t.Fatalf("expected error")
	}
	ae, ok := IsAnthropicError(err)
	if !ok {
		t.Fatalf("expected *AnthropicError, got %T: %v", err, err)
	}
	if ae.StatusCode != 401 {
		t.Errorf("status: %d", ae.StatusCode)
	}
	if ae.Err.Type != "authentication_error" {
		t.Errorf("err.type: %q", ae.Err.Type)
	}
}

func TestInvokeMultiInputPreambleIncludesKeys(t *testing.T) {
	// Inspect the system prompt the request carried — the preamble should
	// list every input key when len(mergedInputs) > 1.
	var seenSystem string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req MessagesRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seenSystem = req.System
		body := map[string]any{
			"id": "x", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6",
			"stop_reason": "end_turn",
			"content": []map[string]any{
				{"type": "text", "text": `{"market_slug":"x","direction":"YES","confidence":0.5,"reasoning":"r"}`},
			},
			"usage": map[string]int{"input_tokens": 1, "output_tokens": 1},
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()
	rt := quietRuntime(t, srv, "thesis.v1", thesisV1Schema)

	inputs := map[string]json.RawMessage{
		"watcher": json.RawMessage(`{"a":1}`),
		"scout":   json.RawMessage(`{"b":2}`),
	}
	if _, err := rt.Invoke(context.Background(), Agent{
		Name: "t", Model: "claude-sonnet-4-6", MaxTokens: 1000, OutputType: "thesis.v1", SystemPrompt: "Be Sibyl.",
	}, inputs); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(seenSystem, "Be Sibyl.") {
		t.Errorf("system should include base prompt")
	}
	if !strings.Contains(seenSystem, "watcher") || !strings.Contains(seenSystem, "scout") {
		t.Errorf("system should list both keys; got: %s", seenSystem)
	}
}

func TestStripJSONFences(t *testing.T) {
	cases := []struct{ in, want string }{
		{"```json\n{\"a\":1}\n```", `{"a":1}`},
		{"```\n{\"a\":1}\n```", `{"a":1}`},
		{`{"a":1}`, `{"a":1}`},
		{"```json\n{\"a\":1}", `{"a":1}`},
		{"  ```json\n{\"a\":1}\n```  ", `{"a":1}`},
	}
	for _, c := range cases {
		if got := stripJSONFences(c.in); got != c.want {
			t.Errorf("stripJSONFences(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
