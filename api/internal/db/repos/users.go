// Package repos owns one repository per table. Each repo function takes
// the package's Querier (db.Querier) as its second arg so callers can pass
// the pool, a transaction, or — in tests — a transaction that will be
// rolled back at teardown. See db/db.go for the rationale.
//
// Domain types live alongside the repo that owns them. Field types are the
// natural Go shapes (uuid.UUID, time.Time, *string for nullable text). When
// a column is nullable we use a pointer; never sentinel values.
package repos

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
)

// User mirrors the users table.
type User struct {
	ID           uuid.UUID
	Email        string
	PasswordHash string  // bcrypt; never logged
	DisplayName  *string // nullable
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// UsersRepo is a stateless wrapper. Constructed once at startup; reused
// across goroutines.
type UsersRepo struct{}

// NewUsersRepo returns a UsersRepo. The function exists for symmetry with
// other repos that take dependencies (AgentTemplatesRepo wants a *types.Registry).
func NewUsersRepo() *UsersRepo { return &UsersRepo{} }

// CreateUserParams captures the fields that come from the signup handler.
// password_hash is supplied pre-hashed (bcrypt) by the handler — the repo
// does no hashing.
type CreateUserParams struct {
	Email        string
	PasswordHash string
	DisplayName  *string
}

// Create inserts a user row and returns the populated row (with id, timestamps).
// On duplicate email, returns an error for which db.IsUniqueViolation(err) returns true.
func (r *UsersRepo) Create(ctx context.Context, q db.Querier, p CreateUserParams) (*User, error) {
	if p.Email == "" {
		return nil, fmt.Errorf("users: Create: empty email")
	}
	if p.PasswordHash == "" {
		return nil, fmt.Errorf("users: Create: empty password hash")
	}
	const sql = `
		INSERT INTO users (email, password_hash, display_name)
		VALUES ($1, $2, $3)
		RETURNING id, email, password_hash, display_name, created_at, updated_at
	`
	var u User
	err := q.QueryRow(ctx, sql, p.Email, p.PasswordHash, p.DisplayName).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("users: Create: %w", err)
	}
	return &u, nil
}

// GetByEmail is the lookup the credential-verification endpoint uses.
// Returns db.ErrNotFound if no user matches.
func (r *UsersRepo) GetByEmail(ctx context.Context, q db.Querier, email string) (*User, error) {
	const sql = `
		SELECT id, email, password_hash, display_name, created_at, updated_at
		FROM users WHERE email = $1
	`
	var u User
	err := q.QueryRow(ctx, sql, email).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("users: GetByEmail: %w", db.MapNoRows(err))
	}
	return &u, nil
}

// GetByID is the lookup the JWT-middleware-attached user_id resolves to a
// User for a given request handler. Returns db.ErrNotFound if no user matches.
func (r *UsersRepo) GetByID(ctx context.Context, q db.Querier, id uuid.UUID) (*User, error) {
	const sql = `
		SELECT id, email, password_hash, display_name, created_at, updated_at
		FROM users WHERE id = $1
	`
	var u User
	err := q.QueryRow(ctx, sql, id).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &u.DisplayName, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("users: GetByID: %w", db.MapNoRows(err))
	}
	return &u, nil
}

// UpdatePassword replaces the password hash and bumps updated_at. The repo
// is intentionally narrow — there's no "change email" path in v0; that
// avoids re-validating against a unique index in the same flow.
func (r *UsersRepo) UpdatePassword(ctx context.Context, q db.Querier, id uuid.UUID, newHash string) error {
	if newHash == "" {
		return fmt.Errorf("users: UpdatePassword: empty hash")
	}
	const sql = `
		UPDATE users
		SET password_hash = $2, updated_at = NOW()
		WHERE id = $1
	`
	tag, err := q.Exec(ctx, sql, id, newHash)
	if err != nil {
		return fmt.Errorf("users: UpdatePassword: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return db.ErrNotFound
	}
	return nil
}
