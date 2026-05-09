package billing

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/google/uuid"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
	"github.com/OldEphraim/sibyl-hub/api/internal/db/repos"
)

// CLAUDE.md "Cost Protection" — env-configured per-user-per-day caps:
//
//	MAX_RUNS_PER_USER_PER_DAY     default 50
//	MAX_COST_USD_PER_USER_PER_DAY default 5.00
//
// The platform reads them once at startup via NewQuotas(); changes require
// a process restart (admin endpoints to flip caps live are a v1+ concern).
const (
	DefaultMaxRunsPerDay     = 50
	DefaultMaxCostUSDPerDay  = 5.00
	envMaxRunsPerDay         = "MAX_RUNS_PER_USER_PER_DAY"
	envMaxCostUSDPerDay      = "MAX_COST_USD_PER_USER_PER_DAY"
)

// Quotas wraps the env-configured caps and the UsageRepo, exposing the
// guard-and-record API the engine, scheduler, and run-create handlers use.
//
// All methods take a db.Querier so callers can pass a tx (for tests / future
// transactional refactors) or the pool (production v0). The atomic-upsert
// pattern in IncrementRunCount / RecordStepCost is what guarantees two
// concurrent runs by the same user can't both pass a pre-check and overshoot
// the cap — the second INSERT serializes against the first via the unique
// (user_id, day) index on user_usage_daily.
type Quotas struct {
	repo            *repos.UsageRepo
	MaxRunsPerDay   int
	MaxCostUSDPerDay float64
}

// NewQuotas reads env vars (with defaults) and returns a populated *Quotas.
// Bad values fall back to defaults rather than failing — startup shouldn't
// hard-fail on a typo'd .env, but this should change in v1+ once we have a
// proper config validation step.
func NewQuotas(repo *repos.UsageRepo) *Quotas {
	return &Quotas{
		repo:             repo,
		MaxRunsPerDay:    intFromEnv(envMaxRunsPerDay, DefaultMaxRunsPerDay),
		MaxCostUSDPerDay: floatFromEnv(envMaxCostUSDPerDay, DefaultMaxCostUSDPerDay),
	}
}

// CheckQuota is the read-only pre-flight check at run start. Returns:
//
//	(true,  "", nil)                      — user has remaining quota
//	(false, "<human-readable reason>", nil) — user is over a cap
//	(false, "", err)                      — DB read failed
//
// Race condition with two concurrent runs: both might pass this check
// simultaneously. The atomic upsert in IncrementRunCount is what ultimately
// serializes them — CheckQuota is the cheap pre-flight, IncrementRunCount
// is the authoritative gate.
//
// Logs no output — the caller decides whether to surface the reason to the
// user (handler returns 429) or to the operator (scheduler logs and skips).
func (q *Quotas) CheckQuota(ctx context.Context, dbq db.Querier, userID uuid.UUID) (ok bool, reason string, err error) {
	usage, err := q.repo.GetTodayUsage(ctx, dbq, userID)
	if err != nil {
		return false, "", fmt.Errorf("billing: CheckQuota: %w", err)
	}
	if usage.RunCount >= q.MaxRunsPerDay {
		return false, fmt.Sprintf("daily run quota exceeded (%d)", q.MaxRunsPerDay), nil
	}
	if usage.TotalCostUSD >= q.MaxCostUSDPerDay {
		return false, fmt.Sprintf("daily cost cap exceeded ($%.2f)", q.MaxCostUSDPerDay), nil
	}
	return true, "", nil
}

// IsOverRunCount reports whether a post-increment run_count exceeds the cap.
// Pulled out so callers can compare without re-reading caps from this struct.
func (q *Quotas) IsOverRunCount(n int) bool { return n > q.MaxRunsPerDay }

// IsOverCostUSD reports whether a post-increment total_cost_usd exceeds the cap.
func (q *Quotas) IsOverCostUSD(cost float64) bool { return cost > q.MaxCostUSDPerDay }

// intFromEnv parses an int from env with a default fallback. Empty / invalid
// → default; the only way to surface a config typo is the run not behaving
// as expected on inspection. v1+ should replace this with a proper config
// validation step (config.MustLoad()).
func intFromEnv(key string, def int) int {
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func floatFromEnv(key string, def float64) float64 {
	s := os.Getenv(key)
	if s == "" {
		return def
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return def
	}
	return f
}
