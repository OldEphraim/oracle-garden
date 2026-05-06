package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/OldEphraim/sibyl-hub/api/internal/polymarket"
	"github.com/OldEphraim/sibyl-hub/api/internal/runtime"
)

// RegisterPolymarketTools registers all five v0 polymarket.* tools against
// the given Polymarket adapter. Call once at startup.
func RegisterPolymarketTools(r *Registry, c *polymarket.Client) {
	r.MustRegister(&gammaGetMarket{c: c})
	r.MustRegister(&gammaSearchMarkets{c: c})
	r.MustRegister(&clobGetOrderbook{c: c})
	r.MustRegister(&clobGetMidpoint{c: c})
	r.MustRegister(&clobGetPricesHistory{c: c})
}

// --- polymarket.gamma_get_market -------------------------------------------

type gammaGetMarket struct{ c *polymarket.Client }

func (t *gammaGetMarket) Name() string       { return "polymarket.gamma_get_market" }
func (t *gammaGetMarket) IsServerSide() bool { return false }
func (t *gammaGetMarket) Definition() runtime.ToolDefinition {
	return runtime.ToolDefinition{
		Name:        APISafeName(t.Name()),
		Description: "Resolve a Polymarket market by its slug. Returns metadata, conditionId, and clobTokenIds (use these against CLOB endpoints).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"slug": {"type": "string", "description": "The market slug, e.g. \"will-bitcoin-hit-150k-by-june-30-2026\""}
			},
			"required": ["slug"],
			"additionalProperties": false
		}`),
	}
}
func (t *gammaGetMarket) Invoke(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var args struct{ Slug string }
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("polymarket.gamma_get_market: input: %w", err)
	}
	m, err := t.c.GetMarket(ctx, args.Slug)
	if err != nil {
		return nil, err
	}
	return marshalCompact(m)
}

// --- polymarket.gamma_search_markets ---------------------------------------

type gammaSearchMarkets struct{ c *polymarket.Client }

func (t *gammaSearchMarkets) Name() string       { return "polymarket.gamma_search_markets" }
func (t *gammaSearchMarkets) IsServerSide() bool { return false }
func (t *gammaSearchMarkets) Definition() runtime.ToolDefinition {
	return runtime.ToolDefinition{
		Name:        APISafeName(t.Name()),
		Description: "Free-text search for active Polymarket markets. Returns up to `limit` markets (default 20). Backed by /public-search.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Free-text query, e.g. \"bitcoin\", \"trump\""},
				"limit": {"type": "integer", "minimum": 1, "maximum": 50, "description": "Maximum number of markets to return (default 20)"}
			},
			"required": ["query"],
			"additionalProperties": false
		}`),
	}
}
func (t *gammaSearchMarkets) Invoke(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var args struct {
		Query string
		Limit int
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("polymarket.gamma_search_markets: input: %w", err)
	}
	out, err := t.c.SearchMarkets(ctx, args.Query, args.Limit)
	if err != nil {
		return nil, err
	}
	return marshalCompact(out)
}

// --- polymarket.clob_get_orderbook -----------------------------------------

type clobGetOrderbook struct{ c *polymarket.Client }

func (t *clobGetOrderbook) Name() string       { return "polymarket.clob_get_orderbook" }
func (t *clobGetOrderbook) IsServerSide() bool { return false }
func (t *clobGetOrderbook) Definition() runtime.ToolDefinition {
	return runtime.ToolDefinition{
		Name:        APISafeName(t.Name()),
		Description: "Fetch the full L2 orderbook for one CLOB token_id. The token_id MUST be one of the values in Market.clobTokenIds (typically two per market — one per outcome). Passing a conditionId returns garbage.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"token_id": {"type": "string", "description": "Polymarket CLOB token_id (very long numeric string from Market.clobTokenIds)"}
			},
			"required": ["token_id"],
			"additionalProperties": false
		}`),
	}
}
func (t *clobGetOrderbook) Invoke(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var args struct {
		TokenID string `json:"token_id"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("polymarket.clob_get_orderbook: input: %w", err)
	}
	out, err := t.c.GetOrderbook(ctx, args.TokenID)
	if err != nil {
		return nil, err
	}
	return marshalCompact(out)
}

// --- polymarket.clob_get_midpoint ------------------------------------------

type clobGetMidpoint struct{ c *polymarket.Client }

func (t *clobGetMidpoint) Name() string       { return "polymarket.clob_get_midpoint" }
func (t *clobGetMidpoint) IsServerSide() bool { return false }
func (t *clobGetMidpoint) Definition() runtime.ToolDefinition {
	return runtime.ToolDefinition{
		Name:        APISafeName(t.Name()),
		Description: "Fetch the orderbook midpoint for one CLOB token_id. Returns a number in [0, 1].",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"token_id": {"type": "string", "description": "Polymarket CLOB token_id (NOT the conditionId)"}
			},
			"required": ["token_id"],
			"additionalProperties": false
		}`),
	}
}
func (t *clobGetMidpoint) Invoke(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var args struct {
		TokenID string `json:"token_id"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("polymarket.clob_get_midpoint: input: %w", err)
	}
	mid, err := t.c.GetMidpoint(ctx, args.TokenID)
	if err != nil {
		return nil, err
	}
	return marshalCompact(map[string]float64{"mid": mid})
}

// --- polymarket.clob_get_prices_history ------------------------------------

type clobGetPricesHistory struct{ c *polymarket.Client }

func (t *clobGetPricesHistory) Name() string       { return "polymarket.clob_get_prices_history" }
func (t *clobGetPricesHistory) IsServerSide() bool { return false }
func (t *clobGetPricesHistory) Definition() runtime.ToolDefinition {
	return runtime.ToolDefinition{
		Name: APISafeName(t.Name()),
		Description: "Fetch historical mid prices for a CLOB token_id. Note: Polymarket's underlying CLOB endpoint names the param `market` but it expects a token_id (NOT a conditionId).",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"token_id": {"type": "string", "description": "Polymarket CLOB token_id (NOT the conditionId)"},
				"interval": {"type": "string", "enum": ["1h","6h","1d","1w","1m","max"], "description": "Time-window the bucketing covers"},
				"fidelity": {"type": "integer", "minimum": 1, "description": "Bucket size in minutes (passed through to CLOB verbatim)"}
			},
			"required": ["token_id"],
			"additionalProperties": false
		}`),
	}
}
func (t *clobGetPricesHistory) Invoke(ctx context.Context, input json.RawMessage) (json.RawMessage, error) {
	var args struct {
		TokenID  string `json:"token_id"`
		Interval string `json:"interval"`
		Fidelity int    `json:"fidelity"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return nil, fmt.Errorf("polymarket.clob_get_prices_history: input: %w", err)
	}
	out, err := t.c.GetPriceHistory(ctx, args.TokenID, args.Interval, args.Fidelity)
	if err != nil {
		return nil, err
	}
	return marshalCompact(out)
}

// marshalCompact emits compact JSON. Keeps tool_result payloads small —
// Anthropic counts every byte against the model's context window.
func marshalCompact(v any) (json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}
