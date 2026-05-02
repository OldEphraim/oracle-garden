package polymarket

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// quietClient builds a *Client whose limiter never blocks (high RPS) and
// whose logger is silent, pointed at the given Gamma + CLOB test servers.
func quietClient(t *testing.T, gammaURL, clobURL string) *Client {
	t.Helper()
	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewClient(ClientOptions{
		HTTPClient:   gammaURL2HTTP(),
		GammaBaseURL: gammaURL,
		CLOBBaseURL:  clobURL,
		Limiter:      rate.NewLimiter(rate.Inf, 1),
		Logger:       silent,
	})
}

func gammaURL2HTTP() *http.Client {
	// Tests don't span hosts that 30-redirect or need cookies; the default
	// timeout-bearing client is fine.
	return &http.Client{Timeout: 5 * time.Second}
}

// TestStringifiedNumberCoercion verifies that the historically-documented
// stringified Gamma fields decode into native Go numeric types: outcomePrices
// as []float64, liquidity as float64, clobTokenIds as []string, outcomes as
// []string. Mirrors the on-the-wire shape we observed live.
func TestStringifiedNumberCoercion(t *testing.T) {
	body := `[{
		"id": "540816",
		"slug": "russia-ukraine-ceasefire-before-gta-vi-554",
		"question": "Russia-Ukraine Ceasefire before GTA VI?",
		"conditionId": "0x9c1a953fe92c8357f1b646ba25d983aa83e90c525992db14fb726fa895cb5763",
		"description": "...",
		"outcomes": "[\"Yes\", \"No\"]",
		"outcomePrices": "[\"0.565\", \"0.435\"]",
		"clobTokenIds": "[\"850149\", \"252731\"]",
		"liquidity": "47500.4779",
		"volume24hr": 3115.7293,
		"endDate": "2026-07-31T12:00:00Z",
		"active": true,
		"closed": false,
		"archived": false,
		"tags": ["politics", "war"]
	}]`

	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/markets" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("slug") != "russia-ukraine-ceasefire-before-gta-vi-554" {
			t.Errorf("unexpected slug: %s", r.URL.Query().Get("slug"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer gamma.Close()

	c := quietClient(t, gamma.URL, "")
	m, err := c.GetMarket(context.Background(), "russia-ukraine-ceasefire-before-gta-vi-554")
	if err != nil {
		t.Fatalf("GetMarket: %v", err)
	}

	if m.Liquidity != 47500.4779 {
		t.Errorf("liquidity: got %v want 47500.4779", m.Liquidity)
	}
	if m.Volume24Hr != 3115.7293 {
		t.Errorf("volume24hr: got %v want 3115.7293", m.Volume24Hr)
	}
	if got, want := m.OutcomePrices, []float64{0.565, 0.435}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("outcomePrices: got %v want %v", got, want)
	}
	if got, want := m.Outcomes, []string{"Yes", "No"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("outcomes: got %v want %v", got, want)
	}
	if got, want := m.ClobTokenIDs, []string{"850149", "252731"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("clobTokenIds: got %v want %v", got, want)
	}
	if !m.EndDate.Equal(time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("endDate: got %v", m.EndDate)
	}
}

// TestCacheHit verifies a second call for the same URL is served from cache
// (i.e., the upstream hit count does not increment).
func TestCacheHit(t *testing.T) {
	var hits int32
	body := `[{
		"id":"1","slug":"x","question":"q","conditionId":"c",
		"outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.5\",\"0.5\"]","clobTokenIds":"[\"a\",\"b\"]",
		"liquidity":"0","volume24hr":0,"endDate":"2026-01-01T00:00:00Z",
		"active":true,"closed":false,"archived":false
	}]`
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, body)
	}))
	defer gamma.Close()

	c := quietClient(t, gamma.URL, "")

	if _, err := c.GetMarket(context.Background(), "x"); err != nil {
		t.Fatalf("first GetMarket: %v", err)
	}
	if _, err := c.GetMarket(context.Background(), "x"); err != nil {
		t.Fatalf("second GetMarket: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("upstream hits: got %d want 1 (cache miss on second call)", got)
	}
}

// TestCacheTTLExpiry verifies that an entry past its TTL triggers a refetch.
func TestCacheTTLExpiry(t *testing.T) {
	var hits int32
	body := `[{
		"id":"1","slug":"y","question":"q","conditionId":"c",
		"outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.5\",\"0.5\"]","clobTokenIds":"[\"a\",\"b\"]",
		"liquidity":"0","volume24hr":0,"endDate":"2026-01-01T00:00:00Z",
		"active":true,"closed":false,"archived":false
	}]`
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, body)
	}))
	defer gamma.Close()

	t0 := time.Now()
	clock := func() time.Time { return t0 }
	c := NewClient(ClientOptions{
		GammaBaseURL: gamma.URL,
		Limiter:      rate.NewLimiter(rate.Inf, 1),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:          func() time.Time { return clock() },
	})

	if _, err := c.GetMarket(context.Background(), "y"); err != nil {
		t.Fatalf("first GetMarket: %v", err)
	}
	// Advance past TTLMetadata (5 min) to force refetch.
	clock = func() time.Time { return t0.Add(TTLMetadata + time.Second) }
	if _, err := c.GetMarket(context.Background(), "y"); err != nil {
		t.Fatalf("second GetMarket: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("upstream hits after TTL expiry: got %d want 2", got)
	}
}

// TestRetryOn429 verifies the client retries after a 429 and ultimately
// succeeds. We assert the upstream was called at least twice.
func TestRetryOn429(t *testing.T) {
	var hits int32
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if n == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"mid":"0.5"}`)
	}))
	defer gamma.Close()

	c := quietClient(t, "", gamma.URL)
	mid, err := c.GetMidpoint(context.Background(), "tok-1")
	if err != nil {
		t.Fatalf("GetMidpoint: %v", err)
	}
	if mid != 0.5 {
		t.Errorf("mid: got %v want 0.5", mid)
	}
	if got := atomic.LoadInt32(&hits); got < 2 {
		t.Errorf("upstream hits: got %d want >= 2 (no retry on 429)", got)
	}
}

// TestTokenIDRouting verifies that GetMidpoint, GetOrderbook, and
// GetPriceHistory issue the token_id parameter against the CLOB endpoints —
// not a conditionId, and not against Gamma. Catches the CLAUDE.md gotcha.
func TestTokenIDRouting(t *testing.T) {
	type seen struct {
		path  string
		query url.Values
	}
	var observed []seen
	clob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = append(observed, seen{path: r.URL.Path, query: r.URL.Query()})
		switch r.URL.Path {
		case "/midpoint":
			_, _ = io.WriteString(w, `{"mid":"0.5"}`)
		case "/book":
			_, _ = io.WriteString(w, `{"market":"0xabc","asset_id":"tok-1","timestamp":"0","hash":"","bids":[],"asks":[]}`)
		case "/prices-history":
			_, _ = io.WriteString(w, `{"history":[]}`)
		default:
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer clob.Close()

	c := quietClient(t, "", clob.URL)
	ctx := context.Background()

	if _, err := c.GetMidpoint(ctx, "tok-1"); err != nil {
		t.Fatalf("GetMidpoint: %v", err)
	}
	if _, err := c.GetOrderbook(ctx, "tok-1"); err != nil {
		t.Fatalf("GetOrderbook: %v", err)
	}
	if _, err := c.GetPriceHistory(ctx, "tok-1", "1d", 60); err != nil {
		t.Fatalf("GetPriceHistory: %v", err)
	}

	if len(observed) != 3 {
		t.Fatalf("expected 3 CLOB calls, got %d: %v", len(observed), observed)
	}

	// /midpoint and /book use ?token_id=...; /prices-history uses ?market=...
	// (CLAUDE.md gotcha: that param value is still a token_id, not a conditionId).
	for _, s := range observed[:2] {
		if got := s.query.Get("token_id"); got != "tok-1" {
			t.Errorf("%s: token_id query: got %q want tok-1", s.path, got)
		}
		if s.query.Get("conditionId") != "" {
			t.Errorf("%s: should not pass conditionId", s.path)
		}
	}
	if got := observed[2].query.Get("market"); got != "tok-1" {
		t.Errorf("/prices-history market param: got %q want tok-1", got)
	}
	if observed[2].query.Get("interval") != "1d" || observed[2].query.Get("fidelity") != "60" {
		t.Errorf("/prices-history: unexpected query %v", observed[2].query)
	}
}

// TestOrderbookCoercion verifies bids/asks levels parse from numeric strings.
func TestOrderbookCoercion(t *testing.T) {
	clob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{
			"market":"0xabc","asset_id":"tok-1","timestamp":"1","hash":"h",
			"bids":[{"price":"0.65","size":"100"},{"price":"0.64","size":"50"}],
			"asks":[{"price":"0.66","size":"75"}]
		}`)
	}))
	defer clob.Close()

	c := quietClient(t, "", clob.URL)
	ob, err := c.GetOrderbook(context.Background(), "tok-1")
	if err != nil {
		t.Fatalf("GetOrderbook: %v", err)
	}
	if len(ob.Bids) != 2 || ob.Bids[0].Price != 0.65 || ob.Bids[0].Size != 100 {
		t.Errorf("bids: %+v", ob.Bids)
	}
	if len(ob.Asks) != 1 || ob.Asks[0].Price != 0.66 || ob.Asks[0].Size != 75 {
		t.Errorf("asks: %+v", ob.Asks)
	}
}

// TestPriceHistoryDecode verifies typed-number CLOB price history decodes
// each point's `t` (unix seconds) into a UTC time.Time.
func TestPriceHistoryDecode(t *testing.T) {
	clob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"history":[{"t":1777604404,"p":0.59},{"t":1777608005,"p":0.585}]}`)
	}))
	defer clob.Close()

	c := quietClient(t, "", clob.URL)
	ps, err := c.GetPriceHistory(context.Background(), "tok-1", "1d", 60)
	if err != nil {
		t.Fatalf("GetPriceHistory: %v", err)
	}
	if len(ps.History) != 2 {
		t.Fatalf("history length: got %d want 2", len(ps.History))
	}
	if ps.History[0].Price != 0.59 {
		t.Errorf("history[0].price: got %v want 0.59", ps.History[0].Price)
	}
	if ps.History[0].Time.Unix() != 1777604404 {
		t.Errorf("history[0].time: got %v want unix 1777604404", ps.History[0].Time)
	}
}

// TestNonRetriableError verifies that a 400 is surfaced without retries.
func TestNonRetriableError(t *testing.T) {
	var hits int32
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, `{"error":"bad"}`, http.StatusBadRequest)
	}))
	defer gamma.Close()

	c := quietClient(t, gamma.URL, "")
	_, err := c.GetMarket(context.Background(), "x")
	if err == nil {
		t.Fatalf("expected error for 400, got nil")
	}
	if !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("expected error to mention HTTP 400, got: %v", err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Errorf("expected exactly 1 upstream hit, got %d", hits)
	}
}

// TestPublicSearchExtractsMarkets verifies SearchMarkets:
//   - hits /public-search (NOT /search, which needs auth)
//   - sends q, limit_per_type, keep_closed_markets=0
//   - flattens nested events[].markets[]
//   - drops markets whose `closed` flag is true (the API only filters at
//     event level)
//   - truncates to the caller's limit
func TestPublicSearchExtractsMarkets(t *testing.T) {
	body := `{
		"events": [
			{"markets": [
				{
					"id":"1","slug":"a","question":"A","conditionId":"c1",
					"outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.5\",\"0.5\"]","clobTokenIds":"[\"t1\",\"t2\"]",
					"liquidity":null,"volume24hr":null,"endDate":"2026-12-31T00:00:00Z",
					"active":true,"closed":false,"archived":false
				},
				{
					"id":"2","slug":"b-closed","question":"B","conditionId":"c2",
					"outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0\",\"1\"]","clobTokenIds":"[\"t3\",\"t4\"]",
					"liquidity":"0","volume24hr":0,"endDate":"2025-01-01T00:00:00Z",
					"active":false,"closed":true,"archived":false
				}
			]},
			{"markets": [
				{
					"id":"3","slug":"c","question":"C","conditionId":"c3",
					"outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.7\",\"0.3\"]","clobTokenIds":"[\"t5\",\"t6\"]",
					"liquidity":"100","volume24hr":50,"endDate":"2026-12-31T00:00:00Z",
					"active":true,"closed":false,"archived":false
				}
			]}
		],
		"pagination": {}
	}`

	var seenPath string
	var seenQuery url.Values
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenQuery = r.URL.Query()
		_, _ = io.WriteString(w, body)
	}))
	defer gamma.Close()

	c := quietClient(t, gamma.URL, "")
	out, err := c.SearchMarkets(context.Background(), "trump", 5)
	if err != nil {
		t.Fatalf("SearchMarkets: %v", err)
	}

	if seenPath != "/public-search" {
		t.Errorf("path: got %q want /public-search", seenPath)
	}
	if got, want := seenQuery.Get("q"), "trump"; got != want {
		t.Errorf("q: got %q want %q", got, want)
	}
	if got, want := seenQuery.Get("limit_per_type"), "5"; got != want {
		t.Errorf("limit_per_type: got %q want %q", got, want)
	}
	if got, want := seenQuery.Get("keep_closed_markets"), "0"; got != want {
		t.Errorf("keep_closed_markets: got %q want %q (must be 0/1, NOT true/false)", got, want)
	}
	if len(out) != 2 {
		t.Fatalf("results: got %d want 2 (closed market should be filtered)", len(out))
	}
	if out[0].Slug != "a" || out[1].Slug != "c" {
		t.Errorf("expected slugs [a, c], got [%s, %s]", out[0].Slug, out[1].Slug)
	}
}

// TestSearchMarketsEmptyResults verifies that an empty search returns a
// non-nil empty slice (not nil), so callers don't need to nil-check.
func TestSearchMarketsEmptyResults(t *testing.T) {
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"events":[],"pagination":{}}`)
	}))
	defer gamma.Close()

	c := quietClient(t, gamma.URL, "")
	out, err := c.SearchMarkets(context.Background(), "no-such-thing", 5)
	if err != nil {
		t.Fatalf("SearchMarkets: %v", err)
	}
	if out == nil {
		t.Fatalf("expected non-nil empty slice, got nil")
	}
	if len(out) != 0 {
		t.Fatalf("expected 0 results, got %d", len(out))
	}
}

// TestSearchMarketsRespectsLimit verifies the caller's limit truncates the
// flattened results.
func TestSearchMarketsRespectsLimit(t *testing.T) {
	body := `{"events":[
		{"markets":[
			{"id":"1","slug":"a","question":"","conditionId":"","outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.5\",\"0.5\"]","clobTokenIds":"[\"x\",\"y\"]","liquidity":"0","volume24hr":0,"endDate":"2026-01-01T00:00:00Z","active":true,"closed":false,"archived":false},
			{"id":"2","slug":"b","question":"","conditionId":"","outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.5\",\"0.5\"]","clobTokenIds":"[\"x\",\"y\"]","liquidity":"0","volume24hr":0,"endDate":"2026-01-01T00:00:00Z","active":true,"closed":false,"archived":false},
			{"id":"3","slug":"c","question":"","conditionId":"","outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.5\",\"0.5\"]","clobTokenIds":"[\"x\",\"y\"]","liquidity":"0","volume24hr":0,"endDate":"2026-01-01T00:00:00Z","active":true,"closed":false,"archived":false}
		]}
	],"pagination":{}}`
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, body)
	}))
	defer gamma.Close()

	c := quietClient(t, gamma.URL, "")
	out, err := c.SearchMarkets(context.Background(), "x", 2)
	if err != nil {
		t.Fatalf("SearchMarkets: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("limit=2: got %d results", len(out))
	}
}

// TestUserAgentOnEveryRequest verifies the configured User-Agent is sent on
// every outbound request (Gamma + CLOB), and that ClientOptions.UserAgent
// overrides the default.
func TestUserAgentOnEveryRequest(t *testing.T) {
	const customUA = "test-ua/1.0"
	var seenUAs []string

	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUAs = append(seenUAs, r.Header.Get("User-Agent"))
		// Default response works for both /markets and /public-search shapes
		// the tests below probe.
		_, _ = io.WriteString(w, `[{"id":"1","slug":"x","question":"","conditionId":"","outcomes":"[\"Yes\",\"No\"]","outcomePrices":"[\"0.5\",\"0.5\"]","clobTokenIds":"[\"x\",\"y\"]","liquidity":"0","volume24hr":0,"endDate":"2026-01-01T00:00:00Z","active":true,"closed":false,"archived":false}]`)
	}))
	defer gamma.Close()

	clob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUAs = append(seenUAs, r.Header.Get("User-Agent"))
		_, _ = io.WriteString(w, `{"mid":"0.5"}`)
	}))
	defer clob.Close()

	c := NewClient(ClientOptions{
		HTTPClient:   gammaURL2HTTP(),
		GammaBaseURL: gamma.URL,
		CLOBBaseURL:  clob.URL,
		Limiter:      rate.NewLimiter(rate.Inf, 1),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		UserAgent:    customUA,
	})
	ctx := context.Background()
	if _, err := c.GetMarket(ctx, "x"); err != nil {
		t.Fatalf("GetMarket: %v", err)
	}
	if _, err := c.GetMidpoint(ctx, "tok-1"); err != nil {
		t.Fatalf("GetMidpoint: %v", err)
	}

	if len(seenUAs) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(seenUAs))
	}
	for i, ua := range seenUAs {
		if ua != customUA {
			t.Errorf("request %d: User-Agent: got %q want %q", i, ua, customUA)
		}
	}
}

// TestUserAgentDefault verifies the default User-Agent is set when no
// override is provided.
func TestUserAgentDefault(t *testing.T) {
	var seenUA string
	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenUA = r.Header.Get("User-Agent")
		_, _ = io.WriteString(w, `[]`)
	}))
	defer gamma.Close()

	c := NewClient(ClientOptions{
		HTTPClient:   gammaURL2HTTP(),
		GammaBaseURL: gamma.URL,
		Limiter:      rate.NewLimiter(rate.Inf, 1),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	_, _ = c.SearchMarkets(context.Background(), "anything", 1)

	if !strings.HasPrefix(seenUA, "sibyl-hub/") {
		t.Errorf("default User-Agent: got %q, expected prefix sibyl-hub/", seenUA)
	}
}

// TestMarketsAllowlistDropsUnknown verifies that gammaMarketsURL drops query
// keys not on the allowlist and emits a WARN log mentioning the dropped key.
// Known-good keys are passed through.
func TestMarketsAllowlistDropsUnknown(t *testing.T) {
	var logged strings.Builder
	logger := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))

	gamma := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Confirm the unknown key was dropped before the request fired.
		if r.URL.Query().Get("question_contains") != "" {
			t.Errorf("question_contains should have been dropped")
		}
		// Known-good slug must still pass through.
		if r.URL.Query().Get("slug") != "x" {
			t.Errorf("slug=x should pass through")
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	defer gamma.Close()

	c := NewClient(ClientOptions{
		HTTPClient:   gammaURL2HTTP(),
		GammaBaseURL: gamma.URL,
		Limiter:      rate.NewLimiter(rate.Inf, 1),
		Logger:       logger,
	})
	v := url.Values{}
	v.Set("slug", "x")
	v.Set("question_contains", "trump")
	v.Set("limit", "1")
	_, _ = c.fetchWithRetry(context.Background(), c.gammaMarketsURL(v))

	out := logged.String()
	if !strings.Contains(out, "dropping unrecognized /markets query param") {
		t.Errorf("expected WARN about dropping unrecognized param, got: %s", out)
	}
	if !strings.Contains(out, "question_contains") {
		t.Errorf("expected WARN to mention 'question_contains', got: %s", out)
	}
}
