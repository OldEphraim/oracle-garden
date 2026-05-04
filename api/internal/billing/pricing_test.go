package billing

import (
	"testing"
)

func TestCostUSDKnownModels(t *testing.T) {
	cases := []struct {
		model string
		in    int
		out   int
		want  float64
	}{
		// 1M in @ $3/MTok = $3.00; 1M out @ $15/MTok = $15.00; total $18.00.
		{"claude-sonnet-4-6", 1_000_000, 1_000_000, 18.00},
		// 1M in @ $1/MTok = $1.00; 1M out @ $5/MTok = $5.00; total $6.00.
		{"claude-haiku-4-5-20251001", 1_000_000, 1_000_000, 6.00},
		// 1M in @ $5/MTok = $5.00; 1M out @ $25/MTok = $25.00; total $30.00.
		{"claude-opus-4-7", 1_000_000, 1_000_000, 30.00},
		// Smaller, realistic sample: 5k input + 1k output on sonnet-4-6.
		// Input: 5000 * 3 / 1_000_000 = 0.015. Output: 1000 * 15 / 1_000_000 = 0.015. Sum = 0.030.
		{"claude-sonnet-4-6", 5_000, 1_000, 0.030},
		// Zero tokens → zero cost.
		{"claude-sonnet-4-6", 0, 0, 0.0},
	}
	for _, c := range cases {
		got := CostUSD(c.model, c.in, c.out)
		if !approxEqual(got, c.want) {
			t.Errorf("CostUSD(%q, %d, %d) = %v, want %v", c.model, c.in, c.out, got, c.want)
		}
	}
}

// TestCostUSDUnknownModel verifies the unknown-model behavior: returns 0,
// emits a WARN log (we don't assert the log here — slog default routes to
// stderr; the runtime's tests cover the behavioral contract).
func TestCostUSDUnknownModel(t *testing.T) {
	got := CostUSD("claude-fake-9-9", 1000, 500)
	if got != 0 {
		t.Errorf("unknown model: cost = %v, want 0", got)
	}
}

// TestKnownModelsCoversChosenSet asserts that every model identifier we
// committed to in DECISION_LOG.md Phase 0 is in the table. Acts as a
// regression flag if pricing.go drifts from the chosen identifiers.
func TestKnownModelsCoversChosenSet(t *testing.T) {
	want := map[string]bool{
		"claude-sonnet-4-6":         true,
		"claude-haiku-4-5-20251001": true,
		"claude-opus-4-7":           true,
	}
	for _, m := range KnownModels() {
		delete(want, m)
	}
	if len(want) > 0 {
		t.Errorf("missing models from price table: %v", want)
	}
}

func approxEqual(a, b float64) bool {
	const eps = 1e-9
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}
