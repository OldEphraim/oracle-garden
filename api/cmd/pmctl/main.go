// pmctl is a manual driver for the Polymarket adapter. Pass --slug <slug>
// to resolve a market and print its midpoint. The tool intentionally calls
// each underlying method twice so the cache-hit log line is visible.
//
//	go run ./cmd/pmctl --slug presidential-election-winner-2028
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/OldEphraim/sibyl-hub/api/internal/polymarket"
)

func main() {
	slug := flag.String("slug", "", "Polymarket market slug to resolve (required)")
	verbose := flag.Bool("v", false, "show DEBUG-level logs (fetch latency, etc.)")
	flag.Parse()

	if *slug == "" {
		fmt.Fprintln(os.Stderr, "pmctl: --slug is required")
		flag.Usage()
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	c := polymarket.NewClient(polymarket.ClientOptions{
		Logger: logger,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := run(ctx, c, *slug); err != nil {
		fmt.Fprintln(os.Stderr, "pmctl error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, c *polymarket.Client, slug string) error {
	fmt.Printf("\n==> Resolving market %q\n", slug)
	m, err := c.GetMarket(ctx, slug)
	if err != nil {
		return err
	}
	printMarket(m)

	if len(m.ClobTokenIDs) == 0 {
		return fmt.Errorf("market has no clobTokenIds; cannot fetch midpoint")
	}

	yesTokenID := m.ClobTokenIDs[0]
	fmt.Printf("\n==> Midpoint of YES token (%s)\n", shorten(yesTokenID))
	mid, err := c.GetMidpoint(ctx, yesTokenID)
	if err != nil {
		return err
	}
	fmt.Printf("    midpoint = %.4f\n", mid)

	// Second call demonstrates cache hit (logger emits "polymarket cache hit").
	fmt.Printf("\n==> Re-resolving market (should hit cache)\n")
	if _, err := c.GetMarket(ctx, slug); err != nil {
		return err
	}
	fmt.Printf("\n==> Re-fetching midpoint (should hit cache)\n")
	if _, err := c.GetMidpoint(ctx, yesTokenID); err != nil {
		return err
	}
	return nil
}

func printMarket(m *polymarket.Market) {
	fmt.Printf("    question:        %s\n", m.Question)
	fmt.Printf("    slug:            %s\n", m.Slug)
	fmt.Printf("    conditionId:     %s\n", m.ConditionID)
	if !m.EndDate.IsZero() {
		fmt.Printf("    endDate:         %s\n", m.EndDate.Format(time.RFC3339))
	}
	fmt.Printf("    active=%v closed=%v archived=%v\n", m.Active, m.Closed, m.Archived)
	fmt.Printf("    liquidity:       %.2f\n", m.Liquidity)
	fmt.Printf("    volume24h:       %.2f\n", m.Volume24Hr)
	fmt.Printf("    outcomes:        %v\n", m.Outcomes)
	fmt.Printf("    outcomePrices:   %v\n", m.OutcomePrices)
	fmt.Printf("    clobTokenIds:\n")
	for i, id := range m.ClobTokenIDs {
		label := ""
		if i < len(m.Outcomes) {
			label = " (" + m.Outcomes[i] + ")"
		}
		fmt.Printf("      [%d]%s %s\n", i, label, id)
	}
}

func shorten(s string) string {
	if len(s) <= 14 {
		return s
	}
	return s[:6] + "…" + s[len(s)-6:] + " (" + strings.TrimSpace(fmt.Sprintf("%d chars", len(s))) + ")"
}
