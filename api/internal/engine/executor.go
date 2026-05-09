package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/OldEphraim/sibyl-hub/api/internal/billing"
	"github.com/OldEphraim/sibyl-hub/api/internal/db"
	"github.com/OldEphraim/sibyl-hub/api/internal/db/repos"
	"github.com/OldEphraim/sibyl-hub/api/internal/runtime"
)

// AgentInvoker is the runtime surface the executor depends on. *runtime.Runtime
// satisfies it directly; tests pass a mock to avoid hitting Anthropic.
type AgentInvoker interface {
	Invoke(ctx context.Context, agent runtime.Agent, mergedInputs map[string]json.RawMessage) (*runtime.InvokeResult, error)
}

// Executor wires together all the dependencies the engine needs at run time.
// Build one at server startup; one Run per workflow invocation.
//
// Holds a db.Querier (not *pgxpool.Pool) so tests can substitute a
// transaction that gets rolled back at teardown — same isolation pattern
// as the repo tests in Phase 3.
type Executor struct {
	db            db.Querier
	invoker       AgentInvoker
	workflowsRepo *repos.WorkflowsRepo
	agentsRepo    *repos.AgentTemplatesRepo
	runsRepo      *repos.RunsRepo
	paperRepo     *repos.PaperTradesRepo
	configRepo    *repos.SystemConfigRepo
	quotas        *billing.Quotas // Phase 7 — nil disables quota enforcement (test path)
	logger        *slog.Logger

	// Caps — Options can override for tests.
	perStepTimeout time.Duration
	perRunTimeout  time.Duration
	maxStepsPerRun int

	// now is injectable for deterministic per-run-timeout tests.
	now func() time.Time
}

// Options configures NewExecutor. Zero values fall back to the
// CLAUDE.md / non-negotiable defaults (90s, 10min, 50). Tests inject smaller
// values to keep the matrix fast.
type Options struct {
	PerStepTimeout time.Duration
	PerRunTimeout  time.Duration
	MaxStepsPerRun int
	Now            func() time.Time
	Logger         *slog.Logger
}

// NewExecutor builds an Executor. invoker may NOT be nil — the engine has
// no work to do without an agent runtime. quotas may be nil for test paths
// that don't want to exercise the cost-cap machinery; production wiring
// always supplies one (see runctl).
func NewExecutor(
	q db.Querier,
	invoker AgentInvoker,
	workflowsRepo *repos.WorkflowsRepo,
	agentsRepo *repos.AgentTemplatesRepo,
	runsRepo *repos.RunsRepo,
	paperRepo *repos.PaperTradesRepo,
	configRepo *repos.SystemConfigRepo,
	quotas *billing.Quotas,
	opts Options,
) *Executor {
	if invoker == nil {
		panic("engine: NewExecutor: nil invoker")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Executor{
		db:             q,
		invoker:        invoker,
		workflowsRepo:  workflowsRepo,
		agentsRepo:     agentsRepo,
		runsRepo:       runsRepo,
		paperRepo:      paperRepo,
		configRepo:     configRepo,
		quotas:         quotas,
		logger:         opts.Logger,
		perStepTimeout: opts.PerStepTimeout,
		perRunTimeout:  opts.PerRunTimeout,
		maxStepsPerRun: opts.MaxStepsPerRun,
		now:            opts.Now,
	}
}

// RunRequest carries the bits a caller (handler in Phase 8, scheduler in
// Phase 7) supplies to spin up a run.
type RunRequest struct {
	WorkflowID  uuid.UUID
	UserID      uuid.UUID
	MarketSlug  string
	UserIntent  string // optional; spread into market_target.v1.user_intent
	TriggeredBy string // 'manual' | 'schedule'
}

// RunResult is what Run returns. The status mirrors workflow_runs.status;
// PaperTrade is non-nil if a terminal trading_decision.v1 produced a row.
type RunResult struct {
	Run        repos.WorkflowRun
	Steps      []repos.AgentStep
	PaperTrade *repos.PaperTrade // nil when the run never reached a terminal node OR the side was ABSTAIN before-Q14
}

// Run executes one workflow. Persists workflow_runs + agent_steps as it
// goes; writes a paper_trades row at terminal completion. Returns the
// final state. Does NOT enforce daily-cost or run-count quotas — that's
// Phase 7's job; the engine is intentionally policy-free.
//
// On any terminating event (success, validation failure, timeout, kill,
// step cap, loop cap), the workflow_run row's status is updated and an
// error_message is set when applicable.
func (e *Executor) Run(ctx context.Context, req RunRequest) (*RunResult, error) {
	// ----- Pre-flight: kill-switch on initial run start -------------------
	kill := newKillSwitchChecker(e.db, e.configRepo)
	if on, err := kill.on(ctx); err != nil {
		return nil, fmt.Errorf("engine: Run: kill_switch read: %w", err)
	} else if on {
		// Don't even create a workflow_runs row — surface to the handler.
		return nil, errors.New("engine: kill_switch is on; refusing to start run")
	}

	// ----- Build the in-memory graph --------------------------------------
	graph, err := NewGraph(ctx, e.db, e.workflowsRepo, e.agentsRepo, req.WorkflowID)
	if err != nil {
		return nil, err
	}

	snap, err := inputSnapshot(req.MarketSlug, req.UserIntent)
	if err != nil {
		return nil, err
	}

	// ----- Persist the workflow_runs row ----------------------------------
	marketSlug := req.MarketSlug
	run, err := e.runsRepo.Create(ctx, e.db, repos.CreateRunParams{
		WorkflowID:    req.WorkflowID,
		UserID:        req.UserID,
		TriggeredBy:   req.TriggeredBy,
		Status:        "running",
		MarketSlug:    &marketSlug,
		InputSnapshot: snap,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: Run: create workflow_run: %w", err)
	}
	startTime := e.now()
	deadline := runDeadline(startTime, e.perRunTimeout)

	e.logger.Info("engine: run started",
		"run_id", run.ID, "workflow_id", req.WorkflowID,
		"market_slug", req.MarketSlug, "entry_nodes", len(graph.EntryNodes))

	// Atomic run-count increment — CLAUDE.md "Cost Protection" mechanism #1.
	// Two concurrent runs by the same user can't both pass the pre-check
	// and overshoot the cap because the second INSERT serializes against
	// the first via the unique (user_id, day) index.
	if e.quotas != nil {
		newCount, ierr := e.quotas.IncrementRunCount(ctx, e.db, req.UserID)
		if ierr != nil {
			_ = e.finalizeRunStatus(ctx, run.ID, "failed", ierr.Error(), true)
			return nil, fmt.Errorf("engine: Run: increment run_count: %w", ierr)
		}
		if e.quotas.IsOverRunCount(newCount) {
			msg := fmt.Sprintf("daily run quota exceeded (%d), this run is #%d", e.quotas.MaxRunsPerDay, newCount)
			_ = e.finalizeRunStatus(ctx, run.ID, "quota_exceeded", msg, true)
			run.Status = "quota_exceeded"
			run.ErrorMessage = &msg
			return &RunResult{Run: *run}, nil
		}
	}

	// ----- Seed entry nodes and prime the ready queue ---------------------
	seed, err := seedEntryInputs(req.MarketSlug, req.UserIntent)
	if err != nil {
		_ = e.finalizeRunStatus(ctx, run.ID, "failed", err.Error(), true)
		return nil, err
	}
	seq := 0
	for _, entry := range graph.EntryNodes {
		// Spread seed fields into the entry node's "latest upstream" map.
		// Synthetic key "__entry__" so the freshness check sees a positive
		// sequence number; the runtime never displays this key directly
		// (mergeInputsForNode flattens it via the same upstream-key map).
		// Actually we inline-spread instead so the runtime sees the bare
		// market_target fields (see DECISION_LOG Phase 6).
		for k, v := range seed {
			seq++
			entry.LatestUpstreamOutputs[k] = upstreamOutput{Payload: v, Seq: seq}
		}
		entry.Pending = true
	}

	// ready holds nodes whose inputs are satisfied AND that haven't been
	// dispatched for their next iteration yet.
	ready := append([]*Node(nil), graph.EntryNodes...)

	maxSteps := e.maxStepsPerRun
	if maxSteps <= 0 {
		maxSteps = DefaultMaxStepsPerRun
	}

	steps := []repos.AgentStep{}
	stepCount := 0
	terminalStatus := "completed"
	terminalErrMsg := ""

	// Capture the entry market_target's question for paper_trades.market_question.
	// The terminal step's output gives us market_slug; we don't have the
	// question without a fetch, so leave it empty and let downstream
	// analytics handle. Phase 14+ can enrich.
	marketQuestion := ""

	var paperTrade *repos.PaperTrade

	// ----- Step loop ------------------------------------------------------
	for len(ready) > 0 {
		// Per-run cap (50 steps) — independent of the per-run wall clock.
		if stepCount >= maxSteps {
			terminalStatus = "failed"
			terminalErrMsg = fmt.Sprintf("engine: per-run step cap (%d) reached", maxSteps)
			break
		}
		// Per-run wall-clock timeout.
		if e.now().After(deadline) {
			terminalStatus = "timed_out"
			terminalErrMsg = fmt.Sprintf("engine: per-run timeout (%s) exceeded", deadline.Sub(startTime))
			break
		}
		// Mid-run kill-switch check.
		if on, err := kill.on(ctx); err != nil {
			terminalStatus = "failed"
			terminalErrMsg = fmt.Sprintf("engine: kill_switch read: %v", err)
			break
		} else if on {
			terminalStatus = "killed"
			terminalErrMsg = "engine: kill_switch flipped on mid-run"
			break
		}

		node := ready[0]
		ready = ready[1:]
		node.Pending = false

		nextIteration := node.Iteration + 1
		if nextIteration > node.WorkflowNode.LoopIterationLimit {
			terminalStatus = "failed"
			terminalErrMsg = fmt.Sprintf("engine: loop iteration limit reached at node %q", node.WorkflowNode.NodeKey)
			break
		}

		merged := mergeInputsForNode(node)
		stepCount++

		// Persist a 'running' step row so the run-detail endpoint can
		// observe in-flight progress in Phase 8/13. No tx — Phase 7 will
		// wrap the finalize in a tx alongside the cost-cap upsert.
		inputJSON, _ := json.Marshal(merged)
		stepRow, err := e.runsRepo.AddStep(ctx, e.db, repos.AddStepParams{
			WorkflowRunID:  run.ID,
			WorkflowNodeID: node.WorkflowNode.ID,
			Iteration:      nextIteration,
			Status:         "running",
			InputData:      inputJSON,
		})
		if err != nil {
			terminalStatus = "failed"
			terminalErrMsg = fmt.Sprintf("engine: persist step start: %v", err)
			break
		}

		// Per-step timeout via context.WithTimeout. The cancel func MUST be
		// deferred per iteration — we use a closure to scope it.
		stepResult, stepErr := func() (*runtime.InvokeResult, error) {
			stepCtx, cancel := stepContext(ctx, e.perStepTimeout)
			defer cancel()
			return e.invoker.Invoke(stepCtx, node.AsRuntimeAgent(), merged)
		}()

		// Persist the step finalization. Tokens/cost are recorded even on
		// failure — already-paid-for tokens don't roll back (CLAUDE.md
		// "Cost protection" mechanism #10).
		var (
			finalStatus   = "completed"
			finalErr      *string
			outputData    json.RawMessage
			promptTokens  *int
			completionTok *int
			costUSD       *float64
			latencyMS     *int
		)
		if stepResult != nil {
			outputData = stepResult.Output
			t := stepResult.PromptTokens
			promptTokens = &t
			ct := stepResult.CompletionTokens
			completionTok = &ct
			c := stepResult.CostUSD
			costUSD = &c
			lm := stepResult.LatencyMS
			latencyMS = &lm
		}
		if stepErr != nil {
			finalStatus = classifyStepError(stepErr)
			msg := stepErr.Error()
			finalErr = &msg
		}

		if updErr := e.runsRepo.UpdateStepCompleted(ctx, e.db, repos.UpdateStepCompletedParams{
			StepID:           stepRow.ID,
			Status:           finalStatus,
			OutputData:       outputData,
			PromptTokens:     promptTokens,
			CompletionTokens: completionTok,
			CostUSD:          costUSD,
			LatencyMs:        latencyMS,
			ErrorMessage:     finalErr,
		}); updErr != nil {
			terminalStatus = "failed"
			terminalErrMsg = fmt.Sprintf("engine: persist step completion: %v", updErr)
			break
		}

		stepRow.Status = finalStatus
		stepRow.OutputData = outputData
		stepRow.PromptTokens = promptTokens
		stepRow.CompletionTokens = completionTok
		stepRow.CostUSD = costUSD
		stepRow.LatencyMs = latencyMS
		stepRow.ErrorMessage = finalErr
		steps = append(steps, *stepRow)

		// Atomic cost increment — CLAUDE.md "Cost Protection" mechanism #10.
		// Done sequentially after UpdateStepCompleted (rather than in the
		// same transaction). The agent_steps row already captures cost data,
		// so a failed RecordStepCost is recoverable in v1+ via reconciliation.
		// Marked as a Phase 8 cleanup in DECISION_LOG.
		if e.quotas != nil && stepResult != nil && (stepResult.PromptTokens > 0 || stepResult.CompletionTokens > 0) {
			tokens := int64(stepResult.PromptTokens + stepResult.CompletionTokens)
			newTotal, rerr := e.quotas.RecordStepCost(ctx, e.db, req.UserID, stepResult.CostUSD, tokens)
			if rerr != nil {
				// Log but don't fail the run — the agent_steps row has the
				// cost data, so reconciliation is possible later.
				e.logger.Error("engine: record step cost failed (run continues)",
					"run_id", run.ID, "step_id", stepRow.ID, "error", rerr)
			} else if e.quotas.IsOverCostUSD(newTotal) {
				terminalStatus = "quota_exceeded"
				terminalErrMsg = fmt.Sprintf(
					"daily cost cap exceeded ($%.2f), current $%.4f",
					e.quotas.MaxCostUSDPerDay, newTotal)
				break
			}
		}

		// Step error → terminate the run.
		if stepErr != nil {
			terminalStatus = finalStatus
			terminalErrMsg = stepErr.Error()
			break
		}

		// ----- Step succeeded — update node state ------------------------
		node.Iteration = nextIteration
		node.LastFiredSeq = seq
		node.LastOutput = outputData

		// Terminal trading_decision.v1 → write paper_trade.
		if node.IsTerminal() && len(outputData) > 0 {
			pt, ptErr := recordPaperTrade(ctx, e.db, e.paperRepo,
				run.ID, req.UserID, marketQuestion, outputData)
			if ptErr != nil {
				// A paper_trade write failure shouldn't fail the run — the
				// underlying agent_steps row already captures the decision.
				// Log and proceed.
				e.logger.Error("engine: record paper_trade failed",
					"run_id", run.ID, "error", ptErr)
			} else {
				paperTrade = pt
				e.logger.Info("engine: paper_trade recorded",
					"run_id", run.ID, "side", pt.Side,
					"size_usd", pt.SizeUSD, "status", pt.Status)
			}
		}

		// ----- Edge propagation ------------------------------------------
		fire := edgeFiringPlan(node.Outgoing, outputData)
		for _, edge := range fire {
			seq++
			edge.To.LatestUpstreamOutputs[node.WorkflowNode.NodeKey] = upstreamOutput{
				Payload: outputData,
				Seq:     seq,
			}
			if edge.To.Pending {
				continue // already on the queue
			}
			if isReady(edge.To) {
				edge.To.Pending = true
				ready = append(ready, edge.To)
			}
		}
	}

	// ----- Finalize the run row ------------------------------------------
	if err := e.finalizeRunStatus(ctx, run.ID, terminalStatus, terminalErrMsg, true); err != nil {
		// Best-effort — return the original outcome to the caller anyway.
		e.logger.Error("engine: finalize run status", "run_id", run.ID, "error", err)
	}
	run.Status = terminalStatus
	if terminalErrMsg != "" {
		run.ErrorMessage = &terminalErrMsg
	}

	return &RunResult{
		Run:        *run,
		Steps:      steps,
		PaperTrade: paperTrade,
	}, nil
}

// finalizeRunStatus persists the terminal status + error_message to the
// workflow_runs row. Always called once per Run, even on success.
func (e *Executor) finalizeRunStatus(ctx context.Context, runID uuid.UUID, status, errMsg string, finished bool) error {
	var em *string
	if errMsg != "" {
		em = &errMsg
	}
	return e.runsRepo.UpdateRunStatus(ctx, e.db, runID, status, em, finished)
}

// isReady applies CLAUDE.md "Ready queue" rules:
//
//	all  : every NON-LOOP upstream node has produced ≥1 output (initial fire)
//	       OR a new output arrived from any upstream since last firing (loop iters)
//	first: at least one upstream has produced output since last firing
//
// The sequence-number gate ("max output seq > LastFiredSeq") handles the
// "since last firing" half for both strategies; the all-vs-first split only
// matters for the iteration-1 readiness check.
//
// Per CLAUDE.md "Loop semantics" worked example: Thesis Builder iter 1 is
// triggered by Watcher + Scout (the non-loop upstreams). The Risk → Thesis
// loop edge does NOT count for iter 1 — Risk hasn't fired yet. NonLoopUpstreamKeys
// is computed at graph build via DFS back-edge detection (graph.go).
func isReady(n *Node) bool {
	if len(n.LatestUpstreamOutputs) == 0 {
		return false
	}
	maxSeq := 0
	for _, v := range n.LatestUpstreamOutputs {
		if v.Seq > maxSeq {
			maxSeq = v.Seq
		}
	}
	if maxSeq <= n.LastFiredSeq {
		return false // nothing new since last firing
	}

	switch n.WorkflowNode.JoinStrategy {
	case "first":
		return true
	case "all", "":
		required := n.NonLoopUpstreamKeys
		if len(required) == 0 {
			// Entry-node-like fallback: no incoming non-loop edges, e.g. an
			// engine-seeded entry node has its market_target.v1 in inputs.
			// Fire on any new output.
			return true
		}
		// Every non-loop upstream must have produced at least one output.
		for k := range required {
			if _, ok := n.LatestUpstreamOutputs[k]; !ok {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// classifyStepError maps a runtime error to the agent_steps.status enum:
//
//	context.DeadlineExceeded → 'timed_out'
//	otherwise               → 'failed'
func classifyStepError(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed_out"
	}
	return "failed"
}
