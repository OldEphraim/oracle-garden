package engine

import (
	"context"
	"time"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
	"github.com/OldEphraim/sibyl-hub/api/internal/db/repos"
)

// CLAUDE.md "Cost Protection" — non-negotiable engine caps:
//
//	5. Per-step timeout                 90 seconds
//	6. Per-run total step limit         50
//	7. Per-run timeout                  10 minutes
//
// Tests inject smaller values via Options to keep the test matrix fast.
// Production runs use these defaults.
const (
	DefaultPerStepTimeout = 90 * time.Second
	DefaultPerRunTimeout  = 10 * time.Minute
	DefaultMaxStepsPerRun = 50
)

// killSwitchChecker reads the kill_switch system_config row. The executor
// queries it at run start AND between steps; mid-step flips don't
// terminate an in-flight Anthropic call (the per-step timeout governs that).
type killSwitchChecker struct {
	q    db.Querier
	repo *repos.SystemConfigRepo
}

func newKillSwitchChecker(q db.Querier, repo *repos.SystemConfigRepo) *killSwitchChecker {
	return &killSwitchChecker{q: q, repo: repo}
}

func (k *killSwitchChecker) on(ctx context.Context) (bool, error) {
	if k == nil || k.repo == nil {
		return false, nil
	}
	return k.repo.GetKillSwitch(ctx, k.q)
}

// stepContext returns a context that cancels after Options.PerStepTimeout
// (or the default). The cancel func MUST be deferred by the caller — pgx
// connections leak otherwise.
func stepContext(parent context.Context, perStepTimeout time.Duration) (context.Context, context.CancelFunc) {
	if perStepTimeout <= 0 {
		perStepTimeout = DefaultPerStepTimeout
	}
	return context.WithTimeout(parent, perStepTimeout)
}

// runDeadline returns the absolute deadline for a run started at start with
// the given per-run timeout (or default).
func runDeadline(start time.Time, perRunTimeout time.Duration) time.Time {
	if perRunTimeout <= 0 {
		perRunTimeout = DefaultPerRunTimeout
	}
	return start.Add(perRunTimeout)
}
