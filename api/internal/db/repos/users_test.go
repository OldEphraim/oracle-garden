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

// TestUsersCRUD covers the happy-path Create/Get/Update flow. Expected-fail
// operations are split into TestUsersDuplicateEmail / TestUsersUpdatePasswordMissing
// because Postgres aborts the entire transaction on a constraint violation
// (SQLSTATE 25P02) — subsequent statements in the same tx fail until ROLLBACK.
// Splitting expected-fail tests gives each its own tx and isolates the failures.
func TestUsersCRUD(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		repo := NewUsersRepo()

		u, err := repo.Create(ctx, tx, CreateUserParams{
			Email:        uniqEmail(),
			PasswordHash: "$2a$10$bcrypt.fake.hash.value",
			DisplayName:  ptr("Alan"),
		})
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if u.ID == uuid.Nil {
			t.Fatalf("Create: empty ID")
		}

		got, err := repo.GetByEmail(ctx, tx, u.Email)
		if err != nil {
			t.Fatalf("GetByEmail: %v", err)
		}
		if got.ID != u.ID {
			t.Errorf("GetByEmail: id = %s want %s", got.ID, u.ID)
		}

		got2, err := repo.GetByID(ctx, tx, u.ID)
		if err != nil {
			t.Fatalf("GetByID: %v", err)
		}
		if got2.Email != u.Email {
			t.Errorf("GetByID: email = %q", got2.Email)
		}

		// Missing user → ErrNotFound. Doesn't poison the tx because no row
		// is touched — pgx.ErrNoRows is a client-side sentinel, not a DB
		// error.
		_, err = repo.GetByEmail(ctx, tx, "nobody-"+uniqEmail())
		if !errors.Is(err, db.ErrNotFound) {
			t.Errorf("GetByEmail nobody: want ErrNotFound, got %v", err)
		}

		// UpdatePassword on the existing user.
		if err := repo.UpdatePassword(ctx, tx, u.ID, "$2a$10$new.hash"); err != nil {
			t.Fatalf("UpdatePassword: %v", err)
		}
		got3, _ := repo.GetByID(ctx, tx, u.ID)
		if got3.PasswordHash != "$2a$10$new.hash" {
			t.Errorf("UpdatePassword: hash = %q", got3.PasswordHash)
		}

		// Empty hash rejected at the repo (no DB roundtrip).
		_, err = repo.Create(ctx, tx, CreateUserParams{Email: uniqEmail(), PasswordHash: ""})
		if err == nil || !strings.Contains(err.Error(), "empty password hash") {
			t.Errorf("empty hash: want repo-level error, got %v", err)
		}
	})
}

// TestUsersDuplicateEmail isolates the unique-violation path. The failed
// INSERT aborts the tx, so this is the last (and only) DB write the test
// performs.
func TestUsersDuplicateEmail(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		repo := NewUsersRepo()
		email := uniqEmail()

		if _, err := repo.Create(ctx, tx, CreateUserParams{Email: email, PasswordHash: "h1"}); err != nil {
			t.Fatalf("first Create: %v", err)
		}
		_, err := repo.Create(ctx, tx, CreateUserParams{Email: email, PasswordHash: "h2"})
		if err == nil || !db.IsUniqueViolation(err) {
			t.Errorf("duplicate Create: want IsUniqueViolation, got %v", err)
		}
	})
}

// TestUsersUpdatePasswordMissing — own tx because nothing depends on the
// failed-update state, and isolating it documents the contract on its own.
func TestUsersUpdatePasswordMissing(t *testing.T) {
	withTx(t, func(t *testing.T, tx pgx.Tx) {
		ctx := context.Background()
		repo := NewUsersRepo()
		err := repo.UpdatePassword(ctx, tx, uuid.New(), "x")
		if !errors.Is(err, db.ErrNotFound) {
			t.Errorf("UpdatePassword missing: want ErrNotFound, got %v", err)
		}
	})
}
