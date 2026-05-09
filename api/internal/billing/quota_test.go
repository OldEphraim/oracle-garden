package billing

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/OldEphraim/sibyl-hub/api/internal/db/repos"
)

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
		t.Skip("no TEST_DATABASE_URL or DATABASE_URL — skipping billing tests")
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

// withTx + helper user creation, mirroring the Phase 3 pattern.
func withTx(t *testing.T, fn func(t *testing.T, tx pgx.Tx, userID uuid.UUID, q *Quotas)) {
	t.Helper()
	p := testPool(t)
	tx, err := p.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	usersRepo := repos.NewUsersRepo()
	u, err := usersRepo.Create(context.Background(), tx, repos.CreateUserParams{
		Email:        fmt.Sprintf("test-%s@example.com", uuid.New().String()),
		PasswordHash: "$2a$10$ignored",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	q := NewQuotas(repos.NewUsageRepo())
	// Override caps for tests so we don't need to set env vars.
	q.MaxRunsPerDay = 3
	q.MaxCostUSDPerDay = 0.50
	fn(t, tx, u.ID, q)
}

// TestCheckQuotaInitialOK — fresh user has full budget.
func TestCheckQuotaInitialOK(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx, uid uuid.UUID, q *Quotas) {
		ok, reason, err := q.CheckQuota(context.Background(), tx, uid)
		if err != nil {
			t.Fatalf("CheckQuota: %v", err)
		}
		if !ok {
			t.Errorf("fresh user: ok=false reason=%q", reason)
		}
	})
}

// TestCheckQuotaOverRunCount — increment 3x → CheckQuota now returns false.
func TestCheckQuotaOverRunCount(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx, uid uuid.UUID, q *Quotas) {
		ctx := context.Background()
		for i := 0; i < 3; i++ {
			n, err := q.IncrementRunCount(ctx, tx, uid)
			if err != nil {
				t.Fatalf("IncrementRunCount #%d: %v", i+1, err)
			}
			if n != i+1 {
				t.Errorf("count #%d: got %d want %d", i+1, n, i+1)
			}
			if q.IsOverRunCount(n) {
				t.Errorf("count %d should not be over cap=3", n)
			}
		}
		ok, reason, err := q.CheckQuota(ctx, tx, uid)
		if err != nil {
			t.Fatalf("CheckQuota: %v", err)
		}
		if ok {
			t.Errorf("after 3 runs (cap=3) CheckQuota should report over")
		}
		if reason == "" {
			t.Errorf("reason should be populated")
		}
	})
}

// TestIncrementRunCountAtomicUpsert — same SQL pattern, two callers in one
// tx still get unique post-increment values.
func TestIncrementRunCountAtomicUpsert(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx, uid uuid.UUID, q *Quotas) {
		ctx := context.Background()
		seen := map[int]bool{}
		for i := 0; i < 5; i++ {
			n, err := q.IncrementRunCount(ctx, tx, uid)
			if err != nil {
				t.Fatalf("IncrementRunCount: %v", err)
			}
			if seen[n] {
				t.Errorf("duplicate count %d", n)
			}
			seen[n] = true
		}
		// Counts should be 1..5.
		for i := 1; i <= 5; i++ {
			if !seen[i] {
				t.Errorf("missing count %d in seen=%v", i, seen)
			}
		}
	})
}

// TestRecordStepCostUpsertAndCap — cost upsert returns running total; the
// IsOverCostUSD helper fires once the threshold is crossed.
func TestRecordStepCostUpsertAndCap(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx, uid uuid.UUID, q *Quotas) {
		ctx := context.Background()
		q.MaxCostUSDPerDay = 0.10

		// Three increments of $0.04, $0.04, $0.05 — totals 0.04, 0.08, 0.13.
		amounts := []float64{0.04, 0.04, 0.05}
		expected := []float64{0.04, 0.08, 0.13}
		for i, amt := range amounts {
			total, err := q.RecordStepCost(ctx, tx, uid, amt, int64((i+1)*100))
			if err != nil {
				t.Fatalf("RecordStepCost #%d: %v", i, err)
			}
			if !approxFloat(total, expected[i], 0.001) {
				t.Errorf("step %d total: got %v want %v", i, total, expected[i])
			}
		}

		// Final total ($0.13) is over the $0.10 cap.
		ok, reason, err := q.CheckQuota(ctx, tx, uid)
		if err != nil {
			t.Fatalf("CheckQuota: %v", err)
		}
		if ok {
			t.Errorf("after total $0.13 over cap $0.10, CheckQuota should report over")
		}
		if reason == "" || !contains(reason, "cost cap") {
			t.Errorf("reason should mention cost cap: %q", reason)
		}
	})
}

// TestRecordStepCostFirstCallSetsRunCountZero — the INSERT branch's
// `run_count = 0` doesn't accidentally bump run_count when the row is
// created by the first cost-recording call (e.g. an admin-bypass run).
func TestRecordStepCostFirstCallSetsRunCountZero(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx, uid uuid.UUID, q *Quotas) {
		ctx := context.Background()
		// Fresh user — no prior IncrementRunCount.
		_, err := q.RecordStepCost(ctx, tx, uid, 0.01, 100)
		if err != nil {
			t.Fatalf("RecordStepCost: %v", err)
		}
		// Inspect the row directly.
		usage, err := q.repo.GetTodayUsage(ctx, tx, uid)
		if err != nil {
			t.Fatalf("GetTodayUsage: %v", err)
		}
		if usage.RunCount != 0 {
			t.Errorf("first-cost-call should leave run_count at 0; got %d", usage.RunCount)
		}
		if usage.TotalTokens != 100 {
			t.Errorf("total_tokens: got %d want 100", usage.TotalTokens)
		}
		if !approxFloat(usage.TotalCostUSD, 0.01, 1e-6) {
			t.Errorf("total_cost_usd: got %v want 0.01", usage.TotalCostUSD)
		}

		// A subsequent IncrementRunCount should cleanly set run_count to 1
		// without touching cost columns.
		n, err := q.IncrementRunCount(ctx, tx, uid)
		if err != nil {
			t.Fatalf("IncrementRunCount: %v", err)
		}
		if n != 1 {
			t.Errorf("post-cost IncrementRunCount: got %d want 1", n)
		}
		usage2, _ := q.repo.GetTodayUsage(ctx, tx, uid)
		if !approxFloat(usage2.TotalCostUSD, 0.01, 1e-6) {
			t.Errorf("cost should be unchanged: got %v want 0.01", usage2.TotalCostUSD)
		}
	})
}

// TestNewQuotasReadsEnvDefaults — setting envs picks them up; bad values
// fall back to defaults.
func TestNewQuotasReadsEnvDefaults(t *testing.T) {
	cases := []struct {
		runEnv, costEnv     string
		wantRuns            int
		wantCost            float64
	}{
		{"", "", DefaultMaxRunsPerDay, DefaultMaxCostUSDPerDay},
		{"100", "10.5", 100, 10.5},
		{"garbage", "garbage", DefaultMaxRunsPerDay, DefaultMaxCostUSDPerDay},
		{"-5", "-1", DefaultMaxRunsPerDay, DefaultMaxCostUSDPerDay},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q/%q", c.runEnv, c.costEnv), func(t *testing.T) {
			t.Setenv(envMaxRunsPerDay, c.runEnv)
			t.Setenv(envMaxCostUSDPerDay, c.costEnv)
			q := NewQuotas(nil)
			if q.MaxRunsPerDay != c.wantRuns {
				t.Errorf("MaxRunsPerDay: got %d want %d", q.MaxRunsPerDay, c.wantRuns)
			}
			if !approxFloat(q.MaxCostUSDPerDay, c.wantCost, 1e-9) {
				t.Errorf("MaxCostUSDPerDay: got %v want %v", q.MaxCostUSDPerDay, c.wantCost)
			}
		})
	}
}

func approxFloat(a, b, eps float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < eps
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(sub) > 0 && stringIndex(s, sub) >= 0))
}

func stringIndex(s, sub string) int {
	if len(sub) == 0 {
		return 0
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
