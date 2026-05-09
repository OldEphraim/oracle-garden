package billing

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
)

// IncrementRunCount bumps user_usage_daily.run_count by 1 and returns the
// post-increment count. Atomic upsert per CLAUDE.md "Cost protection"
// mechanism #1:
//
//	INSERT INTO user_usage_daily (user_id, day, run_count)
//	VALUES ($1, CURRENT_DATE, 1)
//	ON CONFLICT (user_id, day) DO UPDATE
//	  SET run_count = user_usage_daily.run_count + 1
//	RETURNING run_count
//
// Caller compares the returned count against q.MaxRunsPerDay (or
// q.IsOverRunCount(n)). Two concurrent runs by the same user can't both
// pass a pre-check and overshoot — the second INSERT serializes against the
// first via the unique (user_id, day) index. Over-counting in
// quota_exceeded cases is acceptable (the conservative direction) — we
// never decrement.
//
// Thin wrapper around UsageRepo.IncrementRun; lives here so callers don't
// need to know about the repo layer.
func (q *Quotas) IncrementRunCount(ctx context.Context, dbq db.Querier, userID uuid.UUID) (int, error) {
	n, err := q.repo.IncrementRun(ctx, dbq, userID)
	if err != nil {
		return 0, fmt.Errorf("billing: IncrementRunCount: %w", err)
	}
	return n, nil
}

// RecordStepCost atomically increments user_usage_daily.total_tokens and
// total_cost_usd, returning the post-increment total_cost_usd. CLAUDE.md
// mechanism #10:
//
//	INSERT INTO user_usage_daily (user_id, day, run_count, total_tokens, total_cost_usd)
//	VALUES ($1, CURRENT_DATE, 0, $2, $3)
//	ON CONFLICT (user_id, day) DO UPDATE
//	  SET total_tokens   = user_usage_daily.total_tokens   + EXCLUDED.total_tokens,
//	      total_cost_usd = user_usage_daily.total_cost_usd + EXCLUDED.total_cost_usd
//	RETURNING total_cost_usd
//
// Note the INSERT sets run_count = 0 — this row may be created by the first
// cost-recording call before any IncrementRunCount has fired (e.g.
// admin-triggered runs that bypass the run-count gate). The ON CONFLICT
// branch updates only token/cost columns, leaving run_count alone.
//
// Caller compares the returned total against q.MaxCostUSDPerDay (or
// q.IsOverCostUSD(total)). Already-paid-for tokens don't roll back; the
// conservative behavior is to mark the run quota_exceeded so subsequent
// steps don't fire.
func (q *Quotas) RecordStepCost(
	ctx context.Context,
	dbq db.Querier,
	userID uuid.UUID,
	costUSD float64,
	totalTokens int64,
) (float64, error) {
	total, err := q.repo.IncrementCost(ctx, dbq, userID, totalTokens, costUSD)
	if err != nil {
		return 0, fmt.Errorf("billing: RecordStepCost: %w", err)
	}
	return total, nil
}
