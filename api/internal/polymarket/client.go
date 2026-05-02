package polymarket

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"golang.org/x/time/rate"
)

const (
	gammaBaseURL = "https://gamma-api.polymarket.com"
	clobBaseURL  = "https://clob.polymarket.com"

	defaultTimeout   = 15 * time.Second
	defaultRetries   = 4 // worst case ~ 250 + 500 + 1000 + 2000 ms = 3.75s before giving up
	baseBackoff      = 250 * time.Millisecond
	maxBackoff       = 8 * time.Second
	defaultUserAgent = "sibyl-hub/0.1 (+https://github.com/OldEphraim/sibyl-hub)"
)

// ClientOptions configures NewClient. Zero values fall back to sensible
// defaults (see field comments). All fields optional.
type ClientOptions struct {
	HTTPClient   *http.Client
	GammaBaseURL string        // default: https://gamma-api.polymarket.com
	CLOBBaseURL  string        // default: https://clob.polymarket.com
	CacheSize    int           // default: 1024 entries (single shared LRU)
	RatePerMin   int           // default: 50
	RateBurst    int           // default: 5
	Limiter      *rate.Limiter // pre-configured limiter, overrides RatePerMin/RateBurst
	Logger       *slog.Logger  // default: slog.Default()
	UserAgent    string        // default: "sibyl-hub/0.1 (+https://...)" — sent on every request
	Now          func() time.Time
}

// Client is the HTTP-and-cache layer for Polymarket. Safe for concurrent use.
type Client struct {
	httpClient   *http.Client
	gammaBaseURL string
	clobBaseURL  string
	cache        *ttlCache
	limiter      *rateLimiter
	logger       *slog.Logger
	userAgent    string
	now          func() time.Time
}

// NewClient builds a Client with defaults filled in for any zero options.
func NewClient(opts ClientOptions) *Client {
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	gamma := opts.GammaBaseURL
	if gamma == "" {
		gamma = gammaBaseURL
	}
	clob := opts.CLOBBaseURL
	if clob == "" {
		clob = clobBaseURL
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = defaultUserAgent
	}

	c := &Client{
		httpClient:   hc,
		gammaBaseURL: gamma,
		clobBaseURL:  clob,
		cache:        newTTLCache(opts.CacheSize),
		logger:       logger,
		userAgent:    ua,
		now:          now,
	}
	c.cache.now = now

	if opts.Limiter != nil {
		c.limiter = &rateLimiter{l: opts.Limiter}
	} else {
		c.limiter = newRateLimiter(opts.RatePerMin, opts.RateBurst)
	}
	return c
}

// errHTTPStatus carries the HTTP status code so callers can branch on
// "not found" etc. without parsing strings.
type errHTTPStatus struct {
	Status int
	Body   string
}

func (e *errHTTPStatus) Error() string {
	return fmt.Sprintf("polymarket: HTTP %d: %s", e.Status, e.Body)
}

// IsNotFound reports whether err originated from a 404 response.
func IsNotFound(err error) bool {
	var e *errHTTPStatus
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// buildURL joins base + path, then appends query params from values. Values
// is optional (may be nil). Returned URL is the full request URL string.
func buildURL(base, path string, values url.Values) string {
	u := base + path
	if len(values) == 0 {
		return u
	}
	return u + "?" + values.Encode()
}

// allowedMarketsParams is the allowlist of query keys Gamma /markets actually
// honors. The endpoint silently ignores unknown keys (returns the default
// list as if no filter were applied), which produces hard-to-debug bugs when
// future code passes e.g. ?question_contains=... and the caller assumes a
// filter ran. Keys here are observed to filter; expand only after verifying
// against a live endpoint.
var allowedMarketsParams = map[string]struct{}{
	"slug":              {},
	"id":                {},
	"limit":             {},
	"offset":            {},
	"active":            {},
	"closed":            {},
	"archived":          {},
	"order":             {},
	"ascending":         {},
	"tag_id":            {},
	"tag_slug":          {},
	"liquidity_num_min": {},
	"volume_num_min":    {},
}

// gammaMarketsURL builds a Gamma /markets URL after filtering values against
// allowedMarketsParams. Unknown keys are dropped with a WARN log — they are
// NOT a hard error because the endpoint itself doesn't reject them, and a
// strict guardrail would block ad-hoc dev usage. The drop+warn behavior makes
// silent-ignore visible without breaking calls.
//
// All Sibyl Hub code that targets Gamma /markets MUST go through this helper.
// /public-search, /events, and CLOB endpoints have their own param shapes and
// are not subject to this allowlist.
func (c *Client) gammaMarketsURL(values url.Values) string {
	if len(values) == 0 {
		return c.gammaBaseURL + "/markets"
	}
	clean := url.Values{}
	for k, v := range values {
		if _, ok := allowedMarketsParams[k]; !ok {
			c.logger.Warn(
				"polymarket: dropping unrecognized /markets query param — silently ignored by API",
				"param", k,
				"values", v,
			)
			continue
		}
		clean[k] = v
	}
	return buildURL(c.gammaBaseURL, "/markets", clean)
}

// getJSON performs a cached GET against the given URL, parsing the JSON
// response body into dest. ttl controls cache duration; pass 0 to disable
// caching for this call. The cache key is the full URL string.
//
// Cache hit emits an INFO log (`polymarket cache hit`); cache miss emits a
// DEBUG log including upstream latency. 429 responses trigger exponential
// backoff with jitter, capped by defaultRetries.
func (c *Client) getJSON(ctx context.Context, fullURL string, ttl time.Duration, dest any) error {
	if body, ok := c.cache.get(fullURL); ok {
		c.logger.Info("polymarket cache hit", "url", fullURL)
		return json.Unmarshal(body, dest)
	}

	body, err := c.fetchWithRetry(ctx, fullURL)
	if err != nil {
		return err
	}
	c.cache.put(fullURL, body, ttl)
	return json.Unmarshal(body, dest)
}

// fetchWithRetry handles rate limiting + 429 backoff. Returns the response
// body on success. Non-2xx and non-429 responses are wrapped in errHTTPStatus.
func (c *Client) fetchWithRetry(ctx context.Context, fullURL string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		if err := c.limiter.wait(ctx); err != nil {
			return nil, err
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", c.userAgent)

		start := c.now()
		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt < defaultRetries && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				if waitErr := sleepWithJitter(ctx, attempt); waitErr != nil {
					return nil, waitErr
				}
				continue
			}
			return nil, err
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}

		switch {
		case resp.StatusCode >= 200 && resp.StatusCode < 300:
			c.logger.Debug("polymarket fetch",
				"url", fullURL,
				"status", resp.StatusCode,
				"latency_ms", c.now().Sub(start).Milliseconds(),
				"bytes", len(body),
			)
			return body, nil

		case resp.StatusCode == http.StatusTooManyRequests:
			if attempt >= defaultRetries {
				return nil, &errHTTPStatus{Status: resp.StatusCode, Body: string(body)}
			}
			retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
			c.logger.Warn("polymarket 429 throttled",
				"url", fullURL,
				"attempt", attempt+1,
				"retry_after", retryAfter.String(),
			)
			if retryAfter > 0 {
				if err := sleepCtx(ctx, retryAfter); err != nil {
					return nil, err
				}
			} else if err := sleepWithJitter(ctx, attempt); err != nil {
				return nil, err
			}
			continue

		default:
			return nil, &errHTTPStatus{Status: resp.StatusCode, Body: string(body)}
		}
	}
}

// sleepWithJitter sleeps for an exponentially-growing duration with up to
// 25% jitter. attempt is zero-indexed.
func sleepWithJitter(ctx context.Context, attempt int) error {
	d := baseBackoff << attempt
	if d > maxBackoff {
		d = maxBackoff
	}
	jitter := time.Duration(rand.Int63n(int64(d / 4))) // up to ~25% extra
	return sleepCtx(ctx, d+jitter)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseRetryAfter handles both the seconds form ("5") and the HTTP-date form
// of the Retry-After header. Returns 0 if the header is missing or malformed
// (caller falls back to its own backoff).
func parseRetryAfter(h string) time.Duration {
	if h == "" {
		return 0
	}
	if secs, err := strconv.Atoi(h); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

