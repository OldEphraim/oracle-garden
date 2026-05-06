package tools

import (
	"context"
	"encoding/json"

	"github.com/OldEphraim/sibyl-hub/api/internal/runtime"
)

// WebSearchToolType is the Anthropic-side type identifier we declare. v0
// uses the basic stable form (`web_search_20250305`); a newer
// `web_search_20260209` exists which adds code-execution-based result
// filtering, but it pulls in a separate code-execution tool dependency we
// don't want for v0. Documented in DECISION_LOG.md Phase 5.
const WebSearchToolType = "web_search_20250305"

// webSearch is the server-side wrapper for Anthropic's built-in web_search
// tool. The runtime declares it in the tools array; Anthropic executes the
// search itself and inlines the results in the response. The runtime never
// dispatches Invoke for this tool.
type webSearch struct {
	maxUses int
}

// RegisterWebSearch adds the server-side web_search tool to the registry.
// maxUses caps how many search calls a single agent step may make (passed
// to Anthropic via the `max_uses` field on the tool definition); 0 means
// unbounded — generally not what you want.
func RegisterWebSearch(r *Registry, maxUses int) {
	if maxUses <= 0 {
		maxUses = 5 // sensible default — News Scout doesn't need more than a handful per step
	}
	r.MustRegister(&webSearch{maxUses: maxUses})
}

func (t *webSearch) Name() string       { return "web_search" }
func (t *webSearch) IsServerSide() bool { return true }
func (t *webSearch) Definition() runtime.ToolDefinition {
	return runtime.ToolDefinition{
		Type:    WebSearchToolType,
		Name:    "web_search", // Anthropic-fixed name for the built-in tool
		MaxUses: t.maxUses,
	}
}

// Invoke must never run for a server-side tool. Anthropic handles execution
// itself; the response carries the search result inline. Calling this is a
// programmer bug — usually a missing IsServerSide() check at the dispatch
// site — and we want the panic to surface immediately rather than silently
// produce empty tool_result payloads.
func (t *webSearch) Invoke(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	panic("tools: web_search is server-side; Invoke should not be called locally")
}
