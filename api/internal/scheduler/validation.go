// Package scheduler is the cron-driven workflow firing layer. validation.go
// owns the cron-expression validator that workflow-save endpoints (Phase 8)
// call before persisting `schedule_cron`. scheduler.go is the in-process
// loop that wraps robfig/cron and drives the engine on each tick.
package scheduler

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/robfig/cron/v3"
)

// CLAUDE.md "Cost Protection" mechanism #3: minimum schedule interval.
const (
	DefaultMinScheduleIntervalSeconds = 300 // 5 minutes
	envMinScheduleIntervalSeconds     = "MIN_SCHEDULE_INTERVAL_SECONDS"
)

// MinScheduleInterval reads the env-configured floor with the documented
// default. Bad values fall back to the default — same forgiving rule as
// the billing caps. v1+ should harden this with config-validation step.
func MinScheduleInterval() time.Duration {
	s := os.Getenv(envMinScheduleIntervalSeconds)
	if s == "" {
		return time.Duration(DefaultMinScheduleIntervalSeconds) * time.Second
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return time.Duration(DefaultMinScheduleIntervalSeconds) * time.Second
	}
	return time.Duration(n) * time.Second
}

// ValidateCronInterval rejects schedule_cron expressions that fire faster
// than MIN_SCHEDULE_INTERVAL_SECONDS. CLAUDE.md flags this as
// non-negotiable: regex-pattern-matching the cron string is wrong (the
// spec gives `*/2 * * * *` as the canonical bypass — it evaluates to 2
// minutes, which any "must contain only stars and large numbers" regex
// misses). The only correct way is to actually parse the expression and
// compute Next() twice.
//
// Uses cron.ParseStandard (5-field standard cron). robfig/cron supports a
// non-standard 6-field variant with seconds, but standard cron is what
// users understand and what we want to enforce against.
//
// Returns nil on ok; a wrapped error otherwise. Callers (handlers, the
// scheduler's reload path) surface the error verbatim.
func ValidateCronInterval(expr string) error {
	return validateCronWithMin(expr, MinScheduleInterval())
}

// validateCronWithMin is the testable inner function — tests inject a small
// minimum so they can verify both the accept and reject paths without
// depending on env vars.
func validateCronWithMin(expr string, minInterval time.Duration) error {
	expr = trimSpace(expr)
	if expr == "" {
		return fmt.Errorf("scheduler: cron expression is empty")
	}

	schedule, err := cron.ParseStandard(expr)
	if err != nil {
		return fmt.Errorf("scheduler: parse cron %q: %w", expr, err)
	}

	// Compute the actual fire interval by sampling Next() twice from the
	// SAME reference time. Using two distinct calls (the second taking
	// the first's result as input) is what catches `*/2 * * * *`'s 2-min
	// cadence — a regex on the literal string can't.
	now := time.Now()
	first := schedule.Next(now)
	second := schedule.Next(first)
	if first.IsZero() || second.IsZero() {
		return fmt.Errorf("scheduler: cron %q produces no future fires", expr)
	}
	gap := second.Sub(first)
	if gap < minInterval {
		return fmt.Errorf(
			"scheduler: cron %q fires every %s, below minimum %s (CLAUDE.md cost protection mechanism #3)",
			expr, gap.Truncate(time.Second), minInterval,
		)
	}
	return nil
}

// trimSpace is a tiny helper so we don't pull strings.TrimSpace into a
// validation file that's otherwise dependency-light.
func trimSpace(s string) string {
	for len(s) > 0 && isSpace(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && isSpace(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	return s
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
