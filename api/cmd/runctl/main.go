// runctl is a manual driver for the workflow execution engine. It
// bootstraps a minimal seed (5 system agent_templates + the happy-path
// workflow) inline so the engine has something to run, then invokes the
// engine against a --slug, prints the run + steps + paper_trade.
//
// Phase 14 will replace this inline bootstrap with the real `make seed`
// loader; runctl then becomes a thin wrapper that picks the seeded
// workflow by name (or just runs whatever already exists).
//
// Usage:
//
//	export ANTHROPIC_API_KEY=sk-...
//	export DATABASE_URL=postgres://...
//	go run ./cmd/runctl --slug will-bitcoin-hit-150k-by-june-30-2026
//
// One run hits Anthropic 5+ times (one per agent step + tool rounds).
// Budget for ~$0.05–$0.20 per run; the daily-cost cap ($5) lands in Phase 7.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OldEphraim/sibyl-hub/api/internal/billing"
	"github.com/OldEphraim/sibyl-hub/api/internal/db/repos"
	"github.com/OldEphraim/sibyl-hub/api/internal/engine"
	"github.com/OldEphraim/sibyl-hub/api/internal/polymarket"
	"github.com/OldEphraim/sibyl-hub/api/internal/runtime"
	"github.com/OldEphraim/sibyl-hub/api/internal/tools"
	"github.com/OldEphraim/sibyl-hub/api/internal/types"
)

func main() {
	slug := flag.String("slug", "", "Polymarket market slug (required)")
	verbose := flag.Bool("v", false, "DEBUG logs (Anthropic latency, tool dispatch)")
	userEmail := flag.String("user-email", "",
		"reuse a stable user across invocations (created if missing). Default: ephemeral user per run.")
	repeat := flag.Int("repeat", 1, "number of runs to execute back-to-back; useful for quota smoke tests")
	flag.Parse()

	if *slug == "" {
		fmt.Fprintln(os.Stderr, "runctl: --slug is required")
		flag.Usage()
		os.Exit(2)
	}
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "runctl: ANTHROPIC_API_KEY is required")
		os.Exit(2)
	}
	dsn := firstNonEmpty(os.Getenv("TEST_DATABASE_URL"), os.Getenv("DATABASE_URL"))
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "runctl: set DATABASE_URL")
		os.Exit(2)
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	if *repeat < 1 {
		*repeat = 1
	}
	// Per-repeat upper bound — give each invocation up to ~12 minutes
	// (engine cap is 10, plus slack). Run loop walks repeats sequentially.
	ctx := context.Background()

	dbCtx, dbCancel := context.WithTimeout(ctx, 10*time.Second)
	pool, err := pgxpool.New(dbCtx, dsn)
	dbCancel()
	if err != nil {
		fail("pool", err)
	}
	defer pool.Close()

	registry, err := types.Load(ctx, pool)
	if err != nil {
		fail("registry", err)
	}

	// Anthropic + polymarket + tools.
	anthropicClient, err := runtime.NewAnthropicClient(runtime.AnthropicOptions{APIKey: apiKey, Logger: logger})
	if err != nil {
		fail("anthropic", err)
	}
	pmClient := polymarket.NewClient(polymarket.ClientOptions{Logger: logger})
	toolRegistry := tools.NewRegistry()
	tools.RegisterPolymarketTools(toolRegistry, pmClient)
	tools.RegisterWebSearch(toolRegistry, 5)

	rt := runtime.NewRuntime(anthropicClient, registry, toolRegistry, logger)

	usersRepo := repos.NewUsersRepo()
	agentsRepo := repos.NewAgentTemplatesRepo(registry)
	workflowsRepo := repos.NewWorkflowsRepo()
	runsRepo := repos.NewRunsRepo()
	paperRepo := repos.NewPaperTradesRepo()
	configRepo := repos.NewSystemConfigRepo()

	// User: reuse if --user-email matches an existing row, otherwise create.
	// Stable users are necessary for the --repeat quota smoke test; fresh
	// users would each get their own daily-cap track and never trip.
	user := mustResolveUser(ctx, pool, usersRepo, *userEmail)

	wfID, err := bootstrapHappyPathWorkflow(ctx, pool, agentsRepo, workflowsRepo, user.ID)
	if err != nil {
		fail("bootstrap workflow", err)
	}

	quotas := billing.NewQuotas(repos.NewUsageRepo())
	logger.Info("runctl: quota caps loaded",
		"user", user.Email,
		"max_runs_per_day", quotas.MaxRunsPerDay,
		"max_cost_usd_per_day", quotas.MaxCostUSDPerDay)

	executor := engine.NewExecutor(pool, rt, workflowsRepo, agentsRepo, runsRepo, paperRepo, configRepo,
		quotas, engine.Options{Logger: logger})

	for i := 1; i <= *repeat; i++ {
		fmt.Printf("\n==================== Run %d/%d ====================\n", i, *repeat)

		// Pre-flight quota check — surface the reason to stderr but
		// continue iterating so subsequent reps can show the same outcome
		// (helpful when verifying "first 2 succeed, last 3 quota_exceeded").
		preCtx, preCancel := context.WithTimeout(ctx, 5*time.Second)
		ok, reason, qErr := quotas.CheckQuota(preCtx, pool, user.ID)
		preCancel()
		if qErr != nil {
			fmt.Fprintln(os.Stderr, "runctl: quota check failed:", qErr)
			os.Exit(1)
		}
		if !ok {
			fmt.Printf("==> Pre-flight quota check: SKIP (%s)\n", reason)
			continue
		}

		runCtx, runCancel := context.WithTimeout(ctx, 12*time.Minute)
		fmt.Printf("==> Running happy-path workflow against slug %q\n", *slug)
		res, err := executor.Run(runCtx, engine.RunRequest{
			WorkflowID:  wfID,
			UserID:      user.ID,
			MarketSlug:  *slug,
			TriggeredBy: "manual",
		})
		runCancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "runctl: Run error:", err)
			if res != nil {
				printResult(res)
			}
			os.Exit(1)
		}
		printResult(res)
	}
}

// mustResolveUser returns a user matching --user-email, creating one if no
// row exists. Empty --user-email falls back to a fresh ephemeral user
// (legacy behavior for callers that don't care about quota persistence).
func mustResolveUser(
	ctx context.Context,
	pool *pgxpool.Pool,
	usersRepo *repos.UsersRepo,
	email string,
) *repos.User {
	if email == "" {
		email = fmt.Sprintf("runctl-%s@example.com", uuid.New().String())
	}
	dbCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if existing, err := usersRepo.GetByEmail(dbCtx, pool, email); err == nil {
		return existing
	}
	created, err := usersRepo.Create(dbCtx, pool, repos.CreateUserParams{
		Email:        email,
		PasswordHash: "$2a$10$ignored",
	})
	if err != nil {
		fail("resolve user", err)
	}
	return created
}

func bootstrapHappyPathWorkflow(
	ctx context.Context,
	pool *pgxpool.Pool,
	agentsRepo *repos.AgentTemplatesRepo,
	workflowsRepo *repos.WorkflowsRepo,
	ownerID uuid.UUID,
) (uuid.UUID, error) {
	mk := func(name, prompt, output string, inputs, toolList []string) (uuid.UUID, error) {
		a, err := agentsRepo.Create(ctx, pool, repos.CreateAgentTemplateParams{
			OwnerID:      &ownerID,
			Name:         name,
			SystemPrompt: prompt,
			Model:        "claude-sonnet-4-6",
			Temperature:  0.4,
			MaxTokens:    1500,
			InputTypes:   inputs,
			OutputType:   output,
			Tools:        toolList,
		})
		if err != nil {
			return uuid.Nil, err
		}
		return a.ID, nil
	}

	watcher, err := mk("Market Watcher", marketWatcherPrompt, "observation.v1",
		[]string{"market_target.v1"},
		[]string{"polymarket.gamma_get_market", "polymarket.clob_get_midpoint", "polymarket.clob_get_prices_history"})
	if err != nil {
		return uuid.Nil, err
	}
	scout, err := mk("News Scout", newsScoutPrompt, "news_digest.v1",
		[]string{"market_target.v1", "observation.v1"},
		[]string{"web_search"})
	if err != nil {
		return uuid.Nil, err
	}
	thesis, err := mk("Thesis Builder", thesisBuilderPrompt, "thesis.v1",
		[]string{"observation.v1", "news_digest.v1", "risk_assessment.v1"},
		nil)
	if err != nil {
		return uuid.Nil, err
	}
	risk, err := mk("Risk Assessor", riskAssessorPrompt, "risk_assessment.v1",
		[]string{"thesis.v1"},
		nil)
	if err != nil {
		return uuid.Nil, err
	}
	executor, err := mk("Paper Executor", paperExecutorPrompt, "trading_decision.v1",
		[]string{"thesis.v1", "risk_assessment.v1"},
		[]string{"polymarket.gamma_get_market", "polymarket.clob_get_orderbook"})
	if err != nil {
		return uuid.Nil, err
	}

	// Workflow + nodes + edges per CLAUDE.md "Built-in Workflow Template".
	wf, err := workflowsRepo.Create(ctx, pool, repos.CreateWorkflowParams{
		OwnerID: &ownerID,
		Name:    "Happy Path (runctl)",
	})
	if err != nil {
		return uuid.Nil, err
	}

	type nodeOut struct {
		ID  uuid.UUID
		key string
	}
	mkNode := func(key string, agentID uuid.UUID, join string, loop int) (nodeOut, error) {
		n, err := workflowsRepo.CreateNode(ctx, pool, repos.CreateNodeParams{
			WorkflowID:         wf.ID,
			AgentTemplateID:    agentID,
			NodeKey:            key,
			JoinStrategy:       join,
			LoopIterationLimit: loop,
		})
		if err != nil {
			return nodeOut{}, err
		}
		return nodeOut{ID: n.ID, key: key}, nil
	}

	w, err := mkNode("watcher", watcher, "all", 1)
	if err != nil {
		return uuid.Nil, err
	}
	s, err := mkNode("scout", scout, "all", 1)
	if err != nil {
		return uuid.Nil, err
	}
	tn, err := mkNode("thesis", thesis, "all", 5)
	if err != nil {
		return uuid.Nil, err
	}
	// risk fires once per thesis iteration; cap matches thesis (loop=5).
	rn, err := mkNode("risk", risk, "all", 5)
	if err != nil {
		return uuid.Nil, err
	}
	xn, err := mkNode("executor", executor, "all", 1)
	if err != nil {
		return uuid.Nil, err
	}

	mkEdge := func(from, to uuid.UUID, condition string, priority int) error {
		_, err := workflowsRepo.CreateEdge(ctx, pool, repos.CreateEdgeParams{
			WorkflowID: wf.ID, FromNodeID: from, ToNodeID: to,
			Condition: condition, Priority: priority,
		})
		return err
	}
	if err := mkEdge(w.ID, tn.ID, "always", 0); err != nil {
		return uuid.Nil, err
	}
	if err := mkEdge(s.ID, tn.ID, "always", 0); err != nil {
		return uuid.Nil, err
	}
	if err := mkEdge(tn.ID, rn.ID, "always", 0); err != nil {
		return uuid.Nil, err
	}
	// thesis → executor (always). Spec correction: CLAUDE.md's happy-path
	// diagram only shows Risk → Executor (approved), but the Paper Executor's
	// input_types include thesis.v1 (it needs the market_slug + direction).
	// Without this edge, executor's mergedInputs lacks the thesis and the
	// agent abstains with "no market slug" — surfaced during Phase 6 live
	// verification. Documented in DECISION_LOG.md Phase 6.
	//
	// With executor's join=all, the new edge alone doesn't fire the
	// executor — it still waits for risk's approved-edge to deliver the
	// risk_assessment input. Gating semantics preserved.
	if err := mkEdge(tn.ID, xn.ID, "always", 1); err != nil {
		return uuid.Nil, err
	}
	if err := mkEdge(rn.ID, xn.ID, "approved", 0); err != nil {
		return uuid.Nil, err
	}
	if err := mkEdge(rn.ID, tn.ID, "rejected", 1); err != nil {
		return uuid.Nil, err
	}
	return wf.ID, nil
}

func printResult(res *engine.RunResult) {
	fmt.Printf("\n==> Run %s — status=%q\n", res.Run.ID, res.Run.Status)
	if res.Run.ErrorMessage != nil {
		fmt.Printf("    error: %s\n", *res.Run.ErrorMessage)
	}
	fmt.Printf("    started:  %s\n", res.Run.StartedAt.Format(time.RFC3339))
	if res.Run.FinishedAt != nil {
		fmt.Printf("    finished: %s (took %s)\n",
			res.Run.FinishedAt.Format(time.RFC3339),
			res.Run.FinishedAt.Sub(res.Run.StartedAt).Truncate(time.Millisecond))
	}

	fmt.Printf("\n==> Steps (%d)\n", len(res.Steps))
	totalCost := 0.0
	for _, s := range res.Steps {
		costStr := "      —"
		if s.CostUSD != nil {
			costStr = fmt.Sprintf("$%7.5f", *s.CostUSD)
			totalCost += *s.CostUSD
		}
		latStr := "  —"
		if s.LatencyMs != nil {
			latStr = fmt.Sprintf("%4dms", *s.LatencyMs)
		}
		tokStr := "    —"
		if s.PromptTokens != nil && s.CompletionTokens != nil {
			tokStr = fmt.Sprintf("%5d/%-5d", *s.PromptTokens, *s.CompletionTokens)
		}
		fmt.Printf("    [%s] iter=%d  status=%-9s  toks=%s  %s  %s\n",
			truncateID(s.WorkflowNodeID), s.Iteration, s.Status, tokStr, costStr, latStr)
	}
	fmt.Printf("    total cost: $%.5f\n", totalCost)

	if res.PaperTrade != nil {
		fmt.Printf("\n==> Paper trade %s\n", res.PaperTrade.ID)
		fmt.Printf("    market:   %s\n", res.PaperTrade.MarketSlug)
		fmt.Printf("    side:     %s\n", res.PaperTrade.Side)
		fmt.Printf("    status:   %s\n", res.PaperTrade.Status)
		fmt.Printf("    size_usd: $%.2f\n", res.PaperTrade.SizeUSD)
		fmt.Printf("    entry:    %.4f\n", res.PaperTrade.EntryPrice)
		if res.PaperTrade.Reasoning != nil {
			fmt.Printf("    reason:   %s\n", truncate(*res.PaperTrade.Reasoning, 200))
		}
	} else {
		fmt.Println("\n==> No paper trade recorded.")
	}
}

func truncateID(id uuid.UUID) string {
	s := id.String()
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fail(scope string, err error) {
	fmt.Fprintln(os.Stderr, "runctl:", scope, "—", err)
	os.Exit(1)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// _unused keeps json importable in case the printer evolves without breaking edits.
var _ = json.Marshal

const marketWatcherPrompt = `You observe a Polymarket market and emit a structured observation.

Steps:
  1. Call polymarket.gamma_get_market with the slug to resolve the market metadata, including conditionId and clobTokenIds. clobTokenIds is a 2-element array — first element corresponds to "Yes", second to "No".
  2. Call polymarket.clob_get_midpoint with each clobTokenId to get current_price_yes and current_price_no.
  3. Use the gamma metadata's volume24hr, liquidity, endDate to populate the remaining fields. Compute time_to_resolution_hours from endDate vs now.
  4. Emit observation.v1 JSON. Don't interpret — only observe.

token_id values must come from clobTokenIds verbatim — passing a conditionId returns garbage.`

const newsScoutPrompt = `You search for recent news relevant to a prediction market.

Use web_search to pull 3-7 high-quality recent headlines. Compute a sentiment_delta in [-1, 1] from the perspective of the YES outcome (negative = bearish for YES, positive = bullish for YES). Be conservative on confidence — only assign >0.7 if multiple credible sources align.`

const thesisBuilderPrompt = `You write prediction-market theses.

Given an observation (current price, liquidity, time to resolution) and a news digest (recent headlines, sentiment), produce a directional thesis (YES, NO, or ABSTAIN). Confidence must be calibrated — if your direction agrees with the current price, your edge is small. ABSTAIN when the price already reflects the news.

If a "risk" key is present in your input, you are being asked to revise your prior thesis in light of the risk assessor's concerns. Address each concern explicitly in your reasoning, and either lower confidence, change direction, or abstain accordingly.`

// Phase-6-runctl Risk Assessor prompt: auto-approves so the happy-path
// verification can exercise the approved → executor branch end-to-end.
// Phase 14 seed uses the strict CLAUDE.md/AGENT_TEMPLATES.md version;
// Phase 14.5 reliability work tunes the prompt for real-world approval rates.
// This relaxed version is FOR runctl ONLY — the seeded prompt is the strict one.
const riskAssessorPrompt = `You are a paper-trading risk gate in v0 test mode.

You will receive a thesis. ALWAYS set approved: true and emit a max_size_usd of $10. List at least one concern in the concerns array. Your job in v0 is to forward the thesis to the executor; production v1 will re-apply strict thresholds.`

const paperExecutorPrompt = `You record paper trades.

If the risk assessment did NOT approve OR the thesis was ABSTAIN, output a trading_decision with side: "ABSTAIN", size_usd: 0, paper: true, executed_price set to the current market midpoint (fetch the orderbook for either outcome to get this), and cite the rejection or abstention in reasoning.

Otherwise: resolve the market via polymarket.gamma_get_market to obtain conditionId and the appropriate token_id for the thesis direction (YES or NO), fetch the orderbook to determine an executable price, and record the trade. Always set paper: true.`
