package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/robfig/cron/v3"

	"github.com/OldEphraim/sibyl-hub/api/internal/billing"
	"github.com/OldEphraim/sibyl-hub/api/internal/db"
	"github.com/OldEphraim/sibyl-hub/api/internal/db/repos"
)

// RunDispatcher is the engine surface the scheduler depends on.
// *engine.Executor satisfies this directly; tests pass a stub that records
// dispatch calls without actually invoking the agent runtime.
type RunDispatcher interface {
	Run(ctx context.Context, req DispatchRequest) error
}

// DispatchRequest mirrors engine.RunRequest minus its dependency cycle. The
// scheduler builds one of these per (workflow, market_target) pair and
// hands it to the dispatcher.
type DispatchRequest struct {
	WorkflowID  uuid.UUID
	UserID      uuid.UUID
	MarketSlug  string
	TriggeredBy string
}

// Scheduler wraps robfig/cron with the workflows-as-jobs model. On Start()
// it loads every workflow with `schedule_cron IS NOT NULL AND is_active =
// true` and registers a cron job that, on each tick, iterates the
// workflow's market_targets and dispatches one run per target.
//
// Per-tick guards (per CLAUDE.md "Cost Protection"):
//
//   1. Kill switch — if `system_config.kill_switch = true`, log INFO and
//      skip the whole tick (no targets fire).
//   2. Per-user quota — CheckQuota is called before EACH market_target
//      dispatch. Once a user is over quota, remaining targets in the same
//      tick are skipped (cap is per-user-per-day, so the first overflow
//      means the rest will overflow too — log once and stop iterating).
//
// Runs are dispatched fire-and-forget; the scheduler doesn't wait for the
// engine to finish a run before continuing the cron loop. The engine owns
// its own per-step + per-run timeouts.
type Scheduler struct {
	cron       *cron.Cron
	workflows  *repos.WorkflowsRepo
	configRepo *repos.SystemConfigRepo
	dispatcher RunDispatcher
	quotas     *billing.Quotas
	dbq        db.Querier
	logger     *slog.Logger

	mu      sync.Mutex
	entries map[uuid.UUID]scheduledEntry // workflowID → cron entry + last-known schedule_cron
	cronMin time.Duration                // injected MinScheduleInterval for tests
}

// scheduledEntry pairs a robfig/cron entry id with the schedule_cron string
// it was registered against. Reload uses the cron string to detect when a
// workflow's schedule has changed since the last reload — without it we'd
// keep firing on the old schedule until process restart (Phase 7.5 fix).
type scheduledEntry struct {
	entryID      cron.EntryID
	scheduleCron string
}

// Options configures NewScheduler.
type Options struct {
	Logger *slog.Logger
}

// NewScheduler builds a Scheduler. Caller must invoke Start() to begin
// firing jobs and Stop() during shutdown.
func NewScheduler(
	dbq db.Querier,
	workflowsRepo *repos.WorkflowsRepo,
	configRepo *repos.SystemConfigRepo,
	dispatcher RunDispatcher,
	quotas *billing.Quotas,
	opts Options,
) *Scheduler {
	if dispatcher == nil {
		panic("scheduler: NewScheduler: nil dispatcher")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Scheduler{
		cron:       cron.New(cron.WithParser(cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow))),
		workflows:  workflowsRepo,
		configRepo: configRepo,
		dispatcher: dispatcher,
		quotas:     quotas,
		dbq:        dbq,
		logger:     opts.Logger,
		entries:    make(map[uuid.UUID]scheduledEntry),
		cronMin:    MinScheduleInterval(),
	}
}

// Start begins the cron loop. Caller must invoke Stop() at shutdown to
// drain in-flight cron callbacks.
func (s *Scheduler) Start(ctx context.Context) error {
	if err := s.Reload(ctx); err != nil {
		return err
	}
	s.cron.Start()
	return nil
}

// Stop drains the cron loop. Returns a context that completes when all
// in-flight cron callbacks finish; callers can choose to wait.
func (s *Scheduler) Stop() context.Context {
	return s.cron.Stop()
}

// Reload diffs the registered cron entries against the DB's current
// `is_active = true AND schedule_cron IS NOT NULL` set. Adds entries for
// new workflows; removes entries for workflows that have been deleted or
// deactivated. Phase 8's PATCH /api/workflows/:id calls this after a save.
//
// Reload is idempotent — calling it multiple times in quick succession
// only updates the diff, never duplicates entries.
func (s *Scheduler) Reload(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	want, err := s.loadActive(ctx)
	if err != nil {
		return fmt.Errorf("scheduler: Reload: %w", err)
	}

	// Remove entries no longer in the active set.
	for wfID, ent := range s.entries {
		if _, ok := want[wfID]; !ok {
			s.cron.Remove(ent.entryID)
			delete(s.entries, wfID)
			s.logger.Info("scheduler: removed entry", "workflow_id", wfID)
		}
	}

	// Add or re-register entries for active workflows.
	for wfID, wf := range want {
		if wf.ScheduleCron == nil {
			// Defensive — loadActive's WHERE clause excludes NULL
			// schedule_cron rows, so we shouldn't see one here.
			continue
		}
		if existing, ok := s.entries[wfID]; ok {
			if existing.scheduleCron == *wf.ScheduleCron {
				continue // unchanged
			}
			// Schedule changed — drop the old entry, fall through to re-register.
			s.cron.Remove(existing.entryID)
			delete(s.entries, wfID)
			s.logger.Info("scheduler: schedule changed, re-registering",
				"workflow_id", wfID,
				"old", existing.scheduleCron,
				"new", *wf.ScheduleCron)
		}
		if err := s.registerLocked(wf); err != nil {
			s.logger.Error("scheduler: register failed",
				"workflow_id", wfID, "schedule_cron", wf.ScheduleCron, "error", err)
			continue
		}
	}
	return nil
}

// loadActive returns workflows currently eligible for scheduling.
func (s *Scheduler) loadActive(ctx context.Context) (map[uuid.UUID]repos.Workflow, error) {
	const sql = `
		SELECT id, owner_id, name, description, schedule_cron, is_active,
		       market_targets, visibility, forked_from, is_system,
		       created_at, updated_at
		FROM workflows
		WHERE is_active = TRUE AND schedule_cron IS NOT NULL
	`
	rows, err := s.dbq.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]repos.Workflow)
	for rows.Next() {
		var w repos.Workflow
		if err := rows.Scan(
			&w.ID, &w.OwnerID, &w.Name, &w.Description, &w.ScheduleCron, &w.IsActive,
			&w.MarketTargets, &w.Visibility, &w.ForkedFrom, &w.IsSystem,
			&w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out[w.ID] = w
	}
	return out, rows.Err()
}

// registerLocked adds a workflow to the cron scheduler. Caller must hold s.mu.
// Validates the cron expression against the minimum interval before
// registering — a bad expression in the DB shouldn't crash the scheduler.
func (s *Scheduler) registerLocked(wf repos.Workflow) error {
	if wf.ScheduleCron == nil || *wf.ScheduleCron == "" {
		return errors.New("scheduler: empty schedule_cron")
	}
	if err := validateCronWithMin(*wf.ScheduleCron, s.cronMin); err != nil {
		return err
	}
	if wf.OwnerID == nil {
		return errors.New("scheduler: workflow has no owner_id (system-owned scheduling not yet supported)")
	}
	owner := *wf.OwnerID

	job := func() {
		s.fire(wf.ID, owner, wf.MarketTargets)
	}
	id, err := s.cron.AddFunc(*wf.ScheduleCron, job)
	if err != nil {
		return fmt.Errorf("scheduler: AddFunc: %w", err)
	}
	s.entries[wf.ID] = scheduledEntry{
		entryID:      id,
		scheduleCron: *wf.ScheduleCron,
	}
	s.logger.Info("scheduler: registered workflow",
		"workflow_id", wf.ID, "schedule_cron", *wf.ScheduleCron,
		"market_targets", len(wf.MarketTargets))
	return nil
}

// fire is the per-tick job body. Checks the kill switch once, then iterates
// market_targets dispatching one run per target. Per-user quota gates each
// dispatch; the first quota miss aborts the rest of the tick (cap is
// per-user-per-day, so subsequent targets would also miss).
func (s *Scheduler) fire(workflowID, userID uuid.UUID, targets []string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Kill-switch first — overrides everything else.
	if s.configRepo != nil {
		on, err := s.configRepo.GetKillSwitch(ctx, s.dbq)
		if err != nil {
			s.logger.Error("scheduler: kill_switch read failed (skipping tick)",
				"workflow_id", workflowID, "error", err)
			return
		}
		if on {
			s.logger.Info("scheduler: kill_switch on — skipping fire",
				"workflow_id", workflowID, "targets", len(targets))
			return
		}
	}

	if len(targets) == 0 {
		s.logger.Info("scheduler: workflow has no market_targets — nothing to dispatch",
			"workflow_id", workflowID)
		return
	}

	for i, slug := range targets {
		// Per-user quota check. CLAUDE.md: cap is per-user-per-day, so
		// once we hit it, all subsequent targets in this tick will too.
		// Log once and abort the rest of the tick.
		if s.quotas != nil {
			ok, reason, err := s.quotas.CheckQuota(ctx, s.dbq, userID)
			if err != nil {
				s.logger.Error("scheduler: quota read failed",
					"workflow_id", workflowID, "error", err)
				return
			}
			if !ok {
				s.logger.Info("scheduler: quota exhausted — skipping remaining targets",
					"workflow_id", workflowID, "user_id", userID,
					"reason", reason, "skipped", len(targets)-i)
				return
			}
		}

		// Fire-and-forget: the engine handles its own per-step + per-run
		// timeouts. We don't block the cron loop on a 10-minute run.
		go s.dispatch(workflowID, userID, slug)
	}
}

// dispatch is invoked in a goroutine per (workflow, market_target). Errors
// are logged; the engine has already persisted the workflow_run row with
// the appropriate failure status.
func (s *Scheduler) dispatch(workflowID, userID uuid.UUID, slug string) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultDispatchTimeout)
	defer cancel()
	if err := s.dispatcher.Run(ctx, DispatchRequest{
		WorkflowID:  workflowID,
		UserID:      userID,
		MarketSlug:  slug,
		TriggeredBy: "schedule",
	}); err != nil {
		s.logger.Error("scheduler: engine dispatch failed",
			"workflow_id", workflowID, "user_id", userID, "slug", slug, "error", err)
	}
}

// DefaultDispatchTimeout is the absolute upper bound on a scheduled run —
// roughly the per-run timeout (10 min) + slack for any post-run persistence.
// The engine's own per-run timeout is the authoritative cap; this just
// prevents an engine bug from holding a goroutine forever.
const DefaultDispatchTimeout = 12 * time.Minute

// EntryCount is a test/diagnostic accessor — current registered job count.
func (s *Scheduler) EntryCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// HasEntry reports whether a given workflow id is currently registered.
func (s *Scheduler) HasEntry(workflowID uuid.UUID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.entries[workflowID]
	return ok
}

// ScheduleStringFor returns the schedule_cron string a workflow is
// currently registered against, or ("", false) if the workflow isn't in the
// entry map. Test/diagnostic accessor — handlers shouldn't need this.
func (s *Scheduler) ScheduleStringFor(workflowID uuid.UUID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ent, ok := s.entries[workflowID]
	if !ok {
		return "", false
	}
	return ent.scheduleCron, true
}
