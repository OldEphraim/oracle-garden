// agentctl is a manual driver for the agent runtime. It builds an in-process
// agent (no DB-backed agent_template yet — the seed migration lands in
// Phase 14), supplies inputs, calls runtime.Invoke against the real
// Anthropic API, and prints the validated output plus cost.
//
// Two agent profiles, selected via --agent:
//
//	thesis-builder  (Phase 4) no tools, takes observation+news_digest, emits thesis.v1
//	market-watcher  (Phase 5) tools-enabled, takes a market_target, emits observation.v1
//
// Usage:
//
//	export ANTHROPIC_API_KEY=sk-...
//	export DATABASE_URL=postgres://...
//	go run ./cmd/agentctl --agent thesis-builder
//	go run ./cmd/agentctl --agent market-watcher --slug will-bitcoin-hit-150k-by-june-30-2026
//
// Each thesis-builder run costs ~$0.005-$0.010 with Sonnet 4.6; a market-
// watcher run typically rounds to 2-3 tool calls and costs ~$0.01-$0.02
// depending on the orderbook size.
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

	"github.com/OldEphraim/sibyl-hub/api/internal/polymarket"
	"github.com/OldEphraim/sibyl-hub/api/internal/runtime"
	"github.com/OldEphraim/sibyl-hub/api/internal/tools"
	"github.com/OldEphraim/sibyl-hub/api/internal/types"
)

const thesisBuilderPrompt = `You write prediction-market theses for Polymarket markets.
Given an observation (current price, liquidity, time to resolution) and a news digest (recent headlines, sentiment), produce a directional thesis.
Direction must be one of YES, NO, or ABSTAIN. Confidence in [0,1]; calibrate honestly — if your direction agrees with the current price your edge is small. ABSTAIN when the price already reflects the news.`

const marketWatcherPrompt = `You observe a Polymarket market and emit a structured observation.

You will be given a market_target with a slug. Your job:
  1. Call polymarket.gamma_get_market with the slug to resolve the market metadata, including conditionId and clobTokenIds. The clobTokenIds array is typically two entries — the first corresponds to the first outcome (usually "Yes"), the second to the second outcome (usually "No").
  2. Call polymarket.clob_get_midpoint with each of the two clobTokenIds to get current_price_yes and current_price_no.
  3. Use the gamma metadata's volume24hr, liquidity, endDate to populate the remaining fields. Compute time_to_resolution_hours from endDate vs now.
  4. Emit the observation.v1 JSON. Don't interpret — only observe.

The token_id values must come from clobTokenIds verbatim — passing the conditionId returns garbage. Both prices live in [0, 1].`

func main() {
	agentName := flag.String("agent", "thesis-builder", "which agent profile to run: thesis-builder | market-watcher")
	model := flag.String("model", "claude-sonnet-4-6", "Anthropic model identifier")
	slug := flag.String("slug", "", "Polymarket slug (required for market-watcher)")
	verbose := flag.Bool("v", false, "show DEBUG logs (Anthropic latency, tool dispatch)")
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

	// Tools registry — wired with the live polymarket adapter and the
	// server-side web_search tool. Both agents share the same registry;
	// thesis-builder happens not to declare any tools.
	pmClient := polymarket.NewClient(polymarket.ClientOptions{Logger: logger})
	toolRegistry := tools.NewRegistry()
	tools.RegisterPolymarketTools(toolRegistry, pmClient)
	tools.RegisterWebSearch(toolRegistry, 5)

	rt := runtime.NewRuntime(client, registry, toolRegistry, logger)

	agent, mergedInputs, err := buildAgent(*agentName, *model, *slug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentctl:", err)
		os.Exit(2)
	}

	fmt.Printf("==> Invoking agent %q (model=%s)\n", agent.Name, agent.Model)
	res, err := rt.Invoke(ctx, agent, mergedInputs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "agentctl: Invoke error:", err)
		printResultMeta(res)
		os.Exit(1)
	}

	fmt.Printf("\n==> Output (%s)\n", agent.OutputType)
	pretty, _ := json.MarshalIndent(json.RawMessage(res.Output), "    ", "  ")
	fmt.Println("    " + string(pretty))

	printResultMeta(res)
}

func buildAgent(profile, model, slug string) (runtime.Agent, map[string]json.RawMessage, error) {
	switch profile {
	case "thesis-builder":
		return runtime.Agent{
			Name:         "Thesis Builder (driver)",
			Model:        model,
			Temperature:  0.5,
			MaxTokens:    1024,
			SystemPrompt: thesisBuilderPrompt,
			OutputType:   "thesis.v1",
		}, fakeThesisInputs(), nil
	case "market-watcher":
		if slug == "" {
			return runtime.Agent{}, nil, fmt.Errorf("market-watcher requires --slug")
		}
		return runtime.Agent{
				Name:         "Market Watcher (driver)",
				Model:        model,
				Temperature:  0.2,
				MaxTokens:    1500,
				SystemPrompt: marketWatcherPrompt,
				OutputType:   "observation.v1",
				Tools: []string{
					"polymarket.gamma_get_market",
					"polymarket.clob_get_midpoint",
				},
			}, map[string]json.RawMessage{
				"target": json.RawMessage(fmt.Sprintf(`{"market_slug":%q}`, slug)),
			}, nil
	default:
		return runtime.Agent{}, nil, fmt.Errorf("unknown --agent %q (try thesis-builder | market-watcher)", profile)
	}
}

func fakeThesisInputs() map[string]json.RawMessage {
	return map[string]json.RawMessage{
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
