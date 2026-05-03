// Package db owns the pgx connection pool and the Querier abstraction every
// repo accepts. Repos take a Querier as their first runtime arg so the same
// code path works inside and outside transactions:
//
//   - Outside a tx: pass the pool directly. Each call uses one connection.
//   - Inside a tx:  pass the pgx.Tx. All calls share the same conn and the
//     same isolation snapshot; the caller commits or rolls back.
//
// This pattern is what makes the repo tests' transactional-rollback strategy
// possible (each test wraps its work in a tx, rolls back at teardown), and
// what lets the workflow-save endpoint atomically update workflows AND
// strategy_market_subscriptions in one transaction (CLAUDE.md cross-table
// invariant).
//
// Repos do NOT implicitly begin transactions. If a repo function needs to be
// atomic across multiple statements, the caller is expected to begin a tx
// and pass it. The one exception is the SubscriptionsRepo's SyncForWorkflow,
// which takes pgx.Tx specifically (typed parameter, not Querier) because its
// delete-then-insert contract is meaningless without a tx.
package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the minimal pgx surface every repo function targets. Both
// *pgxpool.Pool and pgx.Tx satisfy this; tests substitute one for the other
// without the repo knowing.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// NewPool opens a pgx pool from a Postgres DSN. Caller closes via pool.Close()
// at shutdown. Sets sensible defaults for v0; specific tuning is a Phase 18
// (deployment) concern.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("db: parse DSN: %w", err)
	}
	// v0 defaults — generous; the cap is the Postgres max_connections, and
	// our v0 traffic is single-digit concurrent runs.
	cfg.MaxConns = 16
	cfg.MinConns = 1

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("db: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("db: ping: %w", err)
	}
	return pool, nil
}

// ErrNotFound is returned by repo Get* functions when the row doesn't exist.
// Callers should compare via errors.Is(err, db.ErrNotFound).
var ErrNotFound = errors.New("db: not found")

// IsUniqueViolation reports whether err is a Postgres unique-constraint
// violation (SQLSTATE 23505). Used in handlers to surface "email already
// taken" 409s without parsing error strings.
func IsUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// IsForeignKeyViolation reports whether err is a Postgres FK-constraint
// violation (SQLSTATE 23503). Useful for surfacing "agent_template not found"
// when inserting a workflow_node, etc.
func IsForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

// MapNoRows folds pgx's "no rows" sentinel into our ErrNotFound so handlers
// have one error type to branch on.
func MapNoRows(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
