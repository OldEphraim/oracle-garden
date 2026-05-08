package engine

import (
	"encoding/json"
	"strings"
)

// conditionMatches implements CLAUDE.md's "Edge condition rules":
//
//   - "always" / "" → always true (used for unconditional fan-out)
//   - "approved"   → output has an `approved` field of value true
//   - "rejected"   → output has an `approved` field of value false
//   - <other>      → substring match against the JSON-stringified output (lowercased)
//
// The approved/rejected paths assume the source agent emits risk_assessment.v1.
// CLAUDE.md says workflow-save in Phase 8 validates that source.output_type =
// risk_assessment.v1 for any edge with these conditions; the engine re-checks
// defensively here — a wrongly-wired edge simply doesn't match (no crash, no
// silent fan-out) so an explicit Phase 8 validation pass is what catches the
// configuration error, not a runtime panic.
func conditionMatches(output json.RawMessage, condition string) bool {
	cond := strings.ToLower(strings.TrimSpace(condition))
	switch cond {
	case "", "always":
		return true
	case "approved":
		return jsonHasField(output, "approved") && jsonBoolField(output, "approved", false)
	case "rejected":
		return jsonHasField(output, "approved") && !jsonBoolField(output, "approved", true)
	default:
		// Substring match against the JSON-stringified output. Brittle for
		// v0; v1 will introduce structured edge conditions ({field, op, value}).
		return strings.Contains(strings.ToLower(string(output)), cond)
	}
}

// jsonHasField reports whether the top-level JSON object has `field` as a key.
// Returns false if output isn't a JSON object.
func jsonHasField(output json.RawMessage, field string) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(output, &m); err != nil {
		return false
	}
	_, ok := m[field]
	return ok
}

// jsonBoolField reads `field` from a JSON object as a bool. Returns the
// default if the field is absent or not a bool. Used by the approved/rejected
// matchers — they call jsonHasField first so the default only matters when
// the field is malformed.
func jsonBoolField(output json.RawMessage, field string, def bool) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(output, &m); err != nil {
		return def
	}
	raw, ok := m[field]
	if !ok {
		return def
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return def
	}
	return b
}

// edgeFiringPlan walks a node's outgoing edges and returns the subset that
// should fire after this firing's output, applying the CLAUDE.md rule:
//
//   - every `always` edge fires (fan-out)
//   - among non-`always` edges, the FIRST match by priority wins; remaining
//     conditional edges from this node are not evaluated for this firing
//
// Edges must be pre-sorted in (priority ASC, id ASC) order. The returned
// slice preserves that order.
func edgeFiringPlan(out []*Edge, output json.RawMessage) []*Edge {
	var fire []*Edge
	matchedConditional := false
	for _, e := range out {
		cond := strings.ToLower(strings.TrimSpace(e.Row.Condition))
		isAlways := cond == "" || cond == "always"
		if isAlways {
			fire = append(fire, e)
			continue
		}
		if matchedConditional {
			continue
		}
		if conditionMatches(output, e.Row.Condition) {
			fire = append(fire, e)
			matchedConditional = true
		}
	}
	return fire
}
