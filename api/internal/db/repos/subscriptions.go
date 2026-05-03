package repos

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SubscriptionsRepo owns strategy_market_subscriptions. The table is a
// denormalized view of workflows.market_targets that v0 doesn't read for
// execution decisions; it's seeded so the v1+ watcher service can read
// subscriptions without a schema migration.
//
// The two sources of truth (workflows.market_targets and
// strategy_market_subscriptions) MUST be kept in sync transactionally —
// see CLAUDE.md "Market subscriptions (forward-compat)" and the workflow
// PATCH endpoint description. SyncForWorkflow takes a pgx.Tx (typed,
// not Querier) precisely because its delete-then-insert contract is
// meaningless without a tx; passing a non-tx Querier would let a partial
// update leak into the table if the caller crashed mid-sync.
type SubscriptionsRepo struct{}

func NewSubscriptionsRepo() *SubscriptionsRepo { return &SubscriptionsRepo{} }

// SyncForWorkflow replaces the entire row set for workflowID with the new
// `slugs`. delete-all-then-insert-all in one tx — easy to reason about and
// tests fine; v0 workflow_targets are short slices so the throughput of a
// smarter "diff-based" sync isn't worth the complexity.
//
// The caller passes a pgx.Tx that they later commit. This repo never
// commits or rolls back on its own.
func (r *SubscriptionsRepo) SyncForWorkflow(ctx context.Context, tx pgx.Tx, workflowID uuid.UUID, slugs []string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM strategy_market_subscriptions WHERE workflow_id = $1`, workflowID); err != nil {
		return fmt.Errorf("strategy_market_subscriptions: SyncForWorkflow: delete: %w", err)
	}
	if len(slugs) == 0 {
		return nil
	}

	// pgx supports CopyFrom for fast bulk insert; for v0's tiny slice sizes,
	// a parameterized multi-row INSERT is plenty and avoids requiring a
	// CopyFromSource implementation.
	rows := make([][]any, 0, len(slugs))
	seen := make(map[string]bool, len(slugs))
	for _, s := range slugs {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		rows = append(rows, []any{workflowID, s})
	}
	if len(rows) == 0 {
		return nil
	}
	_, err := tx.CopyFrom(
		ctx,
		pgx.Identifier{"strategy_market_subscriptions"},
		[]string{"workflow_id", "market_slug"},
		pgx.CopyFromRows(rows),
	)
	if err != nil {
		return fmt.Errorf("strategy_market_subscriptions: SyncForWorkflow: copy: %w", err)
	}
	return nil
}

// ListForWorkflow returns the slugs currently subscribed for workflowID.
// Reads can run outside a tx — Querier is the right type here.
func (r *SubscriptionsRepo) ListForWorkflow(ctx context.Context, tx pgx.Tx, workflowID uuid.UUID) ([]string, error) {
	const sql = `
		SELECT market_slug FROM strategy_market_subscriptions
		WHERE workflow_id = $1
		ORDER BY market_slug ASC
	`
	rows, err := tx.Query(ctx, sql, workflowID)
	if err != nil {
		return nil, fmt.Errorf("strategy_market_subscriptions: ListForWorkflow: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("strategy_market_subscriptions: ListForWorkflow: scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
