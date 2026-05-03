package repos

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestUsageAtomicIncrement(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		uid := mustCreateUser(t, ctx, tx)
		repo := NewUsageRepo()

		// First IncrementRun creates the row, returns 1.
		n1, err := repo.IncrementRun(ctx, tx, uid)
		if err != nil {
			t.Fatalf("IncrementRun #1: %v", err)
		}
		if n1 != 1 {
			t.Errorf("IncrementRun #1: got %d want 1", n1)
		}
		// Second call uses the upsert + returns 2.
		n2, _ := repo.IncrementRun(ctx, tx, uid)
		if n2 != 2 {
			t.Errorf("IncrementRun #2: got %d want 2", n2)
		}

		// IncrementCost — atomic upsert returning post-increment cost.
		c1, err := repo.IncrementCost(ctx, tx, uid, 1000, 0.05)
		if err != nil {
			t.Fatalf("IncrementCost #1: %v", err)
		}
		if c1 < 0.0499 || c1 > 0.0501 {
			t.Errorf("IncrementCost #1: got %v want ~0.05", c1)
		}
		c2, _ := repo.IncrementCost(ctx, tx, uid, 500, 0.02)
		if c2 < 0.0699 || c2 > 0.0701 {
			t.Errorf("IncrementCost #2: got %v want ~0.07", c2)
		}

		// GetTodayUsage rolls all that up.
		today, err := repo.GetTodayUsage(ctx, tx, uid)
		if err != nil {
			t.Fatalf("GetTodayUsage: %v", err)
		}
		if today.RunCount != 2 {
			t.Errorf("today.RunCount: got %d want 2", today.RunCount)
		}
		if today.TotalTokens != 1500 {
			t.Errorf("today.TotalTokens: got %d want 1500", today.TotalTokens)
		}
		if today.TotalCostUSD < 0.0699 || today.TotalCostUSD > 0.0701 {
			t.Errorf("today.TotalCostUSD: got %v want ~0.07", today.TotalCostUSD)
		}
	})
}

func TestUsageGetTodayReturnsZeroForMissing(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		uid := mustCreateUser(t, ctx, tx)
		repo := NewUsageRepo()

		// Brand-new user has no usage row. Should return zero values, not ErrNotFound.
		u, err := repo.GetTodayUsage(ctx, tx, uid)
		if err != nil {
			t.Fatalf("GetTodayUsage: %v", err)
		}
		if u.RunCount != 0 || u.TotalTokens != 0 || u.TotalCostUSD != 0 {
			t.Errorf("expected zeroed usage, got %+v", u)
		}
	})
}
