// Package billing owns the model → ($/MTok in, $/MTok out) static price
// table and per-step cost computation. Loose coupling with the runtime: the
// runtime passes (model, prompt_tokens, completion_tokens) and gets back a
// USD float. Persistence (recording cost on agent_steps + incrementing
// user_usage_daily) is the runs-repo's job; this package is pure arithmetic.
//
// Pricing constants are the model identifiers chosen in Phase 0 (see
// DECISION_LOG.md). When Anthropic's headline pricing changes, update the
// map here AND the rationale in DECISION_LOG.md. v1+ will fetch pricing
// dynamically from /v1/models with a 24h cache (see "Deferred to v1+").
package billing

import (
	"log/slog"
)

// modelPrice carries USD-per-million-tokens for input and output.
type modelPrice struct {
	InputUSDPerMTok  float64
	OutputUSDPerMTok float64
}

// prices is the v0 static table. Key is the canonical Claude API ID.
var prices = map[string]modelPrice{
	"claude-sonnet-4-6":         {InputUSDPerMTok: 3.00, OutputUSDPerMTok: 15.00},
	"claude-haiku-4-5-20251001": {InputUSDPerMTok: 1.00, OutputUSDPerMTok: 5.00},
	"claude-opus-4-7":           {InputUSDPerMTok: 5.00, OutputUSDPerMTok: 25.00},
}

// CostUSD computes the dollar cost of one Anthropic API call.
//
// On unknown model: logs WARN and returns 0. *Why 0 instead of fail-fast:*
// a typo in an agent_template's `model` column shouldn't take down a whole
// run — the run still completes, but the cost shows as $0 (under-reporting,
// the conservative direction; users will see actual usage on the Anthropic
// console). The WARN flags the typo for follow-up. Fail-fast on unknown
// models is the wrong tradeoff because an Anthropic-released-but-not-yet-
// in-our-table model has the same shape, and we'd rather under-bill than
// kill the run.
func CostUSD(model string, promptTokens, completionTokens int) float64 {
	p, ok := prices[model]
	if !ok {
		slog.Warn(
			"billing: unknown model — returning 0 cost (update pricing.go)",
			"model", model,
			"prompt_tokens", promptTokens,
			"completion_tokens", completionTokens,
		)
		return 0
	}
	in := float64(promptTokens) * p.InputUSDPerMTok / 1_000_000
	out := float64(completionTokens) * p.OutputUSDPerMTok / 1_000_000
	return in + out
}

// KnownModels returns the slice of model IDs the price table knows about.
// Useful for the agent-builder UI's model dropdown (Phase 11) and for tests
// that want to assert "every chosen model is in the price table".
func KnownModels() []string {
	out := make([]string, 0, len(prices))
	for m := range prices {
		out = append(out, m)
	}
	return out
}
