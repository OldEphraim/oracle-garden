package repos

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestSystemConfigKillSwitch(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		repo := NewSystemConfigRepo()

		// Migration 000003 seeds kill_switch = false.
		on, err := repo.GetKillSwitch(ctx, tx)
		if err != nil {
			t.Fatalf("GetKillSwitch: %v", err)
		}
		if on {
			t.Errorf("seeded kill_switch should be false, got true")
		}

		// Flip to on.
		if err := repo.SetKillSwitch(ctx, tx, true); err != nil {
			t.Fatalf("SetKillSwitch true: %v", err)
		}
		on, _ = repo.GetKillSwitch(ctx, tx)
		if !on {
			t.Errorf("kill_switch after Set(true): want true")
		}

		// Flip back.
		_ = repo.SetKillSwitch(ctx, tx, false)
		on, _ = repo.GetKillSwitch(ctx, tx)
		if on {
			t.Errorf("kill_switch after Set(false): want false")
		}
	})
}
