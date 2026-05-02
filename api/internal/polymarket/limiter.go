package polymarket

import (
	"context"
	"time"

	"golang.org/x/time/rate"
)

// Default rate limit per CLAUDE.md "Polymarket API Reference":
// 50 req/min, intentionally below Polymarket's published headline limits
// (~60/min Gamma, ~100/min CLOB) so brief bursts on either endpoint don't
// breach the platform-wide cap.
const (
	defaultRatePerMinute = 50
	defaultRateBurst     = 5
)

// rateLimiter wraps x/time/rate.Limiter with a context-aware Wait.
type rateLimiter struct {
	l *rate.Limiter
}

func newRateLimiter(perMinute int, burst int) *rateLimiter {
	if perMinute <= 0 {
		perMinute = defaultRatePerMinute
	}
	if burst <= 0 {
		burst = defaultRateBurst
	}
	return &rateLimiter{
		l: rate.NewLimiter(rate.Every(time.Minute/time.Duration(perMinute)), burst),
	}
}

// wait blocks until one token is available or ctx is cancelled.
func (r *rateLimiter) wait(ctx context.Context) error {
	return r.l.Wait(ctx)
}
