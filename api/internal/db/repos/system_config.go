package repos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
)

// SystemConfigRepo is the read/write layer over system_config. v0 only stores
// kill_switch (per the seed migration); the repo is generic so v1+ can add
// further keys without touching the schema.
type SystemConfigRepo struct{}

func NewSystemConfigRepo() *SystemConfigRepo { return &SystemConfigRepo{} }

// GetKillSwitch returns the current kill_switch state. Returns false if the
// row is missing (defensive — admin endpoint can re-seed it).
func (r *SystemConfigRepo) GetKillSwitch(ctx context.Context, q db.Querier) (bool, error) {
	const sql = `SELECT value FROM system_config WHERE key = 'kill_switch'`
	var raw json.RawMessage
	err := q.QueryRow(ctx, sql).Scan(&raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("system_config: GetKillSwitch: %w", err)
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err != nil {
		return false, fmt.Errorf("system_config: GetKillSwitch: parse %q: %w", raw, err)
	}
	return b, nil
}

// SetKillSwitch flips the kill_switch and bumps updated_at. Upserts so the
// admin endpoint works even if the seed migration was rolled back.
func (r *SystemConfigRepo) SetKillSwitch(ctx context.Context, q db.Querier, on bool) error {
	value := []byte("false")
	if on {
		value = []byte("true")
	}
	const sql = `
		INSERT INTO system_config (key, value, updated_at)
		VALUES ('kill_switch', $1::jsonb, NOW())
		ON CONFLICT (key) DO UPDATE
		  SET value = EXCLUDED.value, updated_at = NOW()
	`
	if _, err := q.Exec(ctx, sql, value); err != nil {
		return fmt.Errorf("system_config: SetKillSwitch: %w", err)
	}
	return nil
}
