package polymarket

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
)

// GetMarket resolves a market by slug via Gamma /markets?slug=<exact>&limit=1.
//
// CLAUDE.md documents the endpoint as `GET /markets/{slug-or-id}`, but the
// live Gamma API rejects slugs at /markets/{path-segment} with HTTP 422
// ("id is invalid") — only numeric IDs work in the path form. The list-with-
// filter form below is the only public way to resolve a market by slug today.
// Goes through gammaMarketsURL so the /markets allowlist applies.
// (See DECISION_LOG.md, Phase 2.)
func (c *Client) GetMarket(ctx context.Context, slug string) (*Market, error) {
	if slug == "" {
		return nil, fmt.Errorf("polymarket: GetMarket: empty slug")
	}
	q := url.Values{}
	q.Set("slug", slug)
	q.Set("limit", "1")
	u := c.gammaMarketsURL(q)

	var out []*Market
	if err := c.getJSON(ctx, u, TTLMetadata, &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("polymarket: GetMarket: no market with slug %q", slug)
	}
	return out[0], nil
}

// publicSearchResponse mirrors the wire shape of Gamma /public-search.
// The endpoint groups results by entity type (events, profiles, tags). Each
// event holds a nested `markets` array — that is where individual binary
// markets actually live; there is no top-level `markets` field.
type publicSearchResponse struct {
	Events []publicSearchEvent `json:"events"`
}

type publicSearchEvent struct {
	Markets []*Market `json:"markets"`
}

// SearchMarkets does a free-text search via Gamma /public-search, walks the
// nested events[].markets[], drops markets whose `closed` flag is true, and
// truncates to limit. Always returns a non-nil slice (possibly empty).
//
// Polymarket's `/public-search` is publicly accessible (no auth) — distinct
// from `/search`, which requires authentication. The User-Agent header is
// applied automatically by the client; some Polymarket edge nodes deny
// requests without one.
//
// Notes on observed quirks:
//   - `keep_closed_markets` expects "0" or "1" — passing "true"/"false"
//     returns HTTP 422.
//   - Even with `keep_closed_markets=0`, the filter is applied at the EVENT
//     level (events whose markets are all closed are dropped); markets
//     within a still-open event can themselves be closed. This method
//     therefore filters `m.Closed` client-side as well.
func (c *Client) SearchMarkets(ctx context.Context, query string, limit int) ([]*Market, error) {
	if limit <= 0 {
		limit = 20
	}
	q := url.Values{}
	if query != "" {
		q.Set("q", query)
	}
	q.Set("limit_per_type", strconv.Itoa(limit))
	q.Set("keep_closed_markets", "0")
	u := buildURL(c.gammaBaseURL, "/public-search", q)

	var resp publicSearchResponse
	if err := c.getJSON(ctx, u, TTLMetadata, &resp); err != nil {
		return nil, err
	}

	out := make([]*Market, 0, limit)
	for _, ev := range resp.Events {
		for _, m := range ev.Markets {
			if m == nil || m.Closed {
				continue
			}
			out = append(out, m)
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// GetOrderbook fetches the full L2 orderbook for one CLOB token_id.
//
// tokenID MUST be one of the values in Market.ClobTokenIDs (typically two
// per market — one per outcome). Passing a conditionId here returns garbage
// or empty levels.
func (c *Client) GetOrderbook(ctx context.Context, tokenID string) (*Orderbook, error) {
	if tokenID == "" {
		return nil, fmt.Errorf("polymarket: GetOrderbook: empty token_id")
	}
	q := url.Values{}
	q.Set("token_id", tokenID)
	u := buildURL(c.clobBaseURL, "/book", q)

	var out Orderbook
	if err := c.getJSON(ctx, u, TTLOrderbook, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetMidpoint fetches the orderbook midpoint for one CLOB token_id. Returns
// the parsed float (CLOB's wire format is `{"mid":"0.565"}` — string).
//
// Same token_id-vs-conditionId rule as GetOrderbook.
func (c *Client) GetMidpoint(ctx context.Context, tokenID string) (float64, error) {
	if tokenID == "" {
		return 0, fmt.Errorf("polymarket: GetMidpoint: empty token_id")
	}
	q := url.Values{}
	q.Set("token_id", tokenID)
	u := buildURL(c.clobBaseURL, "/midpoint", q)

	var out struct {
		Mid flexFloat `json:"mid"`
	}
	if err := c.getJSON(ctx, u, TTLPrice, &out); err != nil {
		return 0, err
	}
	return float64(out.Mid), nil
}

// GetPriceHistory fetches historical mid prices for a token_id.
//
// `interval` is a Polymarket-defined string (commonly "1h", "1d", "1w", "max");
// `fidelity` is the bucket size in minutes (passed through verbatim).
func (c *Client) GetPriceHistory(ctx context.Context, tokenID, interval string, fidelity int) (*PriceSeries, error) {
	if tokenID == "" {
		return nil, fmt.Errorf("polymarket: GetPriceHistory: empty token_id")
	}
	q := url.Values{}
	q.Set("market", tokenID) // CLOB names this param `market` but it is a token_id; see CLAUDE.md gotcha.
	if interval != "" {
		q.Set("interval", interval)
	}
	if fidelity > 0 {
		q.Set("fidelity", strconv.Itoa(fidelity))
	}
	u := buildURL(c.clobBaseURL, "/prices-history", q)

	var out PriceSeries
	if err := c.getJSON(ctx, u, TTLPrice, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
