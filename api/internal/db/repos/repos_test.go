// Test setup shared by every *_test.go in this package.
//
// Test strategy: each test wraps its work in a pgx.Tx that's rolled back at
// teardown — no data persists between tests, no Docker per-test, fast.
// Migrations must be applied to the test DB up front (`make migrate-up`).
//
// Connection: TEST_DATABASE_URL if set, else DATABASE_URL. Tests skip with
// t.Skip(...) if neither is set, so `go test` still works in CI containers
// that don't run a Postgres alongside.
package repos

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OldEphraim/sibyl-hub/api/internal/types"
)

var (
	poolOnce sync.Once
	poolErr  error
	pool     *pgxpool.Pool

	registryOnce sync.Once
	registryErr  error
	registry     *types.Registry
)

// testPool returns a process-wide *pgxpool.Pool against the test DB. Tests
// borrow a connection from it via Begin() and roll back at teardown.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	poolOnce.Do(func() {
		dsn := firstNonEmpty(os.Getenv("TEST_DATABASE_URL"), os.Getenv("DATABASE_URL"))
		if dsn == "" {
			return // poolErr stays nil; t.Skip below
		}
		ctx, cancel := context.WithTimeout(context.Background(), poolDialTimeout)
		defer cancel()
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			poolErr = err
			return
		}
		cfg.MaxConns = 4 // tests don't need much
		p, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			poolErr = err
			return
		}
		if err := p.Ping(ctx); err != nil {
			p.Close()
			poolErr = err
			return
		}
		pool = p
	})
	if pool == nil && poolErr == nil {
		t.Skip("no TEST_DATABASE_URL or DATABASE_URL — skipping repo tests")
	}
	if poolErr != nil {
		t.Fatalf("test pool: %v", poolErr)
	}
	return pool
}

// testRegistry loads the type registry once per process. Reads from the pool
// (committed view) so it sees the seeded core types from migration 000002.
// Tests can rely on observation.v1, thesis.v1, etc. existing in the registry.
func testRegistry(t *testing.T) *types.Registry {
	t.Helper()
	p := testPool(t) // skips here if pool is unavailable
	registryOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), poolDialTimeout)
		defer cancel()
		registry, registryErr = types.Load(ctx, p)
	})
	if registryErr != nil {
		t.Fatalf("load registry: %v", registryErr)
	}
	return registry
}

// withTx begins a transaction, runs fn against it, and rolls back. Use for
// any test that touches the DB — it's how we keep tests isolated without
// per-test container provisioning.
func withTx(t *testing.T, fn func(t *testing.T, tx pgx.Tx)) {
	t.Helper()
	p := testPool(t)
	ctx := context.Background()
	tx, err := p.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() {
		if rbErr := tx.Rollback(ctx); rbErr != nil && rbErr != pgx.ErrTxClosed {
			t.Logf("rollback: %v", rbErr)
		}
	}()
	fn(t, tx)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// pointer literals — handy in test param construction.
func ptr[T any](v T) *T { return &v }

// uniqEmail returns an email guaranteed unique within the test process.
// Each test runs inside a tx that gets rolled back, but uniqueness within
// the tx still matters for duplicate-email test cases.
func uniqEmail() string {
	id := uuid.New().String()
	return "test+" + id + "@example.com"
}

const poolDialTimeout = 5 * time.Second
