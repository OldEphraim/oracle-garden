package repos

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
)

func TestAgentTemplatesCRUD(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		reg := testRegistry(t)
		repo := NewAgentTemplatesRepo(reg)
		owner := mustCreateUser(t, ctx, tx)

		// Create with valid types from the seeded registry.
		a, err := repo.Create(ctx, tx, CreateAgentTemplateParams{
			OwnerID:      &owner,
			Name:         "Test Watcher",
			SystemPrompt: "Observe markets.",
			InputTypes:   []string{"market_target.v1"},
			OutputType:   "observation.v1",
			Tools:        []string{"polymarket.gamma_get_market"},
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if a.Model != "claude-sonnet-4-6" {
			t.Errorf("default model: got %q want claude-sonnet-4-6", a.Model)
		}
		if a.Temperature != 0.7 {
			t.Errorf("default temperature: got %v want 0.7", a.Temperature)
		}
		if a.MaxTokens != 2000 {
			t.Errorf("default max_tokens: got %v want 2000", a.MaxTokens)
		}

		// GetByID
		got, err := repo.GetByID(ctx, tx, a.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got.Name != a.Name {
			t.Errorf("name mismatch: %q vs %q", got.Name, a.Name)
		}

		// Update — change description and types.
		newDesc := "Updated"
		newOutput := "thesis.v1"
		newInputs := []string{"observation.v1", "news_digest.v1"}
		updated, err := repo.Update(ctx, tx, a.ID, UpdateAgentTemplateParams{
			Description: &newDesc,
			OutputType:  &newOutput,
			InputTypes:  &newInputs,
		})
		if err != nil {
			t.Fatalf("Update: %v", err)
		}
		if updated.OutputType != "thesis.v1" {
			t.Errorf("update output_type: %q", updated.OutputType)
		}
		if got, want := updated.InputTypes, newInputs; !equalStrings(got, want) {
			t.Errorf("update input_types: %v vs %v", got, want)
		}

		// List by owner
		mineSystem := false
		out, err := repo.List(ctx, tx, ListFilter{OwnerID: &owner, IsSystem: &mineSystem})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(out) != 1 || out[0].ID != a.ID {
			t.Errorf("List by owner: %d results", len(out))
		}

		// Delete
		if err := repo.Delete(ctx, tx, a.ID); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		_, err = repo.GetByID(ctx, tx, a.ID)
		if !errors.Is(err, db.ErrNotFound) {
			t.Errorf("GetByID after Delete: want ErrNotFound, got %v", err)
		}

		// Empty filter returns empty.
		empty, _ := repo.List(ctx, tx, ListFilter{})
		if len(empty) != 0 {
			t.Errorf("empty filter: got %d results", len(empty))
		}
	})
}

func TestAgentTemplatesValidatesTypesOnCreate(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		reg := testRegistry(t)
		repo := NewAgentTemplatesRepo(reg)
		owner := mustCreateUser(t, ctx, tx)

		// Unknown output_type → hard error.
		_, err := repo.Create(ctx, tx, CreateAgentTemplateParams{
			OwnerID:      &owner,
			Name:         "Bad",
			SystemPrompt: "x",
			InputTypes:   []string{"observation.v1"},
			OutputType:   "totally_made_up.v1",
		})
		if err == nil || !strings.Contains(err.Error(), "totally_made_up.v1") {
			t.Errorf("unknown output_type: want error mentioning totally_made_up.v1, got %v", err)
		}

		// Unknown input_type → hard error, listing the missing type.
		_, err = repo.Create(ctx, tx, CreateAgentTemplateParams{
			OwnerID:      &owner,
			Name:         "Bad",
			SystemPrompt: "x",
			InputTypes:   []string{"observation.v1", "fake.v9"},
			OutputType:   "thesis.v1",
		})
		if err == nil || !strings.Contains(err.Error(), "fake.v9") {
			t.Errorf("unknown input_type: want error mentioning fake.v9, got %v", err)
		}
	})
}

func TestAgentTemplatesFork(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		reg := testRegistry(t)
		repo := NewAgentTemplatesRepo(reg)
		owner1 := mustCreateUser(t, ctx, tx)
		owner2 := mustCreateUser(t, ctx, tx)

		src, err := repo.Create(ctx, tx, CreateAgentTemplateParams{
			OwnerID:      &owner1,
			Name:         "Original",
			SystemPrompt: "x",
			InputTypes:   []string{"market_target.v1"},
			OutputType:   "observation.v1",
		})
		if err != nil {
			t.Fatalf("Create source: %v", err)
		}

		fork, err := repo.Fork(ctx, tx, src.ID, owner2)
		if err != nil {
			t.Fatalf("Fork: %v", err)
		}
		if fork.OwnerID == nil || *fork.OwnerID != owner2 {
			t.Errorf("fork owner: got %v want %s", fork.OwnerID, owner2)
		}
		if fork.ForkedFrom == nil || *fork.ForkedFrom != src.ID {
			t.Errorf("fork.ForkedFrom: got %v want %s", fork.ForkedFrom, src.ID)
		}
		if fork.IsSystem {
			t.Errorf("fork is_system: should be false")
		}
		if fork.Name != src.Name {
			t.Errorf("fork name: %q vs %q", fork.Name, src.Name)
		}
	})
}

// mustCreateUser inserts a user and returns their id, used by other repo tests.
func mustCreateUser(t *testing.T, ctx context.Context, tx pgx.Tx) uuid.UUID {
	t.Helper()
	u, err := NewUsersRepo().Create(ctx, tx, CreateUserParams{
		Email:        uniqEmail(),
		PasswordHash: "$2a$10$ignored.in.tests",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return u.ID
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
