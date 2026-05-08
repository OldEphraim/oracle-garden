package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OldEphraim/sibyl-hub/api/internal/db/repos"
	"github.com/OldEphraim/sibyl-hub/api/internal/runtime"
	"github.com/OldEphraim/sibyl-hub/api/internal/types"
)

// ---------- Test setup -----------------------------------------------------

var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		dsn := firstNonEmpty(os.Getenv("TEST_DATABASE_URL"), os.Getenv("DATABASE_URL"))
		if dsn == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return
		}
		if err := p.Ping(ctx); err != nil {
			p.Close()
			return
		}
		pool = p
	})
	if pool == nil {
		t.Skip("no TEST_DATABASE_URL or DATABASE_URL — skipping engine tests")
	}
	return pool
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// withTx runs fn in a transaction that's always rolled back, plus the
// fixtures (test user, agents, workflow rows, executor) it asks for.
func withTx(t *testing.T, fn func(t *testing.T, fix *fixtures)) {
	t.Helper()
	p := testPool(t)
	ctx := context.Background()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	fix := newFixtures(t, ctx, tx)
	fn(t, fix)
}

// fixtures bundles the per-test helpers the engine needs.
type fixtures struct {
	ctx           context.Context
	tx            pgx.Tx
	registry      *types.Registry
	workflowsRepo *repos.WorkflowsRepo
	agentsRepo    *repos.AgentTemplatesRepo
	runsRepo      *repos.RunsRepo
	paperRepo     *repos.PaperTradesRepo
	configRepo    *repos.SystemConfigRepo
	userID        uuid.UUID
}

func newFixtures(t *testing.T, ctx context.Context, tx pgx.Tx) *fixtures {
	t.Helper()
	registry, err := types.Load(ctx, tx)
	if err != nil {
		t.Fatalf("types.Load: %v", err)
	}
	usersRepo := repos.NewUsersRepo()
	u, err := usersRepo.Create(ctx, tx, repos.CreateUserParams{
		Email:        fmt.Sprintf("test-%s@example.com", uuid.New().String()),
		PasswordHash: "$2a$10$ignored",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return &fixtures{
		ctx:           ctx,
		tx:            tx,
		registry:      registry,
		workflowsRepo: repos.NewWorkflowsRepo(),
		agentsRepo:    repos.NewAgentTemplatesRepo(registry),
		runsRepo:      repos.NewRunsRepo(),
		paperRepo:     repos.NewPaperTradesRepo(),
		configRepo:    repos.NewSystemConfigRepo(),
		userID:        u.ID,
	}
}

// agentTemplate inserts a minimal agent_template owned by fix.userID.
func (f *fixtures) agentTemplate(t *testing.T, name, output string, inputs []string) repos.AgentTemplate {
	t.Helper()
	a, err := f.agentsRepo.Create(f.ctx, f.tx, repos.CreateAgentTemplateParams{
		OwnerID: &f.userID, Name: name, SystemPrompt: "x",
		InputTypes: inputs, OutputType: output,
	})
	if err != nil {
		t.Fatalf("agent %s: %v", name, err)
	}
	return *a
}

// workflowAndNodes inserts a workflow + nodes from a small spec map.
// Returns the workflow id and a map[node_key → node_id] for wiring edges.
func (f *fixtures) workflowAndNodes(t *testing.T, nodes []nodeSpec) (uuid.UUID, map[string]uuid.UUID) {
	t.Helper()
	wf, err := f.workflowsRepo.Create(f.ctx, f.tx, repos.CreateWorkflowParams{
		OwnerID: &f.userID, Name: "T",
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	out := make(map[string]uuid.UUID, len(nodes))
	for _, n := range nodes {
		nodeRow, err := f.workflowsRepo.CreateNode(f.ctx, f.tx, repos.CreateNodeParams{
			WorkflowID:         wf.ID,
			AgentTemplateID:    n.agentID,
			NodeKey:            n.key,
			JoinStrategy:       n.join,
			LoopIterationLimit: n.loopLimit,
		})
		if err != nil {
			t.Fatalf("create node %s: %v", n.key, err)
		}
		out[n.key] = nodeRow.ID
	}
	return wf.ID, out
}

func (f *fixtures) edge(t *testing.T, wfID, from, to uuid.UUID, condition string, priority int) {
	t.Helper()
	_, err := f.workflowsRepo.CreateEdge(f.ctx, f.tx, repos.CreateEdgeParams{
		WorkflowID: wfID, FromNodeID: from, ToNodeID: to,
		Condition: condition, Priority: priority,
	})
	if err != nil {
		t.Fatalf("create edge %s→%s (%s): %v", from, to, condition, err)
	}
}

type nodeSpec struct {
	key       string
	agentID   uuid.UUID
	join      string // "all" | "first" | ""
	loopLimit int    // 0 = use repo default
}

// ---------- Mock invoker ---------------------------------------------------

// mockInvoker scripts agent responses by node-key. Each invocation pops the
// next response from a per-node-key list. Counter tracks total calls (across
// all node keys).
type mockInvoker struct {
	mu        sync.Mutex
	scripts   map[string][]invocationResult
	calls     int32
	delay     time.Duration // optional sleep before each Invoke (for timeout tests)
	onInvoke  func(nodeKey string, iteration int)
}

type invocationResult struct {
	output    json.RawMessage
	err       error
	latencyMS int
}

func newMockInvoker() *mockInvoker {
	return &mockInvoker{scripts: make(map[string][]invocationResult)}
}

// queue appends responses for a node key, looked up by the agent name's
// "[node_key]" suffix that engine.AsRuntimeAgent() uses.
func (m *mockInvoker) queue(nodeKey string, results ...invocationResult) {
	m.scripts[nodeKey] = append(m.scripts[nodeKey], results...)
}

func (m *mockInvoker) Invoke(ctx context.Context, agent runtime.Agent, _ map[string]json.RawMessage) (*runtime.InvokeResult, error) {
	atomic.AddInt32(&m.calls, 1)
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return &runtime.InvokeResult{Model: "mock", PromptTokens: 1, CompletionTokens: 0}, ctx.Err()
		}
	}
	// Decode the node_key from the agent.Name suffix "[key]".
	nodeKey := nodeKeyFromAgentName(agent.Name)
	if m.onInvoke != nil {
		m.onInvoke(nodeKey, 0)
	}
	m.mu.Lock()
	queue := m.scripts[nodeKey]
	if len(queue) == 0 {
		m.mu.Unlock()
		return &runtime.InvokeResult{Model: "mock"}, fmt.Errorf("mockInvoker: no script for node %q (agent %q)", nodeKey, agent.Name)
	}
	r := queue[0]
	m.scripts[nodeKey] = queue[1:]
	m.mu.Unlock()
	if r.err != nil {
		return &runtime.InvokeResult{Model: "mock", PromptTokens: 1, CompletionTokens: 1, LatencyMS: r.latencyMS}, r.err
	}
	return &runtime.InvokeResult{
		Output:           r.output,
		Model:            "mock",
		PromptTokens:     1,
		CompletionTokens: 1,
		LatencyMS:        r.latencyMS,
		Attempts:         1,
		StopReason:       "end_turn",
	}, nil
}

func nodeKeyFromAgentName(name string) string {
	i := strings.LastIndex(name, "[")
	j := strings.LastIndex(name, "]")
	if i < 0 || j <= i {
		return ""
	}
	return name[i+1 : j]
}

// ---------- Helpers --------------------------------------------------------

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func makeExecutor(f *fixtures, mock *mockInvoker, opts Options) *Executor {
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	return NewExecutor(f.tx, mock, f.workflowsRepo, f.agentsRepo, f.runsRepo, f.paperRepo, f.configRepo, opts)
}

// validObservation returns a payload that conforms to observation.v1.
func validObservation(slug string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"market_slug":%q, "condition_id":"0xabc", "question":"q",
		"current_price_yes":0.5, "current_price_no":0.5,
		"volume_24h_usd":100, "liquidity_usd":50,
		"time_to_resolution_hours":24,
		"yes_token_id":"y","no_token_id":"n"
	}`, slug))
}

// validNewsDigest returns a payload that conforms to news_digest.v1.
func validNewsDigest(slug string) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"market_slug":%q, "headlines":[],
		"sentiment_delta":0.0, "confidence":0.5
	}`, slug))
}

// validThesis returns a payload that conforms to thesis.v1.
func validThesis(slug, direction string, conf float64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"market_slug":%q, "direction":%q,
		"confidence":%v, "reasoning":"r"
	}`, slug, direction, conf))
}

// validRiskApproved / validRiskRejected.
func validRiskAssessment(approved bool) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"approved":%v, "max_size_usd":10, "reasoning":"r"
	}`, approved))
}

// validTradingDecision returns trading_decision.v1.
func validTradingDecision(slug, side string, sizeUSD, executedPrice float64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"market_slug":%q, "side":%q, "size_usd":%v,
		"executed_price":%v, "paper":true, "reasoning":"r"
	}`, slug, side, sizeUSD, executedPrice))
}

// ---------- Tests ----------------------------------------------------------

// TestLinearFlow: A → B → C (terminal).
func TestLinearFlow(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		watcher := f.agentTemplate(t, "Watcher", "observation.v1", []string{"market_target.v1"})
		thesis := f.agentTemplate(t, "Thesis", "thesis.v1", []string{"observation.v1"})
		executor := f.agentTemplate(t, "Executor", "trading_decision.v1", []string{"thesis.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "watcher", agentID: watcher.ID},
			{key: "thesis", agentID: thesis.ID},
			{key: "executor", agentID: executor.ID},
		})
		f.edge(t, wf, ids["watcher"], ids["thesis"], "always", 0)
		f.edge(t, wf, ids["thesis"], ids["executor"], "always", 0)

		mock := newMockInvoker()
		mock.queue("watcher", invocationResult{output: validObservation("x")})
		mock.queue("thesis", invocationResult{output: validThesis("x", "YES", 0.7)})
		mock.queue("executor", invocationResult{output: validTradingDecision("x", "YES", 10, 0.55)})

		ex := makeExecutor(f, mock, Options{})
		res, err := ex.Run(f.ctx, RunRequest{
			WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual",
		})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Run.Status != "completed" {
			t.Errorf("status: %q", res.Run.Status)
		}
		if len(res.Steps) != 3 {
			t.Errorf("steps: got %d want 3", len(res.Steps))
		}
		if res.PaperTrade == nil {
			t.Fatalf("expected paper_trade row")
		}
		if res.PaperTrade.Status != "open" || res.PaperTrade.Side != "YES" {
			t.Errorf("paper_trade: %+v", res.PaperTrade)
		}
	})
}

// TestFanInAll: Watcher + Scout → Thesis (join_strategy=all).
func TestFanInAll(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		watcher := f.agentTemplate(t, "Watcher", "observation.v1", []string{"market_target.v1"})
		scout := f.agentTemplate(t, "Scout", "news_digest.v1", []string{"market_target.v1"})
		thesis := f.agentTemplate(t, "Thesis", "thesis.v1", []string{"observation.v1", "news_digest.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "watcher", agentID: watcher.ID},
			{key: "scout", agentID: scout.ID},
			{key: "thesis", agentID: thesis.ID, join: "all"},
		})
		f.edge(t, wf, ids["watcher"], ids["thesis"], "always", 0)
		f.edge(t, wf, ids["scout"], ids["thesis"], "always", 0)

		mock := newMockInvoker()
		mock.queue("watcher", invocationResult{output: validObservation("x")})
		mock.queue("scout", invocationResult{output: validNewsDigest("x")})
		mock.queue("thesis", invocationResult{output: validThesis("x", "YES", 0.7)})

		ex := makeExecutor(f, mock, Options{})
		res, err := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Run.Status != "completed" {
			t.Errorf("status: %q", res.Run.Status)
		}
		// Thesis must fire AFTER both watcher and scout (positional check).
		var watcherIdx, scoutIdx, thesisIdx int = -1, -1, -1
		for i, s := range res.Steps {
			switch s.WorkflowNodeID {
			case ids["watcher"]:
				watcherIdx = i
			case ids["scout"]:
				scoutIdx = i
			case ids["thesis"]:
				thesisIdx = i
			}
		}
		if watcherIdx < 0 || scoutIdx < 0 || thesisIdx < 0 {
			t.Fatalf("missing step: watcher=%d scout=%d thesis=%d", watcherIdx, scoutIdx, thesisIdx)
		}
		if thesisIdx < watcherIdx || thesisIdx < scoutIdx {
			t.Errorf("thesis fired before all upstreams: order=%v", []int{watcherIdx, scoutIdx, thesisIdx})
		}
	})
}

// TestFanInFirst: a single entry fans out to two paths that converge on c
// with join_strategy=first. The direct path (a→c) lets c fire on iter 1 with
// only a's output present; the indirect path (a→b→c) then triggers c again
// on iter 2 once b's output arrives.
//
// Why this shape rather than two-entries-into-c: a FIFO ready queue with
// two entry nodes would seed and propagate both before c is popped, so c
// would only fire once. The fan-out + chain structure forces c to be popped
// (and fire) BEFORE b's output reaches it, exercising the "first fires per
// new upstream output" semantics.
func TestFanInFirst(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		a := f.agentTemplate(t, "A", "observation.v1", []string{"market_target.v1"})
		b := f.agentTemplate(t, "B", "news_digest.v1", []string{"observation.v1"})
		c := f.agentTemplate(t, "C", "thesis.v1", []string{"observation.v1", "news_digest.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "a", agentID: a.ID},
			{key: "b", agentID: b.ID},
			{key: "c", agentID: c.ID, join: "first"},
		})
		// Fan-out: a → c AND a → b. Then b → c.
		f.edge(t, wf, ids["a"], ids["c"], "always", 0)
		f.edge(t, wf, ids["a"], ids["b"], "always", 1)
		f.edge(t, wf, ids["b"], ids["c"], "always", 0)

		mock := newMockInvoker()
		mock.queue("a", invocationResult{output: validObservation("x")})
		mock.queue("b", invocationResult{output: validNewsDigest("x")})
		mock.queue("c",
			invocationResult{output: validThesis("x", "YES", 0.5)},
			invocationResult{output: validThesis("x", "YES", 0.6)})

		ex := makeExecutor(f, mock, Options{})
		res, err := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Run.Status != "completed" {
			t.Errorf("status: %q", res.Run.Status)
		}
		// Expect 4 steps: a, c (iter 1), b, c (iter 2).
		if len(res.Steps) != 4 {
			t.Errorf("steps: got %d want 4 (a, c1, b, c2)", len(res.Steps))
		}
		cFires := 0
		var cIters []int
		for _, s := range res.Steps {
			if s.WorkflowNodeID == ids["c"] {
				cFires++
				cIters = append(cIters, s.Iteration)
			}
		}
		if cFires != 2 {
			t.Errorf("c firings: got %d want 2", cFires)
		}
		if len(cIters) >= 2 && (cIters[0] != 1 || cIters[1] != 2) {
			t.Errorf("c iterations: got %v want [1 2]", cIters)
		}
	})
}

// TestFanOut: A has two `always` outgoing edges → both targets fire.
func TestFanOut(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		a := f.agentTemplate(t, "A", "observation.v1", []string{"market_target.v1"})
		b := f.agentTemplate(t, "B", "thesis.v1", []string{"observation.v1"})
		c := f.agentTemplate(t, "C", "news_digest.v1", []string{"observation.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "a", agentID: a.ID},
			{key: "b", agentID: b.ID},
			{key: "c", agentID: c.ID},
		})
		f.edge(t, wf, ids["a"], ids["b"], "always", 0)
		f.edge(t, wf, ids["a"], ids["c"], "always", 1)

		mock := newMockInvoker()
		mock.queue("a", invocationResult{output: validObservation("x")})
		mock.queue("b", invocationResult{output: validThesis("x", "YES", 0.5)})
		mock.queue("c", invocationResult{output: validNewsDigest("x")})

		ex := makeExecutor(f, mock, Options{})
		res, _ := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if res.Run.Status != "completed" {
			t.Errorf("status: %q", res.Run.Status)
		}
		if len(res.Steps) != 3 {
			t.Errorf("steps: got %d want 3", len(res.Steps))
		}
	})
}

// TestBranching: Risk → Executor (approved) | Risk → Watcher-loop (rejected).
// Mutually exclusive conditional edges: only the first match fires.
func TestBranching(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		watcher := f.agentTemplate(t, "Watcher", "observation.v1", []string{"market_target.v1"})
		risk := f.agentTemplate(t, "Risk", "risk_assessment.v1", []string{"observation.v1"})
		executor := f.agentTemplate(t, "Executor", "trading_decision.v1", []string{"risk_assessment.v1"})
		fallback := f.agentTemplate(t, "Fallback", "trading_decision.v1", []string{"risk_assessment.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "watcher", agentID: watcher.ID},
			{key: "risk", agentID: risk.ID},
			{key: "executor", agentID: executor.ID},
			{key: "fallback", agentID: fallback.ID},
		})
		f.edge(t, wf, ids["watcher"], ids["risk"], "always", 0)
		f.edge(t, wf, ids["risk"], ids["executor"], "approved", 0)
		f.edge(t, wf, ids["risk"], ids["fallback"], "rejected", 1)

		mock := newMockInvoker()
		mock.queue("watcher", invocationResult{output: validObservation("x")})
		mock.queue("risk", invocationResult{output: validRiskAssessment(true)})
		mock.queue("executor", invocationResult{output: validTradingDecision("x", "YES", 10, 0.55)})
		// fallback should NOT fire — it's the rejected branch.

		ex := makeExecutor(f, mock, Options{})
		res, _ := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if res.Run.Status != "completed" {
			t.Errorf("status: %q", res.Run.Status)
		}
		// 3 steps: watcher, risk, executor. fallback skipped.
		if len(res.Steps) != 3 {
			t.Errorf("steps: got %d want 3", len(res.Steps))
		}
		for _, s := range res.Steps {
			if s.WorkflowNodeID == ids["fallback"] {
				t.Errorf("fallback fired but rejected branch shouldn't have")
			}
		}
	})
}

// TestMixedFanOutAndBranching: CLAUDE.md worked example.
// Risk has: A=always (priority 0, fan-out logger),
//          B=approved (priority 1),
//          C=rejected (priority 2).
// approved=true → A fires AND B fires; C suppressed.
func TestMixedFanOutAndBranching(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		watcher := f.agentTemplate(t, "Watcher", "observation.v1", []string{"market_target.v1"})
		risk := f.agentTemplate(t, "Risk", "risk_assessment.v1", []string{"observation.v1"})
		logger := f.agentTemplate(t, "Logger", "trading_decision.v1", []string{"risk_assessment.v1"})
		executor := f.agentTemplate(t, "Executor", "trading_decision.v1", []string{"risk_assessment.v1"})
		fallback := f.agentTemplate(t, "Fallback", "trading_decision.v1", []string{"risk_assessment.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "watcher", agentID: watcher.ID},
			{key: "risk", agentID: risk.ID},
			{key: "logger", agentID: logger.ID},
			{key: "executor", agentID: executor.ID},
			{key: "fallback", agentID: fallback.ID},
		})
		f.edge(t, wf, ids["watcher"], ids["risk"], "always", 0)
		f.edge(t, wf, ids["risk"], ids["logger"], "always", 0)    // A
		f.edge(t, wf, ids["risk"], ids["executor"], "approved", 1) // B
		f.edge(t, wf, ids["risk"], ids["fallback"], "rejected", 2) // C

		mock := newMockInvoker()
		mock.queue("watcher", invocationResult{output: validObservation("x")})
		mock.queue("risk", invocationResult{output: validRiskAssessment(true)})
		mock.queue("logger", invocationResult{output: validTradingDecision("x", "ABSTAIN", 0, 0.5)})
		mock.queue("executor", invocationResult{output: validTradingDecision("x", "YES", 10, 0.55)})

		ex := makeExecutor(f, mock, Options{})
		res, _ := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if res.Run.Status != "completed" {
			t.Errorf("status: %q", res.Run.Status)
		}
		fired := map[uuid.UUID]bool{}
		for _, s := range res.Steps {
			fired[s.WorkflowNodeID] = true
		}
		if !fired[ids["logger"]] {
			t.Errorf("logger (always edge) should have fired")
		}
		if !fired[ids["executor"]] {
			t.Errorf("executor (approved match) should have fired")
		}
		if fired[ids["fallback"]] {
			t.Errorf("fallback (rejected) should NOT have fired when approved matched")
		}
	})
}

// TestLoop: Risk rejects → Thesis loops → Risk approves → Executor.
func TestLoop(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		watcher := f.agentTemplate(t, "Watcher", "observation.v1", []string{"market_target.v1"})
		thesis := f.agentTemplate(t, "Thesis", "thesis.v1", []string{"observation.v1", "risk_assessment.v1"})
		risk := f.agentTemplate(t, "Risk", "risk_assessment.v1", []string{"thesis.v1"})
		exec := f.agentTemplate(t, "Executor", "trading_decision.v1", []string{"risk_assessment.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "watcher", agentID: watcher.ID},
			{key: "thesis", agentID: thesis.ID, join: "all", loopLimit: 5},
			{key: "risk", agentID: risk.ID},
			{key: "executor", agentID: exec.ID},
		})
		f.edge(t, wf, ids["watcher"], ids["thesis"], "always", 0)
		f.edge(t, wf, ids["thesis"], ids["risk"], "always", 0)
		f.edge(t, wf, ids["risk"], ids["executor"], "approved", 0)
		f.edge(t, wf, ids["risk"], ids["thesis"], "rejected", 1)

		mock := newMockInvoker()
		mock.queue("watcher", invocationResult{output: validObservation("x")})
		// Iteration 1: thesis fires; risk rejects; thesis loops; risk approves.
		mock.queue("thesis",
			invocationResult{output: validThesis("x", "YES", 0.5)},
			invocationResult{output: validThesis("x", "YES", 0.7)})
		mock.queue("risk",
			invocationResult{output: validRiskAssessment(false)},
			invocationResult{output: validRiskAssessment(true)})
		mock.queue("executor", invocationResult{output: validTradingDecision("x", "YES", 10, 0.55)})

		ex := makeExecutor(f, mock, Options{})
		res, err := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Run.Status != "completed" {
			t.Errorf("status: %q error=%v", res.Run.Status, res.Run.ErrorMessage)
		}
		// Thesis should fire twice (iter 1 and iter 2 after rejection).
		thesisFires := 0
		thesisIters := []int{}
		for _, s := range res.Steps {
			if s.WorkflowNodeID == ids["thesis"] {
				thesisFires++
				thesisIters = append(thesisIters, s.Iteration)
			}
		}
		if thesisFires != 2 {
			t.Errorf("thesis fires: got %d want 2 (iters=%v)", thesisFires, thesisIters)
		}
		if len(thesisIters) >= 2 && thesisIters[1] != 2 {
			t.Errorf("second thesis iteration: got %d want 2", thesisIters[1])
		}
	})
}

// TestLoopIterationLimit: loop runs forever (Risk always rejects). Run fails
// when Thesis would exceed loop_iteration_limit.
func TestLoopIterationLimit(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		watcher := f.agentTemplate(t, "Watcher", "observation.v1", []string{"market_target.v1"})
		thesis := f.agentTemplate(t, "Thesis", "thesis.v1", []string{"observation.v1", "risk_assessment.v1"})
		risk := f.agentTemplate(t, "Risk", "risk_assessment.v1", []string{"thesis.v1"})
		exec := f.agentTemplate(t, "Executor", "trading_decision.v1", []string{"risk_assessment.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "watcher", agentID: watcher.ID},
			{key: "thesis", agentID: thesis.ID, join: "all", loopLimit: 3},
			{key: "risk", agentID: risk.ID},
			{key: "executor", agentID: exec.ID},
		})
		f.edge(t, wf, ids["watcher"], ids["thesis"], "always", 0)
		f.edge(t, wf, ids["thesis"], ids["risk"], "always", 0)
		f.edge(t, wf, ids["risk"], ids["executor"], "approved", 0)
		f.edge(t, wf, ids["risk"], ids["thesis"], "rejected", 1)

		mock := newMockInvoker()
		mock.queue("watcher", invocationResult{output: validObservation("x")})
		// Thesis: 3 iterations max → 3 successful firings before the 4th
		// would exceed the limit.
		for i := 0; i < 5; i++ {
			mock.queue("thesis", invocationResult{output: validThesis("x", "YES", 0.5)})
		}
		// Risk always rejects.
		for i := 0; i < 5; i++ {
			mock.queue("risk", invocationResult{output: validRiskAssessment(false)})
		}

		ex := makeExecutor(f, mock, Options{})
		res, err := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Run.Status != "failed" {
			t.Errorf("status: got %q want failed", res.Run.Status)
		}
		if res.Run.ErrorMessage == nil || !strings.Contains(*res.Run.ErrorMessage, "loop iteration limit") {
			t.Errorf("error_message: %v", res.Run.ErrorMessage)
		}
	})
}

// TestPerStepTimeout: an Invoke that exceeds PerStepTimeout fails the run.
func TestPerStepTimeout(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		a := f.agentTemplate(t, "A", "trading_decision.v1", []string{"market_target.v1"})
		wf, ids := f.workflowAndNodes(t, []nodeSpec{{key: "a", agentID: a.ID}})
		_ = ids

		mock := newMockInvoker()
		mock.delay = 200 * time.Millisecond
		mock.queue("a", invocationResult{output: validTradingDecision("x", "ABSTAIN", 0, 0.5)})

		ex := makeExecutor(f, mock, Options{PerStepTimeout: 10 * time.Millisecond})
		res, err := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Run.Status != "timed_out" {
			t.Errorf("status: got %q want timed_out", res.Run.Status)
		}
	})
}

// TestPerRunTimeout: total wall-clock cap. Use injected `now` to advance time
// past the deadline mid-loop.
func TestPerRunTimeout(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		a := f.agentTemplate(t, "A", "observation.v1", []string{"market_target.v1"})
		b := f.agentTemplate(t, "B", "trading_decision.v1", []string{"observation.v1"})
		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "a", agentID: a.ID},
			{key: "b", agentID: b.ID},
		})
		f.edge(t, wf, ids["a"], ids["b"], "always", 0)

		mock := newMockInvoker()
		mock.queue("a", invocationResult{output: validObservation("x")})
		mock.queue("b", invocationResult{output: validTradingDecision("x", "YES", 10, 0.5)})

		// `now` returns t0 first time, then t0 + 1h on every subsequent call.
		// runDeadline uses t0 + perRunTimeout (1ms) → already past.
		var calls int
		t0 := time.Now()
		nowFn := func() time.Time {
			calls++
			if calls == 1 {
				return t0
			}
			return t0.Add(time.Hour)
		}

		ex := makeExecutor(f, mock, Options{
			PerRunTimeout: 1 * time.Millisecond,
			Now:           nowFn,
		})
		res, _ := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if res.Run.Status != "timed_out" {
			t.Errorf("status: got %q want timed_out (msg=%v)", res.Run.Status, res.Run.ErrorMessage)
		}
	})
}

// TestPerRunStepCap: exceed MaxStepsPerRun → run fails.
func TestPerRunStepCap(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		watcher := f.agentTemplate(t, "Watcher", "observation.v1", []string{"market_target.v1"})
		thesis := f.agentTemplate(t, "Thesis", "thesis.v1", []string{"observation.v1", "risk_assessment.v1"})
		risk := f.agentTemplate(t, "Risk", "risk_assessment.v1", []string{"thesis.v1"})
		exec := f.agentTemplate(t, "Executor", "trading_decision.v1", []string{"risk_assessment.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "watcher", agentID: watcher.ID},
			{key: "thesis", agentID: thesis.ID, join: "all", loopLimit: 50},
			{key: "risk", agentID: risk.ID},
			{key: "executor", agentID: exec.ID},
		})
		f.edge(t, wf, ids["watcher"], ids["thesis"], "always", 0)
		f.edge(t, wf, ids["thesis"], ids["risk"], "always", 0)
		f.edge(t, wf, ids["risk"], ids["executor"], "approved", 0)
		f.edge(t, wf, ids["risk"], ids["thesis"], "rejected", 1)

		mock := newMockInvoker()
		mock.queue("watcher", invocationResult{output: validObservation("x")})
		for i := 0; i < 50; i++ {
			mock.queue("thesis", invocationResult{output: validThesis("x", "YES", 0.5)})
			mock.queue("risk", invocationResult{output: validRiskAssessment(false)})
		}

		ex := makeExecutor(f, mock, Options{MaxStepsPerRun: 4})
		res, _ := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if res.Run.Status != "failed" {
			t.Errorf("status: got %q want failed", res.Run.Status)
		}
		if res.Run.ErrorMessage == nil || !strings.Contains(*res.Run.ErrorMessage, "step cap") {
			t.Errorf("error_message: %v", res.Run.ErrorMessage)
		}
	})
}

// TestKillSwitchMidRun: flip kill_switch in the middle of the loop.
// Tx isolation: the executor reads kill_switch via the SAME tx that's about
// to roll back, so we can flip it inline by doing a SetKillSwitch(true) call
// after the first step has run. We use mock.onInvoke to inject the flip.
func TestKillSwitchMidRun(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		a := f.agentTemplate(t, "A", "observation.v1", []string{"market_target.v1"})
		b := f.agentTemplate(t, "B", "trading_decision.v1", []string{"observation.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "a", agentID: a.ID},
			{key: "b", agentID: b.ID},
		})
		f.edge(t, wf, ids["a"], ids["b"], "always", 0)

		mock := newMockInvoker()
		mock.queue("a", invocationResult{output: validObservation("x")})
		mock.queue("b", invocationResult{output: validTradingDecision("x", "YES", 10, 0.5)})
		mock.onInvoke = func(nodeKey string, _ int) {
			if nodeKey == "a" {
				_ = f.configRepo.SetKillSwitch(f.ctx, f.tx, true)
			}
		}

		ex := makeExecutor(f, mock, Options{})
		res, _ := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if res.Run.Status != "killed" {
			t.Errorf("status: got %q want killed", res.Run.Status)
		}
	})
}

// TestApprovedRejectedOnNonRiskSourceNoMatch: an `approved` edge whose
// source isn't risk_assessment.v1 (no `approved` field) shouldn't crash —
// the engine just doesn't match the condition.
func TestApprovedRejectedOnNonRiskSourceNoMatch(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		watcher := f.agentTemplate(t, "Watcher", "observation.v1", []string{"market_target.v1"})
		exec := f.agentTemplate(t, "Executor", "trading_decision.v1", []string{"observation.v1"})
		fallback := f.agentTemplate(t, "Fallback", "trading_decision.v1", []string{"observation.v1"})

		wf, ids := f.workflowAndNodes(t, []nodeSpec{
			{key: "watcher", agentID: watcher.ID},
			{key: "executor", agentID: exec.ID},
			{key: "fallback", agentID: fallback.ID},
		})
		// Watcher emits observation.v1 (no `approved` field). Both
		// approved and rejected edges should no-match.
		f.edge(t, wf, ids["watcher"], ids["executor"], "approved", 0)
		f.edge(t, wf, ids["watcher"], ids["fallback"], "rejected", 1)

		mock := newMockInvoker()
		mock.queue("watcher", invocationResult{output: validObservation("x")})

		ex := makeExecutor(f, mock, Options{})
		res, err := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		// Run completes (ready queue empties), only Watcher fires.
		if res.Run.Status != "completed" {
			t.Errorf("status: got %q want completed", res.Run.Status)
		}
		if len(res.Steps) != 1 {
			t.Errorf("steps: got %d want 1 (no edge should match)", len(res.Steps))
		}
	})
}

// TestStepFailureTerminatesRun: a runtime error from Invoke ends the run as failed.
func TestStepFailureTerminatesRun(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		a := f.agentTemplate(t, "A", "trading_decision.v1", []string{"market_target.v1"})
		wf, _ := f.workflowAndNodes(t, []nodeSpec{{key: "a", agentID: a.ID}})

		mock := newMockInvoker()
		mock.queue("a", invocationResult{err: errors.New("validation failed after 2 attempts")})

		ex := makeExecutor(f, mock, Options{})
		res, _ := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if res.Run.Status != "failed" {
			t.Errorf("status: got %q want failed", res.Run.Status)
		}
		if len(res.Steps) != 1 || res.Steps[0].Status != "failed" {
			t.Errorf("step: got %+v want failed", res.Steps)
		}
	})
}

// TestAbstainTerminalProducesAbstainedRow: side=ABSTAIN → status='abstained'.
func TestAbstainTerminalProducesAbstainedRow(t *testing.T) {
	withTx(t, func(t *testing.T, f *fixtures) {
		a := f.agentTemplate(t, "A", "trading_decision.v1", []string{"market_target.v1"})
		wf, _ := f.workflowAndNodes(t, []nodeSpec{{key: "a", agentID: a.ID}})

		mock := newMockInvoker()
		mock.queue("a", invocationResult{output: validTradingDecision("x", "ABSTAIN", 0, 0.42)})

		ex := makeExecutor(f, mock, Options{})
		res, err := ex.Run(f.ctx, RunRequest{WorkflowID: wf, UserID: f.userID, MarketSlug: "x", TriggeredBy: "manual"})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.PaperTrade == nil {
			t.Fatalf("expected paper_trade")
		}
		if res.PaperTrade.Status != "abstained" {
			t.Errorf("status: got %q want abstained", res.PaperTrade.Status)
		}
		if res.PaperTrade.SizeUSD != 0 {
			t.Errorf("size_usd: got %v want 0", res.PaperTrade.SizeUSD)
		}
		if res.PaperTrade.EntryPrice != 0.42 {
			t.Errorf("entry_price: got %v want 0.42 (midpoint snapshot)", res.PaperTrade.EntryPrice)
		}
	})
}
