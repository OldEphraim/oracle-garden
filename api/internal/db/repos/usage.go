package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
)

// UserUsageDaily mirrors user_usage_daily. Day is a DATE column at midnight UTC.
type UserUsageDaily struct {
	UserID       uuid.UUID
	Day          time.Time
	RunCount     int
	TotalTokens  int64
	TotalCostUSD float64
}

// UsageRepo owns user_usage_daily. Its two write methods are the heart of
// CLAUDE.md's cost-protection mechanism — both use atomic upserts that
// return post-increment values so concurrent runs by the same user can't
// both pass a pre-check and overshoot the cap.
type UsageRepo struct{}

func NewUsageRepo() *UsageRepo { return &UsageRepo{} }

// IncrementRun bumps run_count by 1 and returns the post-increment count.
// Caller compares against MAX_RUNS_PER_USER_PER_DAY and short-circuits the
// run as 'quota_exceeded' if exceeded. Already-overshoot increments are
// fine (CLAUDE.md: "over-counting in `quota_exceeded` cases is acceptable").
func (r *UsageRepo) IncrementRun(ctx context.Context, q db.Querier, userID uuid.UUID) (int, error) {
	const sql = `
		INSERT INTO user_usage_daily (user_id, day, run_count)
		VALUES ($1, CURRENT_DATE, 1)
		ON CONFLICT (user_id, day) DO UPDATE
		  SET run_count = user_usage_daily.run_count + 1
		RETURNING run_count
	`
	var n int
	if err := q.QueryRow(ctx, sql, userID).Scan(&n); err != nil {
		return 0, fmt.Errorf("user_usage_daily: IncrementRun: %w", err)
	}
	return n, nil
}

// IncrementCost bumps total_tokens and total_cost_usd; returns post-increment
// total_cost_usd so the caller can compare against MAX_COST_USD_PER_USER_PER_DAY
// and mark the run 'quota_exceeded' for subsequent steps.
//
// Per CLAUDE.md mechanism #10: tokens already paid for don't roll back, so
// going over the cap is the conservative direction.
func (r *UsageRepo) IncrementCost(ctx context.Context, q db.Querier, userID uuid.UUID, tokens int64, costUSD float64) (float64, error) {
	const sql = `
		INSERT INTO user_usage_daily (user_id, day, run_count, total_tokens, total_cost_usd)
		VALUES ($1, CURRENT_DATE, 0, $2, $3)
		ON CONFLICT (user_id, day) DO UPDATE
		  SET total_tokens   = user_usage_daily.total_tokens   + EXCLUDED.total_tokens,
		      total_cost_usd = user_usage_daily.total_cost_usd + EXCLUDED.total_cost_usd
		RETURNING total_cost_usd
	`
	var total float64
	if err := q.QueryRow(ctx, sql, userID, tokens, costUSD).Scan(&total); err != nil {
		return 0, fmt.Errorf("user_usage_daily: IncrementCost: %w", err)
	}
	return total, nil
}

// GetTodayUsage returns the row for (userID, CURRENT_DATE), or zero values
// if no row exists. Never returns ErrNotFound — "user has no usage today"
// is a valid state, not an error.
func (r *UsageRepo) GetTodayUsage(ctx context.Context, q db.Querier, userID uuid.UUID) (*UserUsageDaily, error) {
	const sql = `
		SELECT user_id, day, run_count, total_tokens, total_cost_usd
		FROM user_usage_daily
		WHERE user_id = $1 AND day = CURRENT_DATE
	`
	var u UserUsageDaily
	err := q.QueryRow(ctx, sql, userID).
		Scan(&u.UserID, &u.Day, &u.RunCount, &u.TotalTokens, &u.TotalCostUSD)
	if err == nil {
		return &u, nil
	}
	if isPgxNoRows(err) {
		return &UserUsageDaily{UserID: userID, Day: today()}, nil
	}
	return nil, fmt.Errorf("user_usage_daily: GetTodayUsage: %w", err)
}

// GetUsageHistory returns the user's usage rows for the last N days,
// newest first. Used for the /api/me/usage endpoint and the dashboard chart.
func (r *UsageRepo) GetUsageHistory(ctx context.Context, q db.Querier, userID uuid.UUID, days int) ([]*UserUsageDaily, error) {
	if days <= 0 {
		days = 30
	}
	const sql = `
		SELECT user_id, day, run_count, total_tokens, total_cost_usd
		FROM user_usage_daily
		WHERE user_id = $1 AND day >= CURRENT_DATE - $2::int
		ORDER BY day DESC
	`
	rows, err := q.Query(ctx, sql, userID, days)
	if err != nil {
		return nil, fmt.Errorf("user_usage_daily: GetUsageHistory: %w", err)
	}
	defer rows.Close()
	var out []*UserUsageDaily
	for rows.Next() {
		var u UserUsageDaily
		if err := rows.Scan(&u.UserID, &u.Day, &u.RunCount, &u.TotalTokens, &u.TotalCostUSD); err != nil {
			return nil, fmt.Errorf("user_usage_daily: GetUsageHistory: scan: %w", err)
		}
		out = append(out, &u)
	}
	return out, rows.Err()
}

// isPgxNoRows is a tiny helper so the GetTodayUsage zero-value path doesn't
// have to import pgx itself (and so this file doesn't pick up the import
// for a single check).
func isPgxNoRows(err error) bool {
	return err != nil && err.Error() == "no rows in result set"
}

func today() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}
