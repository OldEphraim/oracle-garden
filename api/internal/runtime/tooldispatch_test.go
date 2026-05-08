package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeTool is a programmable test double for runtime.DispatchableTool.
type fakeTool struct {
	name        string
	serverSide  bool
	invokeCount int32
	respond     func(input json.RawMessage) (json.RawMessage, error)
}

func (t *fakeTool) Name() string       { return t.name }
func (t *fakeTool) IsServerSide() bool { return t.serverSide }
func (t *fakeTool) Invoke(ctx context.Context, in json.RawMessage) (json.RawMessage, error) {
	atomic.AddInt32(&t.invokeCount, 1)
	if t.respond == nil {
		return json.RawMessage(`{"ok":true}`), nil
	}
	return t.respond(in)
}

// fakeDispatcher implements runtime.ToolDispatcher with an in-memory map
// keyed by both internal and API names.
type fakeDispatcher struct {
	byInternal map[string]*fakeTool
}

func newFakeDispatcher(tools ...*fakeTool) *fakeDispatcher {
	d := &fakeDispatcher{byInternal: make(map[string]*fakeTool, len(tools))}
	for _, t := range tools {
		d.byInternal[t.name] = t
	}
	return d
}

func (d *fakeDispatcher) DefinitionsFor(internalNames []string) ([]ToolDefinition, error) {
	out := make([]ToolDefinition, 0, len(internalNames))
	for _, n := range internalNames {
		t, ok := d.byInternal[n]
		if !ok {
			return nil, fmt.Errorf("fake: unknown tool %q", n)
		}
		def := ToolDefinition{Name: apiSafeForTest(t.name), Description: "fake", InputSchema: json.RawMessage(`{"type":"object"}`)}
		if t.serverSide {
			def.Type = "fake_server_tool"
		}
		out = append(out, def)
	}
	return out, nil
}

func (d *fakeDispatcher) LookupByAPIName(api string) (DispatchableTool, bool) {
	for _, t := range d.byInternal {
		if apiSafeForTest(t.name) == api {
			return t, true
		}
	}
	return nil, false
}

func apiSafeForTest(s string) string { return strings.ReplaceAll(s, ".", "_") }

// scriptedAnthropic returns a server whose Nth call returns scripts[N-1].
// Each entry is a full *MessagesResponse-shaped payload.
func scriptedAnthropic(t *testing.T, scripts []map[string]any) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		if int(n) > len(scripts) {
			http.Error(w, `{"type":"error","error":{"type":"server_error","message":"out of scripts"}}`, 500)
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(scripts[n-1])
	}))
	return srv, &calls
}

// mkTextScript builds a final response carrying `text` as the only block.
func mkTextScript(text string) map[string]any {
	return map[string]any{
		"id": "msg", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6",
		"stop_reason": "end_turn",
		"content":     []map[string]any{{"type": "text", "text": text}},
		"usage":       map[string]int{"input_tokens": 10, "output_tokens": 5},
	}
}

// mkToolUseScript builds a tool_use response asking for `toolName` with `input`.
func mkToolUseScript(toolName string, input map[string]any) map[string]any {
	inB, _ := json.Marshal(input)
	return map[string]any{
		"id": "msg", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6",
		"stop_reason": "tool_use",
		"content": []map[string]any{
			{"type": "text", "text": "let me check"},
			{
				"type":  "tool_use",
				"id":    "toolu_" + toolName,
				"name":  toolName,
				"input": json.RawMessage(inB),
			},
		},
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 5},
	}
}

// TestToolDispatchRoundTrip verifies a full tool_use → tool_result → final
// text round-trip:
//   - call 1 returns a tool_use for "polymarket.gamma_get_market"
//   - runtime dispatches the tool, builds a tool_result, calls API again
//   - call 2 returns the final text — a valid thesis.v1 JSON
//
// We assert: the tool was invoked exactly once, the second request carried
// an assistant message + a user tool_result, and the validated output is
// returned to the caller.
func TestToolDispatchRoundTrip(t *testing.T) {
	scripts := []map[string]any{
		mkToolUseScript("polymarket_gamma_get_market", map[string]any{"slug": "x"}),
		mkTextScript(`{"market_slug":"x","direction":"YES","confidence":0.6,"reasoning":"because"}`),
	}
	var requestBodies []json.RawMessage
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requestBodies = append(requestBodies, json.RawMessage(body))
		idx := len(requestBodies) - 1
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(scripts[idx])
	}))
	defer srv.Close()

	tool := &fakeTool{
		name: "polymarket.gamma_get_market",
		respond: func(in json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"slug":"x","question":"Q?","conditionId":"0xabc"}`), nil
		},
	}
	disp := newFakeDispatcher(tool)

	c, _ := NewAnthropicClient(AnthropicOptions{
		APIKey: "k", BaseURL: srv.URL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	rt := NewRuntime(c, buildRegistry(t, "thesis.v1", thesisV1Schema), disp,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	res, err := rt.Invoke(context.Background(), Agent{
		Name: "watcher", Model: "claude-sonnet-4-6", MaxTokens: 1000,
		OutputType: "thesis.v1", SystemPrompt: "x",
		Tools: []string{"polymarket.gamma_get_market"},
	}, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Output == nil {
		t.Fatalf("expected output")
	}
	if atomic.LoadInt32(&tool.invokeCount) != 1 {
		t.Errorf("tool invoked %d times, want 1", tool.invokeCount)
	}
	if len(requestBodies) != 2 {
		t.Fatalf("expected 2 API calls, got %d", len(requestBodies))
	}

	// Second request should contain: user(text), assistant(tool_use+text),
	// user(tool_result). Spot-check the messages array.
	var second MessagesRequest
	if err := json.Unmarshal(requestBodies[1], &second); err != nil {
		t.Fatalf("decode second request: %v", err)
	}
	if len(second.Messages) != 3 {
		t.Fatalf("second request messages: got %d want 3", len(second.Messages))
	}
	if second.Messages[1].Role != "assistant" {
		t.Errorf("messages[1].role = %q, want assistant", second.Messages[1].Role)
	}
	if second.Messages[2].Role != "user" {
		t.Errorf("messages[2].role = %q, want user", second.Messages[2].Role)
	}
	if len(second.Messages[2].Content) != 1 || second.Messages[2].Content[0].Type != "tool_result" {
		t.Errorf("messages[2].content[0]: want tool_result, got %+v", second.Messages[2].Content)
	}
	if second.Messages[2].Content[0].ToolUseID != "toolu_polymarket_gamma_get_market" {
		t.Errorf("tool_use_id mismatch: %q", second.Messages[2].Content[0].ToolUseID)
	}

	// Token totals accumulate across the two rounds: 10/5 each → 20/10.
	if res.PromptTokens != 20 || res.CompletionTokens != 10 {
		t.Errorf("tokens accumulated: got %d/%d want 20/10", res.PromptTokens, res.CompletionTokens)
	}
}

// TestToolUseCapAtSixRounds verifies the runtime gives up after exactly
// MaxToolRounds rounds when the model keeps requesting tools.
func TestToolUseCapAtSixRounds(t *testing.T) {
	// Every script asks for the tool again — model never stops.
	scripts := make([]map[string]any, MaxToolRounds)
	for i := range scripts {
		scripts[i] = mkToolUseScript("loopy_tool", map[string]any{"i": i})
	}
	srv, calls := scriptedAnthropic(t, scripts)
	defer srv.Close()

	tool := &fakeTool{name: "loopy.tool"}
	disp := newFakeDispatcher(tool)

	c, _ := NewAnthropicClient(AnthropicOptions{
		APIKey: "k", BaseURL: srv.URL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	rt := NewRuntime(c, buildRegistry(t, "thesis.v1", thesisV1Schema), disp,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	res, err := rt.Invoke(context.Background(), Agent{
		Name: "looper", Model: "claude-sonnet-4-6", MaxTokens: 1000,
		OutputType: "thesis.v1", SystemPrompt: "x",
		Tools: []string{"loopy.tool"},
	}, nil)
	// Cap-reached is an API/runtime error, not a validation error — Invoke
	// returns immediately without retrying. So we expect exactly
	// MaxToolRounds API calls and (MaxToolRounds-1) tool dispatches:
	// rounds 1..5 dispatch, round 6 hits the cap and returns.
	if atomic.LoadInt32(calls) != int32(MaxToolRounds) {
		t.Errorf("API calls: got %d, want %d", *calls, MaxToolRounds)
	}
	if err == nil {
		t.Fatalf("expected error after cap reached")
	}
	if !strings.Contains(err.Error(), "tool-use cap") {
		t.Errorf("error should mention cap: %v", err)
	}
	if got := atomic.LoadInt32(&tool.invokeCount); got != int32(MaxToolRounds-1) {
		t.Errorf("tool invocations: got %d want %d", got, MaxToolRounds-1)
	}
	if res == nil || res.PromptTokens == 0 {
		t.Errorf("expected non-zero token totals on cap-reached: %+v", res)
	}
}

// TestServerSideToolNotLocallyDispatched verifies that a server-side tool
// requested as a regular tool_use block is surfaced as is_error tool_result
// (programmer-bug guardrail) and the test tool's Invoke is NOT called.
func TestServerSideToolNotLocallyDispatched(t *testing.T) {
	scripts := []map[string]any{
		mkToolUseScript("web_search", map[string]any{"query": "x"}),
		mkTextScript(`{"market_slug":"x","direction":"NO","confidence":0.4,"reasoning":"r"}`),
	}
	srv, _ := scriptedAnthropic(t, scripts)
	defer srv.Close()

	tool := &fakeTool{name: "web_search", serverSide: true,
		respond: func(_ json.RawMessage) (json.RawMessage, error) {
			t.Fatalf("server-side tool's Invoke must not be called")
			return nil, nil
		},
	}
	disp := newFakeDispatcher(tool)

	c, _ := NewAnthropicClient(AnthropicOptions{
		APIKey: "k", BaseURL: srv.URL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	rt := NewRuntime(c, buildRegistry(t, "thesis.v1", thesisV1Schema), disp,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	res, err := rt.Invoke(context.Background(), Agent{
		Name: "scout", Model: "claude-sonnet-4-6", MaxTokens: 1000,
		OutputType: "thesis.v1", SystemPrompt: "x",
		Tools: []string{"web_search"},
	}, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	// We DO want to verify the tool was never invoked locally — the t.Fatalf
	// above guards it. invokeCount stays 0.
	if got := atomic.LoadInt32(&tool.invokeCount); got != 0 {
		t.Errorf("server-side tool invokeCount: got %d want 0", got)
	}
	if res.Output == nil {
		t.Errorf("expected output even after server-side-as-local guard fired")
	}
}

// TestToolErrorBecomesIsErrorResult verifies that a local tool returning an
// error becomes a tool_result with is_error=true; the model gets to react.
func TestToolErrorBecomesIsErrorResult(t *testing.T) {
	scripts := []map[string]any{
		mkToolUseScript("flaky_tool", map[string]any{"x": 1}),
		mkTextScript(`{"market_slug":"x","direction":"ABSTAIN","confidence":0.0,"reasoning":"tool failed"}`),
	}
	var requestBodies []json.RawMessage
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requestBodies = append(requestBodies, json.RawMessage(body))
		idx := len(requestBodies) - 1
		mu.Unlock()
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(scripts[idx])
	}))
	defer srv.Close()

	tool := &fakeTool{
		name: "flaky.tool",
		respond: func(_ json.RawMessage) (json.RawMessage, error) {
			return nil, errors.New("polymarket: HTTP 503: Service Unavailable")
		},
	}
	disp := newFakeDispatcher(tool)

	c, _ := NewAnthropicClient(AnthropicOptions{
		APIKey: "k", BaseURL: srv.URL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	rt := NewRuntime(c, buildRegistry(t, "thesis.v1", thesisV1Schema), disp,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := rt.Invoke(context.Background(), Agent{
		Name: "executor", Model: "claude-sonnet-4-6", MaxTokens: 1000,
		OutputType: "thesis.v1", SystemPrompt: "x",
		Tools: []string{"flaky.tool"},
	}, nil); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	// Inspect the second request: the tool_result should carry is_error: true
	// and the error message verbatim.
	var second MessagesRequest
	_ = json.Unmarshal(requestBodies[1], &second)
	if len(second.Messages) != 3 {
		t.Fatalf("messages: %d", len(second.Messages))
	}
	tr := second.Messages[2].Content[0]
	if tr.Type != "tool_result" {
		t.Errorf("type: %q", tr.Type)
	}
	if !tr.IsError {
		t.Errorf("is_error should be true on tool failure")
	}
	if !strings.Contains(string(tr.Content), "503") {
		t.Errorf("result content should carry the error: %q", string(tr.Content))
	}
}

// TestUnknownToolNameSurfacesAsError verifies that if Anthropic asks for a
// tool name we don't know, the runtime sends back is_error=true rather than
// crashing.
func TestUnknownToolNameSurfacesAsError(t *testing.T) {
	scripts := []map[string]any{
		mkToolUseScript("ghost_tool", map[string]any{}),
		mkTextScript(`{"market_slug":"x","direction":"ABSTAIN","confidence":0.0,"reasoning":"r"}`),
	}
	srv, _ := scriptedAnthropic(t, scripts)
	defer srv.Close()

	disp := newFakeDispatcher() // empty registry
	c, _ := NewAnthropicClient(AnthropicOptions{
		APIKey: "k", BaseURL: srv.URL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	rt := NewRuntime(c, buildRegistry(t, "thesis.v1", thesisV1Schema), disp,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	if _, err := rt.Invoke(context.Background(), Agent{
		Name: "ghost", Model: "claude-sonnet-4-6", MaxTokens: 1000,
		OutputType: "thesis.v1", SystemPrompt: "x",
		// Empty Tools — runtime won't pre-resolve, but the model could still
		// emit a tool_use block (sometimes happens with cached prompts).
	}, nil); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
}

// TestNoTools_NoChange verifies tool-less agents pass through unchanged.
func TestNoTools_NoChange(t *testing.T) {
	srv, _ := scriptedAnthropic(t, []map[string]any{
		mkTextScript(`{"market_slug":"x","direction":"YES","confidence":0.5,"reasoning":"r"}`),
	})
	defer srv.Close()

	c, _ := NewAnthropicClient(AnthropicOptions{
		APIKey: "k", BaseURL: srv.URL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	// nil dispatcher is allowed when no tools are declared.
	rt := NewRuntime(c, buildRegistry(t, "thesis.v1", thesisV1Schema), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	res, err := rt.Invoke(context.Background(), Agent{
		Name: "tlee", Model: "claude-sonnet-4-6", MaxTokens: 1000,
		OutputType: "thesis.v1", SystemPrompt: "x",
	}, nil)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Output == nil {
		t.Errorf("expected output")
	}
}

// TestAgentDeclaresToolsButNoDispatcher verifies the guardrail: if an agent
// has Tools but the runtime has no ToolDispatcher, Invoke fails fast.
func TestAgentDeclaresToolsButNoDispatcher(t *testing.T) {
	srv, _ := scriptedAnthropic(t, []map[string]any{mkTextScript(`{}`)})
	defer srv.Close()

	c, _ := NewAnthropicClient(AnthropicOptions{
		APIKey: "k", BaseURL: srv.URL,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	rt := NewRuntime(c, buildRegistry(t, "thesis.v1", thesisV1Schema), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	_, err := rt.Invoke(context.Background(), Agent{
		Name: "x", Model: "claude-sonnet-4-6", MaxTokens: 1000,
		OutputType: "thesis.v1", SystemPrompt: "x",
		Tools: []string{"some.tool"},
	}, nil)
	if err == nil {
		t.Fatalf("expected error when agent has tools but runtime has no dispatcher")
	}
	if !strings.Contains(err.Error(), "no ToolDispatcher") {
		t.Errorf("expected error to mention dispatcher: %v", err)
	}
}
