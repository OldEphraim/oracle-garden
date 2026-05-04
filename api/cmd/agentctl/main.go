// agentctl is a manual driver for the agent runtime. It loads the type
// registry from the configured Postgres, builds a hardcoded Thesis Builder
// agent, supplies fake observation.v1 + news_digest.v1 inputs, calls
// runtime.Invoke against the real Anthropic API, and prints the validated
// thesis output plus cost.
//
// Usage:
//
//	export ANTHROPIC_API_KEY=sk-...
//	export DATABASE_URL=postgres://...
//	go run ./cmd/agentctl
//
// Each run costs roughly $0.001-$0.005 with claude-sonnet-4-6 on this
// prompt. Honors --model so you can spot-check Haiku/Opus pricing.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OldEphraim/sibyl-hub/api/internal/runtime"
	"github.com/OldEphraim/sibyl-hub/api/internal/types"
)

const thesisBuilderPrompt = `You write prediction-market theses for Polymarket markets.
Given an observation (current price, liquidity, time to resolution) and a news digest (recent headlines, sentiment), produce a directional thesis.
Direction must be one of YES, NO, or ABSTAIN. Confidence in [0,1]; calibrate honestly — if your direction agrees with the current price your edge is small. ABSTAIN when the price already reflects the news.`

func main() {
	model := flag.String("model", "claude-sonnet-4-6", "Anthropic model identifier")
	verbose := flag.Bool("v", false, "show DEBUG logs (Anthropic latency, etc.)")
	flag.Parse()

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "agentctl: ANTHROPIC_API_KEY is required")
		os.Exit(2)
	}
	dsn := firstNonEmpty(os.Getenv("TEST_DATABASE_URL"), os.Getenv("DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "agentctl: set DATABASE_URL (registry loads from it)")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentctl: pool:", err)
		os.Exit(1)
	}
	defer pool.Close()

	registry, err := types.Load(ctx, pool)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentctl: registry:", err)
		os.Exit(1)
	}

	client, err := runtime.NewAnthropicClient(runtime.AnthropicOptions{
		APIKey: apiKey,
		Logger: logger,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentctl: client:", err)
		os.Exit(1)
	}
	rt := runtime.NewRuntime(client, registry, logger)

	agent := runtime.Agent{
		Name:         "Thesis Builder (driver)",
		Model:        *model,
		Temperature:  0.5,
		MaxTokens:    1024,
		SystemPrompt: thesisBuilderPrompt,
		OutputType:   "thesis.v1",
	}

	mergedInputs := map[string]json.RawMessage{
		"watcher": json.RawMessage(`{
			"market_slug": "will-bitcoin-hit-150k-by-june-30-2026",
			"condition_id": "0xa0f4c4924ea1a8b410b4ce821c2a9955fad21a1b19bdcfde90816732278b3dd5",
			"question": "Will Bitcoin hit $150k by June 30, 2026?",
			"current_price_yes": 0.0175,
			"current_price_no": 0.9825,
			"volume_24h_usd": 5821652.89,
			"liquidity_usd": 19822.56,
			"time_to_resolution_hours": 1416,
			"yes_token_id": "139156...394586",
			"no_token_id": "132906...156991"
		}`),
		"scout": json.RawMessage(`{
			"market_slug": "will-bitcoin-hit-150k-by-june-30-2026",
			"headlines": [
				{"title": "Bitcoin trades around $108k as ETF inflows slow", "summary": "Spot ETFs saw net outflows for a third straight day amid macro uncertainty.", "source": "Bloomberg"},
				{"title": "Crypto analysts see resistance at $115k", "summary": "Multiple desks flag the $115k zone as a key technical level for any leg up.", "source": "CoinDesk"}
			],
			"sentiment_delta": -0.15,
			"confidence": 0.6
		}`)}

	fmt.Printf("==> Invoking agent %q (model=%s)\n", agent.Name, agent.Model)
	res, err := rt.Invoke(ctx, agent, mergedInputs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentctl: Invoke error:", err)
		printResultMeta(res)
		os.Exit(1)
	}

	fmt.Println("\n==> Output (thesis.v1)")
	pretty, _ := json.MarshalIndent(json.RawMessage(res.Output), "    ", "  ")
	fmt.Println("    " + string(pretty))

	printResultMeta(res)
}

func printResultMeta(res *runtime.InvokeResult) {
	if res == nil {
		return
	}
	fmt.Println("\n==> Stats")
	fmt.Printf("    attempts:           %d\n", res.Attempts)
	fmt.Printf("    prompt_tokens:      %d\n", res.PromptTokens)
	fmt.Printf("    completion_tokens:  %d\n", res.CompletionTokens)
	fmt.Printf("    cost_usd:           $%.6f\n", res.CostUSD)
	fmt.Printf("    latency_ms:         %d\n", res.LatencyMS)
	fmt.Printf("    model (echoed):     %s\n", res.Model)
	fmt.Printf("    stop_reason:        %s\n", res.StopReason)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
