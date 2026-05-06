package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/OldEphraim/sibyl-hub/api/internal/runtime"
)

type stubTool struct {
	name       string
	serverSide bool
}

func (s *stubTool) Name() string       { return s.name }
func (s *stubTool) IsServerSide() bool { return s.serverSide }
func (s *stubTool) Definition() runtime.ToolDefinition {
	return runtime.ToolDefinition{Name: APISafeName(s.name)}
}
func (s *stubTool) Invoke(_ context.Context, _ json.RawMessage) (json.RawMessage, error) {
	if s.serverSide {
		panic("server-side tool's Invoke must not be called")
	}
	return json.RawMessage(`{"ok":true}`), nil
}

func TestAPISafeName(t *testing.T) {
	cases := map[string]string{
		"polymarket.gamma_get_market": "polymarket_gamma_get_market",
		"web_search":                  "web_search",
		"a.b.c":                       "a_b_c",
		"":                            "",
	}
	for in, want := range cases {
		if got := APISafeName(in); got != want {
			t.Errorf("APISafeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegistryRegisterAndLookup(t *testing.T) {
	r := NewRegistry()
	a := &stubTool{name: "polymarket.gamma_get_market"}
	if err := r.Register(a); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got, ok := r.Get(a.Name()); !ok || got != a {
		t.Errorf("Get(internal): %v %v", got, ok)
	}
	if got, ok := r.LookupByAPIName("polymarket_gamma_get_market"); !ok || got != a {
		t.Errorf("LookupByAPIName: %v %v", got, ok)
	}
	if _, ok := r.LookupByAPIName("nope"); ok {
		t.Errorf("LookupByAPIName(\"nope\"): expected false")
	}
}

func TestRegistryDuplicateRejected(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(&stubTool{name: "a.b"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(&stubTool{name: "a.b"}); err == nil {
		t.Errorf("expected duplicate error")
	}
	// Different internal but colliding API name — also rejected.
	if err := r.Register(&stubTool{name: "a_b"}); err == nil || !strings.Contains(err.Error(), "duplicate API name") {
		t.Errorf("expected API-name collision error, got: %v", err)
	}
}

func TestRegistryDefinitionsFor(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubTool{name: "a"})
	r.MustRegister(&stubTool{name: "b"})

	defs, err := r.DefinitionsFor([]string{"a", "b"})
	if err != nil {
		t.Fatalf("DefinitionsFor: %v", err)
	}
	if len(defs) != 2 || defs[0].Name != "a" || defs[1].Name != "b" {
		t.Errorf("unexpected defs: %+v", defs)
	}

	if _, err := r.DefinitionsFor([]string{"a", "missing"}); err == nil {
		t.Errorf("expected error for missing tool")
	}
	if defs, err := r.DefinitionsFor(nil); err != nil || defs != nil {
		t.Errorf("nil input: defs=%v err=%v", defs, err)
	}
}

func TestRegistryAllSorted(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(&stubTool{name: "z.tool"})
	r.MustRegister(&stubTool{name: "a.tool"})
	r.MustRegister(&stubTool{name: "m.tool"})
	all := r.All()
	want := []string{"a.tool", "m.tool", "z.tool"}
	if len(all) != len(want) {
		t.Fatalf("got %d, want %d", len(all), len(want))
	}
	for i, w := range want {
		if all[i].Name() != w {
			t.Errorf("All()[%d] = %q, want %q", i, all[i].Name(), w)
		}
	}
}

func TestPolymarketToolDefinitionsValid(t *testing.T) {
	// Don't actually exercise the polymarket adapter — just confirm each
	// tool's Definition() returns a valid-looking ToolDefinition with an
	// underscore name and a JSON-Schema-shaped input_schema.
	r := NewRegistry()
	RegisterPolymarketTools(r, nil) // nil client is fine if we never Invoke
	wantNames := []string{
		"polymarket.gamma_get_market",
		"polymarket.gamma_search_markets",
		"polymarket.clob_get_orderbook",
		"polymarket.clob_get_midpoint",
		"polymarket.clob_get_prices_history",
	}
	for _, n := range wantNames {
		tool, ok := r.Get(n)
		if !ok {
			t.Errorf("missing tool %q", n)
			continue
		}
		def := tool.Definition()
		if strings.Contains(def.Name, ".") {
			t.Errorf("%s: API name contains dot: %q", n, def.Name)
		}
		if def.Name != APISafeName(n) {
			t.Errorf("%s: API name = %q, want %q", n, def.Name, APISafeName(n))
		}
		var schemaShape map[string]any
		if err := json.Unmarshal(def.InputSchema, &schemaShape); err != nil {
			t.Errorf("%s: input_schema not JSON: %v", n, err)
		}
		if schemaShape["type"] != "object" {
			t.Errorf("%s: input_schema.type = %v, want object", n, schemaShape["type"])
		}
	}
}

func TestWebSearchIsServerSideAndPanicsOnInvoke(t *testing.T) {
	r := NewRegistry()
	RegisterWebSearch(r, 5)
	tool, ok := r.Get("web_search")
	if !ok {
		t.Fatalf("web_search not registered")
	}
	if !tool.IsServerSide() {
		t.Errorf("web_search.IsServerSide() = false, want true")
	}
	def := tool.Definition()
	if def.Type != WebSearchToolType {
		t.Errorf("web_search type = %q, want %q", def.Type, WebSearchToolType)
	}
	if def.MaxUses != 5 {
		t.Errorf("MaxUses = %d, want 5", def.MaxUses)
	}
	defer func() {
		if recover() == nil {
			t.Errorf("expected panic from web_search.Invoke")
		}
	}()
	_, _ = tool.Invoke(context.Background(), nil)
}
