// Package tools is the registry of agent-callable tools. v0 ships a fixed
// set; user-built agents pick from this set in the agent_templates.tools
// column. Custom tools (registered by users) are a v2 concern (CLAUDE.md
// "Out of Scope").
//
// Two kinds of tools exist:
//
//   - Local tools (e.g. all polymarket.*): the agent runtime invokes
//     Tool.Invoke(ctx, args) when the assistant emits a tool_use block,
//     formats the result as a tool_result, and feeds it back to the model.
//   - Server-side tools (e.g. web_search): handled by Anthropic itself.
//     The runtime declares them in the request's `tools` array but never
//     dispatches them — the response carries the result inline as part of
//     content. Tool.Invoke on these panics; calling it is a programmer
//     error.
//
// Tool naming convention. Internal names use dotted form (matches CLAUDE.md
// table — "polymarket.gamma_get_market"). Anthropic's API rejects dots in
// tool names (regex ^[a-zA-Z0-9_-]{1,64}$), so the registry maintains an
// API-safe alias (underscores) for the wire-level Name. Callers should use
// the dotted form everywhere; the registry handles the translation at the
// API boundary.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/OldEphraim/sibyl-hub/api/internal/runtime"
)

// Tool is the interface every registered tool implements.
type Tool interface {
	// Name returns the canonical (dotted) internal identifier, e.g.
	// "polymarket.gamma_get_market". This is what's stored in
	// agent_templates.tools and shown in the agent-builder UI.
	Name() string

	// Definition returns the wire-level tool definition Anthropic sees.
	// For local tools, includes input_schema. For server-side tools,
	// includes the type field (e.g. "web_search_20250305").
	Definition() runtime.ToolDefinition

	// IsServerSide reports whether Anthropic executes this tool itself.
	// True for web_search; false for everything else.
	IsServerSide() bool

	// Invoke runs the tool with the input the assistant emitted in its
	// tool_use block. Result is the JSON-stringified payload the runtime
	// wraps into a tool_result block.
	//
	// Server-side tools panic on Invoke — they should never be locally
	// dispatched; calling them is a programmer error.
	Invoke(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

// Registry holds the registered tools, keyed by both internal name (dots)
// and API name (underscores) so tool_use lookups can come back via either
// path.
type Registry struct {
	byInternal map[string]Tool
	byAPI      map[string]Tool
}

// NewRegistry returns an empty registry. Use Register(t Tool) to add tools.
func NewRegistry() *Registry {
	return &Registry{
		byInternal: make(map[string]Tool),
		byAPI:      make(map[string]Tool),
	}
}

// Register adds a tool to both maps. Returns an error on duplicate registration
// or invalid name.
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return fmt.Errorf("tools: Register: nil tool")
	}
	internal := t.Name()
	if internal == "" {
		return fmt.Errorf("tools: Register: empty Name()")
	}
	api := APISafeName(internal)
	if _, dup := r.byInternal[internal]; dup {
		return fmt.Errorf("tools: Register: duplicate internal name %q", internal)
	}
	if _, dup := r.byAPI[api]; dup {
		return fmt.Errorf("tools: Register: duplicate API name %q (internal=%q)", api, internal)
	}
	r.byInternal[internal] = t
	r.byAPI[api] = t
	return nil
}

// MustRegister panics on error — use at startup when registering the static
// v0 tool set, where any failure is a programmer mistake.
func (r *Registry) MustRegister(t Tool) {
	if err := r.Register(t); err != nil {
		panic(err)
	}
}

// Get looks up a tool by its internal (dotted) name.
func (r *Registry) Get(internal string) (Tool, bool) {
	t, ok := r.byInternal[internal]
	return t, ok
}

// LookupByAPIName looks up a tool by the underscore-flavored name Anthropic
// echoes in tool_use.name. The return type is the runtime-side
// DispatchableTool interface so *Registry satisfies runtime.ToolDispatcher
// directly — no adapter needed at wiring sites.
func (r *Registry) LookupByAPIName(api string) (runtime.DispatchableTool, bool) {
	t, ok := r.byAPI[api]
	if !ok {
		return nil, false
	}
	return t, true
}

// DefinitionsFor builds the runtime.ToolDefinition slice for a given list of
// internal tool names — what the runtime passes to Anthropic's `tools`
// field. Unknown names return an error (callers should validate at agent-
// save time, but a runtime error is the safety net).
func (r *Registry) DefinitionsFor(internalNames []string) ([]runtime.ToolDefinition, error) {
	if len(internalNames) == 0 {
		return nil, nil
	}
	out := make([]runtime.ToolDefinition, 0, len(internalNames))
	for _, n := range internalNames {
		t, ok := r.byInternal[n]
		if !ok {
			return nil, fmt.Errorf("tools: DefinitionsFor: unknown tool %q", n)
		}
		out = append(out, t.Definition())
	}
	return out, nil
}

// All returns every registered tool, sorted by internal name. Useful for
// the agent-builder UI's tool-picker (Phase 11).
func (r *Registry) All() []Tool {
	out := make([]Tool, 0, len(r.byInternal))
	for _, t := range r.byInternal {
		out = append(out, t)
	}
	sortToolsByName(out)
	return out
}

// APISafeName translates a dotted internal name (CLAUDE.md convention) into
// the underscore form Anthropic accepts. Pure function, exported so tests
// and the engine can compute names without going through the registry.
func APISafeName(internal string) string {
	return strings.ReplaceAll(internal, ".", "_")
}

// sortToolsByName uses a tiny insertion sort — N is small (≤ 6 in v0).
func sortToolsByName(ts []Tool) {
	for i := 1; i < len(ts); i++ {
		j := i
		for j > 0 && ts[j].Name() < ts[j-1].Name() {
			ts[j], ts[j-1] = ts[j-1], ts[j]
			j--
		}
	}
}
