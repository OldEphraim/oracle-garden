package engine

import (
	"encoding/json"
	"fmt"
)

// mergeInputsForNode builds the map[upstreamNodeKey → outputPayload] that
// the runtime's Invoke takes. Only includes upstream entries that have at
// least one output recorded — keys for upstreams that haven't produced
// anything yet (legitimate during loop firings) are simply absent.
//
// The runtime's buildSystemPrompt adds a "keys present" preamble when the
// returned map has more than one entry, so this is the single point that
// drives multi-input prompting.
func mergeInputsForNode(n *Node) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(n.LatestUpstreamOutputs))
	for k, v := range n.LatestUpstreamOutputs {
		out[k] = v.Payload
	}
	return out
}

// seedEntryInputs converts a market_target.v1 payload (the engine's
// initial seed for entry nodes) into the merged-inputs map that the
// runtime expects. Spreading the market_target fields directly (rather
// than wrapping them under a synthetic key) keeps the agent prompt
// natural — Market Watcher reads `market_slug` at the top level.
//
// For entry nodes with multiple seeded fields (currently market_slug and
// optionally user_intent per market_target.v1), the runtime's preamble
// will list those field names — slightly off from the upstream-node-key
// convention but consistent with "the input shape you've been given."
// Documented in DECISION_LOG.md Phase 6.
func seedEntryInputs(marketSlug, userIntent string) (map[string]json.RawMessage, error) {
	target := map[string]any{"market_slug": marketSlug}
	if userIntent != "" {
		target["user_intent"] = userIntent
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return nil, fmt.Errorf("engine: seedEntryInputs: marshal: %w", err)
	}

	// Round-trip through json.RawMessage to preserve number formatting and
	// produce per-key RawMessage values.
	var into map[string]json.RawMessage
	if err := json.Unmarshal(raw, &into); err != nil {
		return nil, fmt.Errorf("engine: seedEntryInputs: unmarshal: %w", err)
	}
	return into, nil
}

// inputSnapshot is the JSONB blob the engine writes to workflow_runs.input_snapshot
// at the start of a run. v0 stores the seeded market_target; v1+ may include
// additional context (price snapshot, news preview).
func inputSnapshot(marketSlug, userIntent string) (json.RawMessage, error) {
	target := map[string]any{"market_slug": marketSlug}
	if userIntent != "" {
		target["user_intent"] = userIntent
	}
	return json.Marshal(target)
}
