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

// Workflow mirrors the workflows table.
type Workflow struct {
	ID            uuid.UUID
	OwnerID       *uuid.UUID
	Name          string
	Description   *string
	ScheduleCron  *string
	IsActive      bool
	MarketTargets []string
	Visibility    string
	ForkedFrom    *uuid.UUID
	IsSystem      bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// WorkflowNode mirrors workflow_nodes. ConfigOverrides is the raw JSONB.
type WorkflowNode struct {
	ID                 uuid.UUID
	WorkflowID         uuid.UUID
	AgentTemplateID    uuid.UUID
	NodeKey            string
	PositionX          float32
	PositionY          float32
	ConfigOverrides    json.RawMessage
	JoinStrategy       string
	LoopIterationLimit int
}

// WorkflowEdge mirrors workflow_edges.
type WorkflowEdge struct {
	ID         uuid.UUID
	WorkflowID uuid.UUID
	FromNodeID uuid.UUID
	ToNodeID   uuid.UUID
	Condition  string
	Priority   int
}

// WorkflowGraph is what the engine and the workflow-detail handler load
// together. Edges arrive in priority order — see the constant below.
type WorkflowGraph struct {
	Workflow Workflow
	Nodes    []WorkflowNode
	Edges    []WorkflowEdge
}

// EdgeOrderClause is the SQL fragment EVERY query that loads outgoing edges
// from a node MUST use. CLAUDE.md and STEPS.md call this out as
// non-negotiable: the engine relies on deterministic edge order to evaluate
// conditional edges by priority, and a missing ORDER BY makes the engine's
// test matrix flaky.
//
// Public constant so external callers (the engine in Phase 6) can use it
// directly when they're loading edges outside this repo (e.g., per-node
// outgoing-edge subqueries against a held connection). Search the codebase
// for `EdgeOrderClause` to audit all call sites.
const EdgeOrderClause = "ORDER BY priority ASC, id ASC"

// WorkflowsRepo is the read/write layer over workflows + workflow_nodes +
// workflow_edges. CRUD on the workflow row stays here; cross-table mutation
// (workflow + subscriptions) is the caller's job (begin tx, call us, call
// SubscriptionsRepo.SyncForWorkflow, commit).
type WorkflowsRepo struct{}

func NewWorkflowsRepo() *WorkflowsRepo { return &WorkflowsRepo{} }

// CreateWorkflowParams captures the workflow row only — nodes and edges are
// inserted via separate calls so the caller can validate the graph (Phase 8
// type-compat check) between insertion of the row and insertion of nodes/edges.
type CreateWorkflowParams struct {
	OwnerID       *uuid.UUID
	Name          string
	Description   *string
	ScheduleCron  *string
	IsActive      *bool // nil → use schema default (true); explicit *false to draft
	MarketTargets []string
	Visibility    string
	ForkedFrom    *uuid.UUID
	IsSystem      bool
}

func (r *WorkflowsRepo) Create(ctx context.Context, q db.Querier, p CreateWorkflowParams) (*Workflow, error) {
	if p.Name == "" {
		return nil, fmt.Errorf("workflows: Create: empty name")
	}
	visibility := p.Visibility
	if visibility == "" {
		visibility = "private"
	}
	targets := p.MarketTargets
	if targets == nil {
		targets = []string{}
	}
	// IsActive: bool zero-value collides with the schema default (TRUE), so
	// callers express intent via a pointer. nil → take the DB default.
	isActive := true
	if p.IsActive != nil {
		isActive = *p.IsActive
	}
	const sql = `
		INSERT INTO workflows (
			owner_id, name, description, schedule_cron, is_active,
			market_targets, visibility, forked_from, is_system
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id, owner_id, name, description, schedule_cron, is_active,
		          market_targets, visibility, forked_from, is_system,
		          created_at, updated_at
	`
	var w Workflow
	err := q.QueryRow(ctx, sql,
		p.OwnerID, p.Name, p.Description, p.ScheduleCron, isActive,
		targets, visibility, p.ForkedFrom, p.IsSystem,
	).Scan(
		&w.ID, &w.OwnerID, &w.Name, &w.Description, &w.ScheduleCron, &w.IsActive,
		&w.MarketTargets, &w.Visibility, &w.ForkedFrom, &w.IsSystem,
		&w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("workflows: Create: %w", err)
	}
	return &w, nil
}

// GetByID returns the workflow row only — call GetGraph if you need nodes
// and edges too. Splitting the two keeps the lightweight list endpoint cheap.
func (r *WorkflowsRepo) GetByID(ctx context.Context, q db.Querier, id uuid.UUID) (*Workflow, error) {
	const sql = `
		SELECT id, owner_id, name, description, schedule_cron, is_active,
		       market_targets, visibility, forked_from, is_system,
		       created_at, updated_at
		FROM workflows WHERE id = $1
	`
	var w Workflow
	err := q.QueryRow(ctx, sql, id).Scan(
		&w.ID, &w.OwnerID, &w.Name, &w.Description, &w.ScheduleCron, &w.IsActive,
		&w.MarketTargets, &w.Visibility, &w.ForkedFrom, &w.IsSystem,
		&w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("workflows: GetByID: %w", db.MapNoRows(err))
	}
	return &w, nil
}

// GetGraph loads workflow + nodes + edges in a single connection. Edges are
// ordered by EdgeOrderClause (priority ASC, id ASC) — relied on by the
// engine for deterministic conditional-edge evaluation.
func (r *WorkflowsRepo) GetGraph(ctx context.Context, q db.Querier, id uuid.UUID) (*WorkflowGraph, error) {
	w, err := r.GetByID(ctx, q, id)
	if err != nil {
		return nil, err
	}
	nodes, err := r.ListNodes(ctx, q, id)
	if err != nil {
		return nil, err
	}
	edges, err := r.ListEdges(ctx, q, id)
	if err != nil {
		return nil, err
	}
	return &WorkflowGraph{Workflow: *w, Nodes: nodes, Edges: edges}, nil
}

// ListWorkflows returns workflows owned by ownerID, plus optionally any
// is_system rows (per CLAUDE.md per-resource rules — system templates are
// readable by all). limit defaults to 50.
func (r *WorkflowsRepo) ListByOwner(ctx context.Context, q db.Querier, ownerID uuid.UUID, includeSystem bool, limit int) ([]*Workflow, error) {
	if limit <= 0 {
		limit = 50
	}
	var (
		sql  string
		args []any
	)
	if includeSystem {
		sql = `
			SELECT id, owner_id, name, description, schedule_cron, is_active,
			       market_targets, visibility, forked_from, is_system,
			       created_at, updated_at
			FROM workflows
			WHERE owner_id = $1 OR is_system = TRUE
			ORDER BY created_at DESC
			LIMIT $2
		`
		args = []any{ownerID, limit}
	} else {
		sql = `
			SELECT id, owner_id, name, description, schedule_cron, is_active,
			       market_targets, visibility, forked_from, is_system,
			       created_at, updated_at
			FROM workflows
			WHERE owner_id = $1
			ORDER BY created_at DESC
			LIMIT $2
		`
		args = []any{ownerID, limit}
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("workflows: ListByOwner: %w", err)
	}
	defer rows.Close()
	var out []*Workflow
	for rows.Next() {
		var w Workflow
		if err := rows.Scan(
			&w.ID, &w.OwnerID, &w.Name, &w.Description, &w.ScheduleCron, &w.IsActive,
			&w.MarketTargets, &w.Visibility, &w.ForkedFrom, &w.IsSystem,
			&w.CreatedAt, &w.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("workflows: ListByOwner: scan: %w", err)
		}
		out = append(out, &w)
	}
	return out, rows.Err()
}

// UpdateWorkflowParams — same nil-means-leave-alone convention as
// UpdateAgentTemplateParams.
type UpdateWorkflowParams struct {
	Name          *string
	Description   *string
	ScheduleCron  *string
	IsActive      *bool
	MarketTargets *[]string
	Visibility    *string
}

func (r *WorkflowsRepo) Update(ctx context.Context, q db.Querier, id uuid.UUID, p UpdateWorkflowParams) (*Workflow, error) {
	args := []any{id}
	sets := []string{}
	add := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if p.Name != nil {
		add("name", *p.Name)
	}
	if p.Description != nil {
		add("description", *p.Description)
	}
	if p.ScheduleCron != nil {
		add("schedule_cron", *p.ScheduleCron)
	}
	if p.IsActive != nil {
		add("is_active", *p.IsActive)
	}
	if p.MarketTargets != nil {
		add("market_targets", *p.MarketTargets)
	}
	if p.Visibility != nil {
		add("visibility", *p.Visibility)
	}
	if len(sets) == 0 {
		return r.GetByID(ctx, q, id)
	}
	sets = append(sets, "updated_at = NOW()")
	sql := "UPDATE workflows SET " + joinComma(sets) + " WHERE id = $1 RETURNING " +
		"id, owner_id, name, description, schedule_cron, is_active, " +
		"market_targets, visibility, forked_from, is_system, created_at, updated_at"
	var w Workflow
	err := q.QueryRow(ctx, sql, args...).Scan(
		&w.ID, &w.OwnerID, &w.Name, &w.Description, &w.ScheduleCron, &w.IsActive,
		&w.MarketTargets, &w.Visibility, &w.ForkedFrom, &w.IsSystem,
		&w.CreatedAt, &w.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("workflows: Update: %w", err)
	}
	return &w, nil
}

func (r *WorkflowsRepo) Delete(ctx context.Context, q db.Querier, id uuid.UUID) error {
	tag, err := q.Exec(ctx, "DELETE FROM workflows WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("workflows: Delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return db.ErrNotFound
	}
	return nil
}

// CreateNodeParams matches workflow_nodes columns.
type CreateNodeParams struct {
	WorkflowID         uuid.UUID
	AgentTemplateID    uuid.UUID
	NodeKey            string
	PositionX          float32
	PositionY          float32
	ConfigOverrides    json.RawMessage
	JoinStrategy       string
	LoopIterationLimit int
}

func (r *WorkflowsRepo) CreateNode(ctx context.Context, q db.Querier, p CreateNodeParams) (*WorkflowNode, error) {
	if p.NodeKey == "" {
		return nil, fmt.Errorf("workflow_nodes: CreateNode: empty node_key")
	}
	join := p.JoinStrategy
	if join == "" {
		join = "all"
	}
	loop := p.LoopIterationLimit
	if loop == 0 {
		loop = 5
	}
	cfg := p.ConfigOverrides
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	const sql = `
		INSERT INTO workflow_nodes (
			workflow_id, agent_template_id, node_key,
			position_x, position_y, config_overrides,
			join_strategy, loop_iteration_limit
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		RETURNING id, workflow_id, agent_template_id, node_key,
		          position_x, position_y, config_overrides,
		          join_strategy, loop_iteration_limit
	`
	var n WorkflowNode
	err := q.QueryRow(ctx, sql,
		p.WorkflowID, p.AgentTemplateID, p.NodeKey,
		p.PositionX, p.PositionY, cfg,
		join, loop,
	).Scan(
		&n.ID, &n.WorkflowID, &n.AgentTemplateID, &n.NodeKey,
		&n.PositionX, &n.PositionY, &n.ConfigOverrides,
		&n.JoinStrategy, &n.LoopIterationLimit,
	)
	if err != nil {
		return nil, fmt.Errorf("workflow_nodes: CreateNode: %w", err)
	}
	return &n, nil
}

// ListNodes returns all nodes for a workflow. No ordering is mandated by
// the schema; we use id ASC for stability.
func (r *WorkflowsRepo) ListNodes(ctx context.Context, q db.Querier, workflowID uuid.UUID) ([]WorkflowNode, error) {
	const sql = `
		SELECT id, workflow_id, agent_template_id, node_key,
		       position_x, position_y, config_overrides,
		       join_strategy, loop_iteration_limit
		FROM workflow_nodes WHERE workflow_id = $1
		ORDER BY id ASC
	`
	rows, err := q.Query(ctx, sql, workflowID)
	if err != nil {
		return nil, fmt.Errorf("workflow_nodes: ListNodes: %w", err)
	}
	defer rows.Close()
	var out []WorkflowNode
	for rows.Next() {
		var n WorkflowNode
		if err := rows.Scan(
			&n.ID, &n.WorkflowID, &n.AgentTemplateID, &n.NodeKey,
			&n.PositionX, &n.PositionY, &n.ConfigOverrides,
			&n.JoinStrategy, &n.LoopIterationLimit,
		); err != nil {
			return nil, fmt.Errorf("workflow_nodes: ListNodes: scan: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CreateEdgeParams matches workflow_edges.
type CreateEdgeParams struct {
	WorkflowID uuid.UUID
	FromNodeID uuid.UUID
	ToNodeID   uuid.UUID
	Condition  string
	Priority   int
}

func (r *WorkflowsRepo) CreateEdge(ctx context.Context, q db.Querier, p CreateEdgeParams) (*WorkflowEdge, error) {
	cond := p.Condition
	if cond == "" {
		cond = "always"
	}
	const sql = `
		INSERT INTO workflow_edges (workflow_id, from_node_id, to_node_id, condition, priority)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, workflow_id, from_node_id, to_node_id, condition, priority
	`
	var e WorkflowEdge
	err := q.QueryRow(ctx, sql, p.WorkflowID, p.FromNodeID, p.ToNodeID, cond, p.Priority).
		Scan(&e.ID, &e.WorkflowID, &e.FromNodeID, &e.ToNodeID, &e.Condition, &e.Priority)
	if err != nil {
		return nil, fmt.Errorf("workflow_edges: CreateEdge: %w", err)
	}
	return &e, nil
}

// ListEdges returns all edges for a workflow, ordered by EdgeOrderClause.
// This is the workflow-wide listing the engine consumes when it builds the
// in-memory graph. Per-node outgoing-edge listings (ListEdgesFromNode) use
// the same ordering — same invariant, different filter.
func (r *WorkflowsRepo) ListEdges(ctx context.Context, q db.Querier, workflowID uuid.UUID) ([]WorkflowEdge, error) {
	sql := `
		SELECT id, workflow_id, from_node_id, to_node_id, condition, priority
		FROM workflow_edges WHERE workflow_id = $1
		` + EdgeOrderClause
	rows, err := q.Query(ctx, sql, workflowID)
	if err != nil {
		return nil, fmt.Errorf("workflow_edges: ListEdges: %w", err)
	}
	defer rows.Close()
	var out []WorkflowEdge
	for rows.Next() {
		var e WorkflowEdge
		if err := rows.Scan(&e.ID, &e.WorkflowID, &e.FromNodeID, &e.ToNodeID, &e.Condition, &e.Priority); err != nil {
			return nil, fmt.Errorf("workflow_edges: ListEdges: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListEdgesFromNode is the per-node outgoing-edge listing the engine uses
// during graph traversal. Same ordering invariant as ListEdges.
func (r *WorkflowsRepo) ListEdgesFromNode(ctx context.Context, q db.Querier, fromNodeID uuid.UUID) ([]WorkflowEdge, error) {
	sql := `
		SELECT id, workflow_id, from_node_id, to_node_id, condition, priority
		FROM workflow_edges WHERE from_node_id = $1
		` + EdgeOrderClause
	rows, err := q.Query(ctx, sql, fromNodeID)
	if err != nil {
		return nil, fmt.Errorf("workflow_edges: ListEdgesFromNode: %w", err)
	}
	defer rows.Close()
	var out []WorkflowEdge
	for rows.Next() {
		var e WorkflowEdge
		if err := rows.Scan(&e.ID, &e.WorkflowID, &e.FromNodeID, &e.ToNodeID, &e.Condition, &e.Priority); err != nil {
			return nil, fmt.Errorf("workflow_edges: ListEdgesFromNode: scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
