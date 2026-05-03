package types

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	tpoolOnce sync.Once
	tpool     *pgxpool.Pool
)

func openPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	tpoolOnce.Do(func() {
		dsn := os.Getenv("TEST_DATABASE_URL")
		if dsn == "" {
			dsn = os.Getenv("DATABASE_URL")
		}
		if dsn == "" {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p, err := pgxpool.New(ctx, dsn)
		if err == nil {
			tpool = p
		}
	})
	if tpool == nil {
		t.Skip("no TEST_DATABASE_URL or DATABASE_URL — skipping registry tests")
	}
	return tpool
}

// TestRegistryLoadsCoreTypes verifies the registry reads the seeded core
// types from migration 000002 and compiles every one. Failure here likely
// means a JSON Schema in TYPES.md drifted from what jsonschema/v6 accepts.
func TestRegistryLoadsCoreTypes(t *testing.T) {
	p := openPool(t)
	reg, err := Load(context.Background(), p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{
		"market_target.v1",
		"observation.v1",
		"news_digest.v1",
		"thesis.v1",
		"risk_assessment.v1",
		"trading_decision.v1",
	}
	for _, id := range want {
		if _, err := reg.Get(id); err != nil {
			t.Errorf("Get(%q): %v", id, err)
		}
	}
	all := reg.All()
	if len(all) < len(want) {
		t.Errorf("All(): %d entries, want >= %d", len(all), len(want))
	}
	// All() is sorted by ID — sanity check the prefix.
	if all[0].ID() != "market_target.v1" {
		t.Errorf("All()[0]: got %s want market_target.v1 (sorted by id)", all[0].ID())
	}
}

// TestRegistryGetUnknown verifies the typed error.
func TestRegistryGetUnknown(t *testing.T) {
	p := openPool(t)
	reg, err := Load(context.Background(), p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = reg.Get("does_not_exist.v1")
	if err == nil {
		t.Fatalf("expected error")
	}
	var ute *ErrUnknownType
	if !errorsAs(err, &ute) {
		t.Errorf("expected *ErrUnknownType, got %T: %v", err, err)
	}
}

// TestRegistryMustExist documents the multi-error reporting behavior used by
// AgentTemplatesRepo.Create at submission time.
func TestRegistryMustExist(t *testing.T) {
	p := openPool(t)
	reg, _ := Load(context.Background(), p)

	// Known types pass.
	if err := reg.MustExist("observation.v1", "thesis.v1"); err != nil {
		t.Errorf("MustExist with known: %v", err)
	}
	// Unknown types fail; error should mention each one.
	err := reg.MustExist("observation.v1", "fake_a.v1", "fake_b.v1")
	if err == nil {
		t.Fatalf("expected error")
	}
	if !strings.Contains(err.Error(), "fake_a.v1") || !strings.Contains(err.Error(), "fake_b.v1") {
		t.Errorf("expected both unknown types listed, got: %v", err)
	}
}

// TestValidateAgainstObservationV1 covers the happy path AND a representative
// failure mode (missing required field) for the observation.v1 schema —
// the most-checked schema in v0.
func TestValidateAgainstObservationV1(t *testing.T) {
	p := openPool(t)
	reg, _ := Load(context.Background(), p)

	good := []byte(`{
		"market_slug":"x",
		"condition_id":"0xabc",
		"question":"q",
		"current_price_yes":0.5,
		"current_price_no":0.5,
		"volume_24h_usd":100,
		"liquidity_usd":50,
		"time_to_resolution_hours":24,
		"yes_token_id":"y",
		"no_token_id":"n"
	}`)
	if errs, err := reg.ValidateAgainst(good, "observation.v1"); err != nil || len(errs) != 0 {
		t.Errorf("good payload: errs=%v err=%v", errs, err)
	}

	// Missing required field 'liquidity_usd'.
	bad := []byte(`{
		"market_slug":"x",
		"condition_id":"0xabc",
		"question":"q",
		"current_price_yes":0.5,
		"current_price_no":0.5,
		"volume_24h_usd":100,
		"time_to_resolution_hours":24,
		"yes_token_id":"y",
		"no_token_id":"n"
	}`)
	errs, err := reg.ValidateAgainst(bad, "observation.v1")
	if err != nil {
		t.Fatalf("ValidateAgainst: %v", err)
	}
	if len(errs) == 0 {
		t.Fatalf("expected validation errors, got none")
	}
	joined := joinValidationErrors(errs)
	if !strings.Contains(joined, "liquidity_usd") {
		t.Errorf("expected error to mention liquidity_usd, got: %s", joined)
	}

	// Invalid JSON — different error path.
	if _, err := reg.ValidateAgainst([]byte(`not-json`), "observation.v1"); err == nil {
		t.Errorf("expected error for non-JSON input")
	}

	// Unknown type — *ErrUnknownType.
	if _, err := reg.ValidateAgainst(good, "fake.v1"); err == nil {
		t.Errorf("expected error for unknown type")
	}
}

func joinValidationErrors(errs []ValidationError) string {
	var sb strings.Builder
	for i, e := range errs {
		if i > 0 {
			sb.WriteString("; ")
		}
		sb.WriteString(e.String())
	}
	return sb.String()
}

func errorsAs(err error, target any) bool {
	return errors.As(err, target)
}
