package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
)

// PaperTrade mirrors the paper_trades table.
type PaperTrade struct {
	ID              uuid.UUID
	WorkflowRunID   uuid.UUID
	UserID          uuid.UUID
	MarketSlug      string
	ConditionID     string
	TokenID         string
	MarketQuestion  string
	Side            string  // 'YES' | 'NO' | 'ABSTAIN'
	SizeUSD         float64
	EntryPrice      float64
	Reasoning       *string
	Status          string // 'open' | 'closed' | 'resolved' | 'abstained'
	CurrentPrice    *float64
	PnLUSD          *float64
	EnteredAt       time.Time
	ExitedAt        *time.Time
	ResolvedAt      *time.Time
}

// PaperTradesRepo persists the engine's terminal side-effect: when the
// Paper Executor's agent_steps row finalizes with a valid trading_decision.v1,
// the engine maps the payload onto this row.
//
// v0 only ever writes 'open' or 'abstained' rows; 'closed' / 'resolved'
// transitions are forward-compat for v1+ PnL and resolution monitoring.
// See CLAUDE.md "Paper Trades" + DATABASE_SCHEMA.md for the column-vs-status
// rules around entry_price (executable price for 'open', midpoint snapshot
// for 'abstained').
type PaperTradesRepo struct{}

func NewPaperTradesRepo() *PaperTradesRepo { return &PaperTradesRepo{} }

// CreatePaperTradeParams is the engine-mapped row, before the DB assigns id
// and entered_at. Status is dictated by the side per CLAUDE.md / AGENT_TEMPLATES.md:
//
//	YES, NO   → 'open',     size_usd > 0, entry_price = decision.executed_price
//	ABSTAIN   → 'abstained', size_usd = 0, entry_price = decision.executed_price (midpoint snapshot)
type CreatePaperTradeParams struct {
	WorkflowRunID  uuid.UUID
	UserID         uuid.UUID
	MarketSlug     string
	ConditionID    string
	TokenID        string
	MarketQuestion string
	Side           string
	SizeUSD        float64
	EntryPrice     float64
	Reasoning      *string
	Status         string // engine fills based on Side
}

// Create inserts a paper_trades row. The engine calls this immediately
// after UpdateStepCompleted on the Paper Executor's step.
func (r *PaperTradesRepo) Create(ctx context.Context, q db.Querier, p CreatePaperTradeParams) (*PaperTrade, error) {
	if p.Side == "" {
		return nil, fmt.Errorf("paper_trades: Create: empty side")
	}
	if p.Status == "" {
		return nil, fmt.Errorf("paper_trades: Create: empty status")
	}
	const sql = `
		INSERT INTO paper_trades (
			workflow_run_id, user_id,
			market_slug, condition_id, token_id, market_question,
			side, size_usd, entry_price, reasoning, status
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, workflow_run_id, user_id,
		          market_slug, condition_id, token_id, market_question,
		          side, size_usd, entry_price, reasoning, status,
		          current_price, pnl_usd, entered_at, exited_at, resolved_at
	`
	var pt PaperTrade
	err := q.QueryRow(ctx, sql,
		p.WorkflowRunID, p.UserID,
		p.MarketSlug, p.ConditionID, p.TokenID, p.MarketQuestion,
		p.Side, p.SizeUSD, p.EntryPrice, p.Reasoning, p.Status,
	).Scan(
		&pt.ID, &pt.WorkflowRunID, &pt.UserID,
		&pt.MarketSlug, &pt.ConditionID, &pt.TokenID, &pt.MarketQuestion,
		&pt.Side, &pt.SizeUSD, &pt.EntryPrice, &pt.Reasoning, &pt.Status,
		&pt.CurrentPrice, &pt.PnLUSD, &pt.EnteredAt, &pt.ExitedAt, &pt.ResolvedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("paper_trades: Create: %w", err)
	}
	return &pt, nil
}

// ListByUser returns the user's paper trades, newest first. Used by the
// account/dashboard pages in Phase 13+.
func (r *PaperTradesRepo) ListByUser(ctx context.Context, q db.Querier, userID uuid.UUID, limit int) ([]*PaperTrade, error) {
	if limit <= 0 {
		limit = 50
	}
	const sql = `
		SELECT id, workflow_run_id, user_id,
		       market_slug, condition_id, token_id, market_question,
		       side, size_usd, entry_price, reasoning, status,
		       current_price, pnl_usd, entered_at, exited_at, resolved_at
		FROM paper_trades
		WHERE user_id = $1
		ORDER BY entered_at DESC
		LIMIT $2
	`
	rows, err := q.Query(ctx, sql, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("paper_trades: ListByUser: %w", err)
	}
	defer rows.Close()
	var out []*PaperTrade
	for rows.Next() {
		var pt PaperTrade
		if err := rows.Scan(
			&pt.ID, &pt.WorkflowRunID, &pt.UserID,
			&pt.MarketSlug, &pt.ConditionID, &pt.TokenID, &pt.MarketQuestion,
			&pt.Side, &pt.SizeUSD, &pt.EntryPrice, &pt.Reasoning, &pt.Status,
			&pt.CurrentPrice, &pt.PnLUSD, &pt.EnteredAt, &pt.ExitedAt, &pt.ResolvedAt,
		); err != nil {
			return nil, fmt.Errorf("paper_trades: ListByUser: scan: %w", err)
		}
		out = append(out, &pt)
	}
	return out, rows.Err()
}

// ListByRun returns paper trades created by a specific workflow run.
// In v0 this is always 0 or 1 rows (workflows have a single terminal node).
func (r *PaperTradesRepo) ListByRun(ctx context.Context, q db.Querier, runID uuid.UUID) ([]*PaperTrade, error) {
	const sql = `
		SELECT id, workflow_run_id, user_id,
		       market_slug, condition_id, token_id, market_question,
		       side, size_usd, entry_price, reasoning, status,
		       current_price, pnl_usd, entered_at, exited_at, resolved_at
		FROM paper_trades
		WHERE workflow_run_id = $1
		ORDER BY entered_at ASC
	`
	rows, err := q.Query(ctx, sql, runID)
	if err != nil {
		return nil, fmt.Errorf("paper_trades: ListByRun: %w", err)
	}
	defer rows.Close()
	var out []*PaperTrade
	for rows.Next() {
		var pt PaperTrade
		if err := rows.Scan(
			&pt.ID, &pt.WorkflowRunID, &pt.UserID,
			&pt.MarketSlug, &pt.ConditionID, &pt.TokenID, &pt.MarketQuestion,
			&pt.Side, &pt.SizeUSD, &pt.EntryPrice, &pt.Reasoning, &pt.Status,
			&pt.CurrentPrice, &pt.PnLUSD, &pt.EnteredAt, &pt.ExitedAt, &pt.ResolvedAt,
		); err != nil {
			return nil, fmt.Errorf("paper_trades: ListByRun: scan: %w", err)
		}
		out = append(out, &pt)
	}
	return out, rows.Err()
}
