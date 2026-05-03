// typecheck is a manual driver for the type registry. It loads the registry
// from the configured Postgres, validates a sample observation.v1 payload
// (passing), then validates a deliberately-broken one (failing with readable
// errors). Used during Phase 3 verification per STEPS.md.
//
//	DATABASE_URL=postgres://... go run ./cmd/typecheck
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OldEphraim/sibyl-hub/api/internal/types"
)

func main() {
	dsn := firstNonEmpty(os.Getenv("TEST_DATABASE_URL"), os.Getenv("DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "typecheck: set DATABASE_URL (or TEST_DATABASE_URL)")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "typecheck: pool:", err)
		os.Exit(1)
	}
	defer pool.Close()

	registry, err := types.Load(ctx, pool)
	if err != nil {
		fmt.Fprintln(os.Stderr, "typecheck: load registry:", err)
		os.Exit(1)
	}
	fmt.Printf("==> Loaded %d types from registry:\n", len(registry.All()))
	for _, d := range registry.All() {
		core := ""
		if d.IsCore {
			core = " (core)"
		}
		fmt.Printf("    - %s%s\n", d.ID(), core)
	}

	good := []byte(`{
		"market_slug":"will-bitcoin-hit-150k-by-june-30-2026",
		"condition_id":"0xa0f4c4924ea1a8b410b4ce821c2a9955fad21a1b19bdcfde90816732278b3dd5",
		"question":"Will Bitcoin hit $150k by June 30, 2026?",
		"current_price_yes":0.014,
		"current_price_no":0.986,
		"volume_24h_usd":5821652.89,
		"liquidity_usd":19822.56,
		"time_to_resolution_hours":1416.0,
		"recent_price_change_24h_pct":-3.5,
		"yes_token_id":"139156...394586",
		"no_token_id":"132906...156991"
	}`)
	fmt.Println("\n==> Validating a well-formed observation.v1 payload")
	errs, err := registry.ValidateAgainst(good, "observation.v1")
	if err != nil {
		fmt.Fprintln(os.Stderr, "    parse error:", err)
		os.Exit(1)
	}
	if len(errs) == 0 {
		fmt.Println("    OK — no errors")
	} else {
		fmt.Println("    UNEXPECTED — got errors:")
		for _, e := range errs {
			fmt.Printf("      • %s\n", e)
		}
		os.Exit(1)
	}

	bad := []byte(`{
		"market_slug":"x",
		"condition_id":"0xabc",
		"question":"q",
		"current_price_yes":1.5,
		"current_price_no":"not-a-number",
		"volume_24h_usd":-100,
		"yes_token_id":"y",
		"no_token_id":"n",
		"unexpected_field":"shouldn't be here"
	}`)
	fmt.Println("\n==> Validating a deliberately-broken observation.v1 payload")
	fmt.Println("    issues seeded: missing required field, out-of-range price,")
	fmt.Println("    string-where-number, negative volume, additionalProperty.")
	errs, err = registry.ValidateAgainst(bad, "observation.v1")
	if err != nil {
		fmt.Fprintln(os.Stderr, "    parse error:", err)
		os.Exit(1)
	}
	if len(errs) == 0 {
		fmt.Println("    UNEXPECTED — no validation errors reported.")
		os.Exit(1)
	}
	fmt.Printf("    %d validation error(s):\n", len(errs))
	for _, e := range errs {
		fmt.Printf("      • %s\n", e)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
