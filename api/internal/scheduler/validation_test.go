package scheduler

import (
	"strings"
	"testing"
	"time"
)

// TestValidateCronInterval covers CLAUDE.md's non-negotiable rule:
// regex-pattern-matching the cron string is wrong. The only correct
// implementation parses + computes Next() twice. The */2 * * * * case is
// the canonical regression flag — if a future "optimization" tries to
// short-circuit via a regex check, this test will fire.
func TestValidateCronInterval(t *testing.T) {
	const min = 5 * time.Minute

	cases := []struct {
		name      string
		expr      string
		wantError bool
		errIncludes string
	}{
		{name: "hourly_ok",   expr: "0 * * * *",  wantError: false},
		{name: "every_5min",  expr: "*/5 * * * *", wantError: false},
		{name: "9am_daily",   expr: "0 9 * * *",   wantError: false},
		{name: "every_minute", expr: "* * * * *",   wantError: true, errIncludes: "below minimum"},
		// The regex-bypass gotcha — `*/2 * * * *` evaluates to 2 minutes,
		// which a "must contain only stars and big numbers" regex misses.
		{name: "every_2min_regression", expr: "*/2 * * * *", wantError: true, errIncludes: "below minimum"},
		// Standard cron 5-field; the parser explicitly rejects 6-field
		// (with-seconds) input, which is what we want.
		{name: "with_seconds_rejected", expr: "*/30 * * * * *", wantError: true},
		{name: "garbage", expr: "garbage", wantError: true, errIncludes: "parse cron"},
		{name: "empty", expr: "", wantError: true, errIncludes: "empty"},
		{name: "whitespace_only", expr: "   ", wantError: true, errIncludes: "empty"},
		{name: "trimmed", expr: "  0 * * * *  ", wantError: false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := validateCronWithMin(c.expr, min)
			if c.wantError {
				if err == nil {
					t.Errorf("expected error for %q, got nil", c.expr)
					return
				}
				if c.errIncludes != "" && !strings.Contains(err.Error(), c.errIncludes) {
					t.Errorf("error for %q should include %q, got: %v", c.expr, c.errIncludes, err)
				}
			} else {
				if err != nil {
					t.Errorf("expected ok for %q, got error: %v", c.expr, err)
				}
			}
		})
	}
}

// TestMinScheduleIntervalEnvOverride — the env override is read; bad values
// fall back to the default.
func TestMinScheduleIntervalEnvOverride(t *testing.T) {
	cases := []struct {
		env  string
		want time.Duration
	}{
		{"", time.Duration(DefaultMinScheduleIntervalSeconds) * time.Second},
		{"600", 600 * time.Second},
		{"garbage", time.Duration(DefaultMinScheduleIntervalSeconds) * time.Second},
		{"-5", time.Duration(DefaultMinScheduleIntervalSeconds) * time.Second},
	}
	for _, c := range cases {
		t.Run(c.env, func(t *testing.T) {
			t.Setenv(envMinScheduleIntervalSeconds, c.env)
			if got := MinScheduleInterval(); got != c.want {
				t.Errorf("env=%q: got %s want %s", c.env, got, c.want)
			}
		})
	}
}
