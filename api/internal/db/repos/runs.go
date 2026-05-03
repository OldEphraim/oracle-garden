package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
)

// WorkflowRun mirrors workflow_runs.
type WorkflowRun struct {
	ID            uuid.UUID
	WorkflowID    uuid.UUID
	UserID        uuid.UUID
	TriggeredBy   string // 'schedule' | 'manual'
	Status        string // 'pending' | 'running' | 'completed' | 'failed' | 'timed_out' | 'killed' | 'quota_exceeded'
	MarketSlug    *string
	InputSnapshot json.RawMessage
	ErrorMessage  *string
	StartedAt     time.Time
	FinishedAt    *time.Time
}

// AgentStep mirrors agent_steps.
type AgentStep struct {
	ID               uuid.UUID
	WorkflowRunID    uuid.UUID
	WorkflowNodeID   uuid.UUID
	Iteration        int
	Status           string
	InputData        json.RawMessage
	OutputData       json.RawMessage
	PromptTokens     *int
	CompletionTokens *int
	CostUSD          *float64
	LatencyMs        *int
	ErrorMessage     *string
	StartedAt        time.Time
	FinishedAt       *time.Time
}

// RunWithSteps is what the run-detail handler returns: the run plus its
// time-ordered list of agent_steps.
type RunWithSteps struct {
	Run   WorkflowRun
	Steps []AgentStep
}

// RunsRepo bundles operations on workflow_runs + agent_steps. The two tables
// are tightly coupled (every step belongs to a run; runs are read with
// their steps), so they share a repo.
type RunsRepo struct{}

func NewRunsRepo() *RunsRepo { return &RunsRepo{} }

// CreateRunParams matches the columns the run-create flow needs to set.
// status defaults to 'pending' if empty.
type CreateRunParams struct {
	WorkflowID    uuid.UUID
	UserID        uuid.UUID
	TriggeredBy   string
	Status        string
	MarketSlug    *string
	InputSnapshot json.RawMessage
}

func (r *RunsRepo) Create(ctx context.Context, q db.Querier, p CreateRunParams) (*WorkflowRun, error) {
	if p.TriggeredBy == "" {
		return nil, fmt.Errorf("workflow_runs: Create: empty triggered_by")
	}
	status := p.Status
	if status == "" {
		status = "pending"
	}
	snap := p.InputSnapshot
	if len(snap) == 0 {
		snap = json.RawMessage(`{}`)
	}
	const sql = `
		INSERT INTO workflow_runs (workflow_id, user_id, triggered_by, status, market_slug, input_snapshot)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, workflow_id, user_id, triggered_by, status, market_slug, input_snapshot,
		          error_message, started_at, finished_at
	`
	var run WorkflowRun
	err := q.QueryRow(ctx, sql, p.WorkflowID, p.UserID, p.TriggeredBy, status, p.MarketSlug, snap).Scan(
		&run.ID, &run.WorkflowID, &run.UserID, &run.TriggeredBy, &run.Status,
		&run.MarketSlug, &run.InputSnapshot, &run.ErrorMessage, &run.StartedAt, &run.FinishedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("workflow_runs: Create: %w", err)
	}
	return &run, nil
}

// UpdateRunStatus sets status, optionally also error_message and finished_at
// (set finished=true to stamp NOW()). Used at run termination from the engine.
func (r *RunsRepo) UpdateRunStatus(ctx context.Context, q db.Querier, runID uuid.UUID, status string, errorMessage *string, finished bool) error {
	if status == "" {
		return fmt.Errorf("workflow_runs: UpdateRunStatus: empty status")
	}
	var (
		sql  string
		args []any
	)
	if finished {
		sql = `UPDATE workflow_runs SET status = $2, error_message = $3, finished_at = NOW() WHERE id = $1`
	} else {
		sql = `UPDATE workflow_runs SET status = $2, error_message = $3 WHERE id = $1`
	}
	args = []any{runID, status, errorMessage}
	tag, err := q.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("workflow_runs: UpdateRunStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return db.ErrNotFound
	}
	return nil
}

// AddStepParams matches agent_steps insert columns. status defaults to 'pending'.
type AddStepParams struct {
	WorkflowRunID  uuid.UUID
	WorkflowNodeID uuid.UUID
	Iteration      int
	Status         string
	InputData      json.RawMessage
}

// AddStep inserts an agent_steps row in the 'pending' or 'running' state.
// Cost/token columns are filled in via UpdateStepCompleted at step finish.
func (r *RunsRepo) AddStep(ctx context.Context, q db.Querier, p AddStepParams) (*AgentStep, error) {
	status := p.Status
	if status == "" {
		status = "pending"
	}
	iteration := p.Iteration
	if iteration <= 0 {
		iteration = 1
	}
	const sql = `
		INSERT INTO agent_steps (workflow_run_id, workflow_node_id, iteration, status, input_data)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, workflow_run_id, workflow_node_id, iteration, status,
		          input_data, output_data, prompt_tokens, completion_tokens,
		          cost_usd, latency_ms, error_message, started_at, finished_at
	`
	var s AgentStep
	err := q.QueryRow(ctx, sql, p.WorkflowRunID, p.WorkflowNodeID, iteration, status, p.InputData).Scan(
		&s.ID, &s.WorkflowRunID, &s.WorkflowNodeID, &s.Iteration, &s.Status,
		&s.InputData, &s.OutputData, &s.PromptTokens, &s.CompletionTokens,
		&s.CostUSD, &s.LatencyMs, &s.ErrorMessage, &s.StartedAt, &s.FinishedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("agent_steps: AddStep: %w", err)
	}
	return &s, nil
}

// UpdateStepCompletedParams carries the per-step finalization data.
// All token/cost/latency fields are pointers because steps that fail or
// time out before reaching the model don't have them.
type UpdateStepCompletedParams struct {
	StepID           uuid.UUID
	Status           string // 'completed' | 'failed' | 'timed_out'
	OutputData       json.RawMessage
	PromptTokens     *int
	CompletionTokens *int
	CostUSD          *float64
	LatencyMs        *int
	ErrorMessage     *string
}

// UpdateStepCompleted finalizes a step. Stamps finished_at=NOW().
func (r *RunsRepo) UpdateStepCompleted(ctx context.Context, q db.Querier, p UpdateStepCompletedParams) error {
	if p.Status == "" {
		return fmt.Errorf("agent_steps: UpdateStepCompleted: empty status")
	}
	const sql = `
		UPDATE agent_steps
		SET status = $2,
		    output_data = $3,
		    prompt_tokens = $4,
		    completion_tokens = $5,
		    cost_usd = $6,
		    latency_ms = $7,
		    error_message = $8,
		    finished_at = NOW()
		WHERE id = $1
	`
	tag, err := q.Exec(ctx, sql,
		p.StepID, p.Status, p.OutputData, p.PromptTokens, p.CompletionTokens,
		p.CostUSD, p.LatencyMs, p.ErrorMessage,
	)
	if err != nil {
		return fmt.Errorf("agent_steps: UpdateStepCompleted: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return db.ErrNotFound
	}
	return nil
}

// UpdateStepStatus updates a step's status without touching cost/token data.
// Used for in-progress transitions ('pending' -> 'running' when the step
// starts firing). A finishing transition should use UpdateStepCompleted.
func (r *RunsRepo) UpdateStepStatus(ctx context.Context, q db.Querier, stepID uuid.UUID, status string) error {
	if status == "" {
		return fmt.Errorf("agent_steps: UpdateStepStatus: empty status")
	}
	tag, err := q.Exec(ctx, "UPDATE agent_steps SET status = $2 WHERE id = $1", stepID, status)
	if err != nil {
		return fmt.Errorf("agent_steps: UpdateStepStatus: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return db.ErrNotFound
	}
	return nil
}

// GetWithSteps returns the run plus its time-ordered agent_steps.
// Steps order: started_at ASC, then id ASC for stability when two steps share
// a started_at down to the second (rare but possible under heavy parallel work).
func (r *RunsRepo) GetWithSteps(ctx context.Context, q db.Querier, runID uuid.UUID) (*RunWithSteps, error) {
	const runSQL = `
		SELECT id, workflow_id, user_id, triggered_by, status, market_slug, input_snapshot,
		       error_message, started_at, finished_at
		FROM workflow_runs WHERE id = $1
	`
	var run WorkflowRun
	if err := q.QueryRow(ctx, runSQL, runID).Scan(
		&run.ID, &run.WorkflowID, &run.UserID, &run.TriggeredBy, &run.Status,
		&run.MarketSlug, &run.InputSnapshot, &run.ErrorMessage, &run.StartedAt, &run.FinishedAt,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("workflow_runs: GetWithSteps: %w", err)
	}

	const stepsSQL = `
		SELECT id, workflow_run_id, workflow_node_id, iteration, status,
		       input_data, output_data, prompt_tokens, completion_tokens,
		       cost_usd, latency_ms, error_message, started_at, finished_at
		FROM agent_steps WHERE workflow_run_id = $1
		ORDER BY started_at ASC, id ASC
	`
	rows, err := q.Query(ctx, stepsSQL, runID)
	if err != nil {
		return nil, fmt.Errorf("agent_steps: GetWithSteps: %w", err)
	}
	defer rows.Close()
	var steps []AgentStep
	for rows.Next() {
		var s AgentStep
		if err := rows.Scan(
			&s.ID, &s.WorkflowRunID, &s.WorkflowNodeID, &s.Iteration, &s.Status,
			&s.InputData, &s.OutputData, &s.PromptTokens, &s.CompletionTokens,
			&s.CostUSD, &s.LatencyMs, &s.ErrorMessage, &s.StartedAt, &s.FinishedAt,
		); err != nil {
			return nil, fmt.Errorf("agent_steps: GetWithSteps: scan: %w", err)
		}
		steps = append(steps, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent_steps: GetWithSteps: rows: %w", err)
	}
	return &RunWithSteps{Run: run, Steps: steps}, nil
}

// ListByWorkflow returns runs of a given workflow filtered by user_id (for
// the `GET /api/workflows/:id/runs` endpoint, which scopes a shareable
// parent's child rows to the requesting user — CLAUDE.md per-resource rule).
func (r *RunsRepo) ListByWorkflow(ctx context.Context, q db.Querier, workflowID, userID uuid.UUID, limit int) ([]*WorkflowRun, error) {
	if limit <= 0 {
		limit = 50
	}
	const sql = `
		SELECT id, workflow_id, user_id, triggered_by, status, market_slug, input_snapshot,
		       error_message, started_at, finished_at
		FROM workflow_runs
		WHERE workflow_id = $1 AND user_id = $2
		ORDER BY started_at DESC
		LIMIT $3
	`
	rows, err := q.Query(ctx, sql, workflowID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("workflow_runs: ListByWorkflow: %w", err)
	}
	defer rows.Close()
	var out []*WorkflowRun
	for rows.Next() {
		var run WorkflowRun
		if err := rows.Scan(
			&run.ID, &run.WorkflowID, &run.UserID, &run.TriggeredBy, &run.Status,
			&run.MarketSlug, &run.InputSnapshot, &run.ErrorMessage, &run.StartedAt, &run.FinishedAt,
		); err != nil {
			return nil, fmt.Errorf("workflow_runs: ListByWorkflow: scan: %w", err)
		}
		out = append(out, &run)
	}
	return out, rows.Err()
}
