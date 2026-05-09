package scheduler

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/robfig/cron/v3"

	"github.com/OldEphraim/sibyl-hub/api/internal/billing"
	"github.com/OldEphraim/sibyl-hub/api/internal/db/repos"
)

// Test pool — same pattern as billing_test and engine_test.
var (
	poolOnce sync.Once
	pool     *pgxpool.Pool
)

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			dsn = os.Getenv("DATABASE_URL")
		}
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
		t.Skip("no TEST_DATABASE_URL or DATABASE_URL — skipping scheduler tests")
	}
	return pool
}

// fixtures bundles per-test state.
type fixtures struct {
	tx            pgx.Tx
	workflows     *repos.WorkflowsRepo
	configRepo    *repos.SystemConfigRepo
	usageRepo     *repos.UsageRepo
	agentsRepo    *repos.AgentTemplatesRepo
	userID        uuid.UUID
}

func withFixtures(t *testing.T, fn func(t *testing.T, fix *fixtures)) {
	t.Helper()
	p := testPool(t)
	tx, err := p.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// User + types-registry-aware agents repo.
	usersRepo := repos.NewUsersRepo()
	u, err := usersRepo.Create(context.Background(), tx, repos.CreateUserParams{
		Email:        fmt.Sprintf("sched-%s@example.com", uuid.New().String()),
		PasswordHash: "$2a$10$ignored",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	fn(t, &fixtures{
		tx:         tx,
		workflows:  repos.NewWorkflowsRepo(),
		configRepo: repos.NewSystemConfigRepo(),
		usageRepo:  repos.NewUsageRepo(),
		userID:     u.ID,
	})
}

// recordingDispatcher captures every Run() call for test inspection.
type recordingDispatcher struct {
	mu    sync.Mutex
	calls []DispatchRequest
}

func (r *recordingDispatcher) Run(ctx context.Context, req DispatchRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, req)
	return nil
}

func (r *recordingDispatcher) Calls() []DispatchRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]DispatchRequest, len(r.calls))
	copy(out, r.calls)
	return out
}

func (r *recordingDispatcher) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// makeWorkflow inserts a workflow with the given schedule + targets.
func makeWorkflow(t *testing.T, f *fixtures, scheduleCron string, active bool, targets []string) uuid.UUID {
	t.Helper()
	wf, err := f.workflows.Create(context.Background(), f.tx, repos.CreateWorkflowParams{
		OwnerID:       &f.userID,
		Name:          "T",
		ScheduleCron:  &scheduleCron,
		IsActive:      &active,
		MarketTargets: targets,
	})
	if err != nil {
		t.Fatalf("create workflow: %v", err)
	}
	return wf.ID
}

func makeScheduler(f *fixtures, dispatcher RunDispatcher, quotas *billing.Quotas) *Scheduler {
	return NewScheduler(f.tx, f.workflows, f.configRepo, dispatcher, quotas, Options{Logger: quietLogger()})
}

// ---------- Tests ---------------------------------------------------------

// TestRegisterPicksUpActiveScheduledWorkflows: only active rows with
// schedule_cron != NULL show up in the entry map.
func TestRegisterPicksUpActiveScheduledWorkflows(t *testing.T) {
	withFixtures(t, func(t *testing.T, f *fixtures) {
		ctx := context.Background()
		active := makeWorkflow(t, f, "*/10 * * * *", true, []string{"x"})
		_ = makeWorkflow(t, f, "*/10 * * * *", false, []string{"y"}) // inactive

		// Manual-only workflow (no schedule) must be excluded.
		manualName := "Manual"
		manualOwner := f.userID
		manualActive := true
		_, err := f.workflows.Create(ctx, f.tx, repos.CreateWorkflowParams{
			OwnerID:  &manualOwner,
			Name:     manualName,
			IsActive: &manualActive,
			// no ScheduleCron
		})
		if err != nil {
			t.Fatal(err)
		}

		s := makeScheduler(f, &recordingDispatcher{}, nil)
		if err := s.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if !s.HasEntry(active) {
			t.Errorf("active workflow not registered")
		}
		if s.EntryCount() != 1 {
			t.Errorf("entry count: got %d want 1", s.EntryCount())
		}
	})
}

// TestKillSwitchSkipsAllTargets: when kill_switch=true, the cron job body
// should NOT dispatch anything.
func TestKillSwitchSkipsAllTargets(t *testing.T) {
	withFixtures(t, func(t *testing.T, f *fixtures) {
		ctx := context.Background()
		_ = makeWorkflow(t, f, "*/10 * * * *", true, []string{"a", "b", "c"})
		_ = f.configRepo.SetKillSwitch(ctx, f.tx, true)

		disp := &recordingDispatcher{}
		s := makeScheduler(f, disp, nil)
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}

		// Trigger the job body manually — the cron loop's actual ticking
		// is exercised by the cron library; we test the inline path.
		fireAllRegistered(s)

		if got := disp.Count(); got != 0 {
			t.Errorf("dispatcher calls with kill_switch on: got %d want 0", got)
		}
	})
}

// TestMultiTargetDispatchesPerTarget: a 3-target workflow fires 3 runs.
func TestMultiTargetDispatchesPerTarget(t *testing.T) {
	withFixtures(t, func(t *testing.T, f *fixtures) {
		ctx := context.Background()
		wfID := makeWorkflow(t, f, "*/10 * * * *", true, []string{"alpha", "beta", "gamma"})

		disp := &recordingDispatcher{}
		s := makeScheduler(f, disp, nil)
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		fireAllRegistered(s)

		// Goroutine dispatch — wait until count reaches 3 or fail.
		waitForCount(t, disp, 3, 2*time.Second)
		calls := disp.Calls()
		seenSlugs := map[string]bool{}
		for _, c := range calls {
			if c.WorkflowID != wfID {
				t.Errorf("workflow_id mismatch: %s vs %s", c.WorkflowID, wfID)
			}
			if c.UserID != f.userID {
				t.Errorf("user_id mismatch: %s vs %s", c.UserID, f.userID)
			}
			if c.TriggeredBy != "schedule" {
				t.Errorf("triggered_by: %q", c.TriggeredBy)
			}
			seenSlugs[c.MarketSlug] = true
		}
		for _, want := range []string{"alpha", "beta", "gamma"} {
			if !seenSlugs[want] {
				t.Errorf("missing dispatch for %q (got %v)", want, seenSlugs)
			}
		}
	})
}

// TestQuotaSkipsRemainingTargetsInTick: once the user is over their daily
// run cap, the remaining targets in the SAME tick are skipped.
func TestQuotaSkipsRemainingTargetsInTick(t *testing.T) {
	withFixtures(t, func(t *testing.T, f *fixtures) {
		ctx := context.Background()
		_ = makeWorkflow(t, f, "*/10 * * * *", true, []string{"a", "b", "c"})

		// Pre-fill run_count so the quota check trips immediately.
		quotas := billing.NewQuotas(f.usageRepo)
		quotas.MaxRunsPerDay = 2
		for i := 0; i < 2; i++ {
			if _, err := f.usageRepo.IncrementRun(ctx, f.tx, f.userID); err != nil {
				t.Fatal(err)
			}
		}

		disp := &recordingDispatcher{}
		s := makeScheduler(f, disp, quotas)
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		fireAllRegistered(s)
		// Give any racing goroutines a moment.
		time.Sleep(100 * time.Millisecond)
		if got := disp.Count(); got != 0 {
			t.Errorf("dispatcher calls when over quota: got %d want 0", got)
		}
	})
}

// TestReloadAddsNewWorkflows: register one workflow, then add another and
// Reload again — both end up in the entry map.
func TestReloadAddsNewWorkflows(t *testing.T) {
	withFixtures(t, func(t *testing.T, f *fixtures) {
		ctx := context.Background()
		first := makeWorkflow(t, f, "*/10 * * * *", true, []string{"x"})

		s := makeScheduler(f, &recordingDispatcher{}, nil)
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		if s.EntryCount() != 1 {
			t.Fatalf("after first Reload: count=%d want 1", s.EntryCount())
		}

		second := makeWorkflow(t, f, "*/15 * * * *", true, []string{"y"})
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		if s.EntryCount() != 2 {
			t.Errorf("after second Reload: count=%d want 2", s.EntryCount())
		}
		if !s.HasEntry(first) || !s.HasEntry(second) {
			t.Errorf("missing entries: first=%v second=%v",
				s.HasEntry(first), s.HasEntry(second))
		}
	})
}

// TestReloadRemovesDeactivatedWorkflows: a workflow that flipped to
// is_active=false gets pruned on Reload.
func TestReloadRemovesDeactivatedWorkflows(t *testing.T) {
	withFixtures(t, func(t *testing.T, f *fixtures) {
		ctx := context.Background()
		wfID := makeWorkflow(t, f, "*/10 * * * *", true, []string{"x"})

		s := makeScheduler(f, &recordingDispatcher{}, nil)
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		if !s.HasEntry(wfID) {
			t.Fatalf("entry not registered initially")
		}

		// Deactivate the workflow.
		falsy := false
		_, err := f.workflows.Update(ctx, f.tx, wfID, repos.UpdateWorkflowParams{IsActive: &falsy})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		if s.HasEntry(wfID) {
			t.Errorf("entry should be removed after deactivation")
		}
	})
}

// TestReloadPicksUpScheduleChange (Phase 7.5 fix): when a workflow's
// schedule_cron mutates between two Reload() calls, the old cron entry
// is removed and a new one is registered with the new schedule. Without
// this fix, Phase 8's PATCH /api/workflows/:id would silently keep firing
// on the old schedule until process restart.
func TestReloadPicksUpScheduleChange(t *testing.T) {
	withFixtures(t, func(t *testing.T, f *fixtures) {
		ctx := context.Background()
		wfID := makeWorkflow(t, f, "*/5 * * * *", true, []string{"x"})

		s := makeScheduler(f, &recordingDispatcher{}, nil)
		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}
		if !s.HasEntry(wfID) {
			t.Fatalf("entry not registered initially")
		}
		oldID, ok := entryIDFor(s, wfID)
		if !ok {
			t.Fatalf("entryIDFor: not found")
		}
		if got, _ := s.ScheduleStringFor(wfID); got != "*/5 * * * *" {
			t.Errorf("initial schedule: got %q want */5 * * * *", got)
		}

		// Mutate schedule_cron in the DB — same Workflows repo Update path
		// the Phase 8 handler will use.
		newCron := "0 * * * *"
		if _, err := f.workflows.Update(ctx, f.tx, wfID, repos.UpdateWorkflowParams{
			ScheduleCron: &newCron,
		}); err != nil {
			t.Fatalf("Update: %v", err)
		}

		if err := s.Reload(ctx); err != nil {
			t.Fatal(err)
		}

		// Workflow should still be registered.
		if !s.HasEntry(wfID) {
			t.Fatalf("entry removed after schedule change")
		}
		newID, ok := entryIDFor(s, wfID)
		if !ok {
			t.Fatalf("entryIDFor after Reload: not found")
		}
		if newID == oldID {
			t.Errorf("EntryID should have changed after re-register: old=%d new=%d", oldID, newID)
		}
		if got, _ := s.ScheduleStringFor(wfID); got != newCron {
			t.Errorf("post-reload schedule: got %q want %q", got, newCron)
		}
	})
}

// TestRegisterRejectsBadCron: a workflow with `*/2 * * * *` should fail
// validation and not be registered.
func TestRegisterRejectsBadCron(t *testing.T) {
	withFixtures(t, func(t *testing.T, f *fixtures) {
		ctx := context.Background()
		wfID := makeWorkflow(t, f, "*/2 * * * *", true, []string{"x"})

		s := makeScheduler(f, &recordingDispatcher{}, nil)
		// Reload doesn't fail — bad cron logs ERROR and skips.
		if err := s.Reload(ctx); err != nil {
			t.Fatalf("Reload: %v", err)
		}
		if s.HasEntry(wfID) {
			t.Errorf("workflow with sub-min cron should not have been registered")
		}
	})
}

// fireAllRegistered triggers every registered job's body inline. Used by
// tests to exercise the scheduler's per-tick logic without waiting for the
// real cron clock.
func fireAllRegistered(s *Scheduler) {
	s.mu.Lock()
	entries := make([]cron.Entry, 0, len(s.entries))
	for _, ent := range s.entries {
		entries = append(entries, s.cron.Entry(ent.entryID))
	}
	s.mu.Unlock()
	for _, e := range entries {
		if e.Job != nil {
			e.Job.Run()
		}
	}
}

// entryIDFor reads the unexported entries map for the schedule-change test.
// Test-only — exposing this on Scheduler would invite handler/scheduler
// coupling we don't want.
func entryIDFor(s *Scheduler, workflowID uuid.UUID) (cron.EntryID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ent, ok := s.entries[workflowID]
	if !ok {
		return 0, false
	}
	return ent.entryID, true
}

// waitForCount blocks until the dispatcher's call count reaches `want` or
// the timeout expires (test fails on timeout).
func waitForCount(t *testing.T, disp *recordingDispatcher, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if disp.Count() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waitForCount: got %d want %d after %s", disp.Count(), want, timeout)
}

