package repos

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
	"github.com/OldEphraim/sibyl-hub/api/internal/types"
)

// AgentTemplate mirrors the agent_templates table.
type AgentTemplate struct {
	ID            uuid.UUID
	OwnerID       *uuid.UUID // NULL for system templates
	Name          string
	Description   *string
	SystemPrompt  string
	Model         string
	Temperature   float32
	MaxTokens     int
	InputTypes    []string
	OutputType    string
	Tools         []string
	Visibility    string // 'private' | 'unlisted' | 'public'
	ForkedFrom    *uuid.UUID
	IsSystem      bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// AgentTemplatesRepo is one of the few repos with a registry dependency.
// Keeping the type-existence check at the persistence boundary (rather than
// in handlers) makes "you can't reference an unregistered type" an invariant
// that no caller can forget.
type AgentTemplatesRepo struct {
	registry *types.Registry
}

// NewAgentTemplatesRepo accepts the loaded type registry. The registry is
// required — passing nil panics, because every Create/Update call validates
// declared types against it.
func NewAgentTemplatesRepo(registry *types.Registry) *AgentTemplatesRepo {
	if registry == nil {
		panic("repos: NewAgentTemplatesRepo: nil registry")
	}
	return &AgentTemplatesRepo{registry: registry}
}

// CreateAgentTemplateParams captures the inputs to a Create call. Defaults
// match the schema (model = 'claude-sonnet-4-6' if empty, temperature = 0.7
// if zero, max_tokens = 2000 if zero).
type CreateAgentTemplateParams struct {
	OwnerID      *uuid.UUID // nil for system templates
	Name         string
	Description  *string
	SystemPrompt string
	Model        string
	Temperature  float32
	MaxTokens    int
	InputTypes   []string
	OutputType   string
	Tools        []string
	Visibility   string
	ForkedFrom   *uuid.UUID
	IsSystem     bool
}

// Create inserts an agent_template row. Validates that every InputType and
// OutputType is registered before the INSERT — a row that references an
// unknown type would be useless (no agent step could ever validate its
// output) and we'd rather reject at create time than at first run.
func (r *AgentTemplatesRepo) Create(ctx context.Context, q db.Querier, p CreateAgentTemplateParams) (*AgentTemplate, error) {
	if p.Name == "" {
		return nil, fmt.Errorf("agent_templates: Create: empty name")
	}
	if p.SystemPrompt == "" {
		return nil, fmt.Errorf("agent_templates: Create: empty system_prompt")
	}
	if p.OutputType == "" {
		return nil, fmt.Errorf("agent_templates: Create: empty output_type")
	}
	if err := r.registry.MustExist(append([]string{p.OutputType}, p.InputTypes...)...); err != nil {
		return nil, fmt.Errorf("agent_templates: Create: %w", err)
	}

	model := p.Model
	if model == "" {
		model = "claude-sonnet-4-6"
	}
	temperature := p.Temperature
	if temperature == 0 {
		temperature = 0.7
	}
	maxTokens := p.MaxTokens
	if maxTokens == 0 {
		maxTokens = 2000
	}
	visibility := p.Visibility
	if visibility == "" {
		visibility = "private"
	}
	tools := p.Tools
	if tools == nil {
		tools = []string{}
	}

	const sql = `
		INSERT INTO agent_templates (
			owner_id, name, description, system_prompt,
			model, temperature, max_tokens,
			input_types, output_type, tools,
			visibility, forked_from, is_system
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id, owner_id, name, description, system_prompt,
		          model, temperature, max_tokens,
		          input_types, output_type, tools,
		          visibility, forked_from, is_system,
		          created_at, updated_at
	`
	var a AgentTemplate
	err := q.QueryRow(ctx, sql,
		p.OwnerID, p.Name, p.Description, p.SystemPrompt,
		model, temperature, maxTokens,
		p.InputTypes, p.OutputType, tools,
		visibility, p.ForkedFrom, p.IsSystem,
	).Scan(
		&a.ID, &a.OwnerID, &a.Name, &a.Description, &a.SystemPrompt,
		&a.Model, &a.Temperature, &a.MaxTokens,
		&a.InputTypes, &a.OutputType, &a.Tools,
		&a.Visibility, &a.ForkedFrom, &a.IsSystem,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("agent_templates: Create: %w", err)
	}
	return &a, nil
}

// GetByID returns one template by id. db.ErrNotFound if missing.
func (r *AgentTemplatesRepo) GetByID(ctx context.Context, q db.Querier, id uuid.UUID) (*AgentTemplate, error) {
	const sql = `
		SELECT id, owner_id, name, description, system_prompt,
		       model, temperature, max_tokens,
		       input_types, output_type, tools,
		       visibility, forked_from, is_system,
		       created_at, updated_at
		FROM agent_templates WHERE id = $1
	`
	var a AgentTemplate
	err := q.QueryRow(ctx, sql, id).Scan(
		&a.ID, &a.OwnerID, &a.Name, &a.Description, &a.SystemPrompt,
		&a.Model, &a.Temperature, &a.MaxTokens,
		&a.InputTypes, &a.OutputType, &a.Tools,
		&a.Visibility, &a.ForkedFrom, &a.IsSystem,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("agent_templates: GetByID: %w", db.MapNoRows(err))
	}
	return &a, nil
}

// ListFilter narrows a List call to the rows the caller wants. All fields
// are optional; the zero value returns an empty list (refusing to dump the
// entire table by accident).
type ListFilter struct {
	OwnerID    *uuid.UUID // include owner_id = X
	IsSystem   *bool      // include is_system = X
	Visibility *string    // include visibility = X (e.g. "public")
	Limit      int        // 0 → 50
}

// List returns templates matching the filter. The shape mirrors the
// CLAUDE.md per-resource access rules — handlers compose a filter that
// reflects "mine" + "system" + "public", not the repo (so the access policy
// stays in the handler layer).
//
// Visibility on its own is OR-able with owner/system; the filter struct
// expresses one branch at a time. To compose multiple branches, the handler
// calls List twice (once per branch) and merges results — keeps repo SQL
// trivial and predictable.
func (r *AgentTemplatesRepo) List(ctx context.Context, q db.Querier, f ListFilter) ([]*AgentTemplate, error) {
	if f.OwnerID == nil && f.IsSystem == nil && f.Visibility == nil {
		return []*AgentTemplate{}, nil
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}

	args := []any{}
	where := ""
	add := func(clause string, v any) {
		args = append(args, v)
		if where == "" {
			where = "WHERE " + clause
		} else {
			where += " AND " + clause
		}
		// Substitute $N
		where = substitutePlaceholder(where, len(args))
	}
	if f.OwnerID != nil {
		add("owner_id = ?", *f.OwnerID)
	}
	if f.IsSystem != nil {
		add("is_system = ?", *f.IsSystem)
	}
	if f.Visibility != nil {
		add("visibility = ?", *f.Visibility)
	}
	args = append(args, limit)
	limitParam := substitutePlaceholder("?", len(args))

	sql := `
		SELECT id, owner_id, name, description, system_prompt,
		       model, temperature, max_tokens,
		       input_types, output_type, tools,
		       visibility, forked_from, is_system,
		       created_at, updated_at
		FROM agent_templates
		` + where + `
		ORDER BY created_at DESC
		LIMIT ` + limitParam
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("agent_templates: List: %w", err)
	}
	defer rows.Close()

	var out []*AgentTemplate
	for rows.Next() {
		var a AgentTemplate
		if err := rows.Scan(
			&a.ID, &a.OwnerID, &a.Name, &a.Description, &a.SystemPrompt,
			&a.Model, &a.Temperature, &a.MaxTokens,
			&a.InputTypes, &a.OutputType, &a.Tools,
			&a.Visibility, &a.ForkedFrom, &a.IsSystem,
			&a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("agent_templates: List: scan: %w", err)
		}
		out = append(out, &a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("agent_templates: List: rows: %w", err)
	}
	return out, nil
}

// UpdateAgentTemplateParams is intentionally per-field optional (pointers).
// nil means "leave alone"; a non-nil value (including an empty slice) means
// "set to this".
type UpdateAgentTemplateParams struct {
	Name         *string
	Description  *string
	SystemPrompt *string
	Model        *string
	Temperature  *float32
	MaxTokens    *int
	InputTypes   *[]string
	OutputType   *string
	Tools        *[]string
	Visibility   *string
}

// Update applies the non-nil fields to the row. Re-validates types if either
// InputTypes or OutputType is being changed.
func (r *AgentTemplatesRepo) Update(ctx context.Context, q db.Querier, id uuid.UUID, p UpdateAgentTemplateParams) (*AgentTemplate, error) {
	// Type validation: gather the new effective types (existing values for
	// unchanged fields), check against the registry.
	if p.InputTypes != nil || p.OutputType != nil {
		current, err := r.GetByID(ctx, q, id)
		if err != nil {
			return nil, err
		}
		out := current.OutputType
		if p.OutputType != nil {
			out = *p.OutputType
		}
		in := current.InputTypes
		if p.InputTypes != nil {
			in = *p.InputTypes
		}
		if err := r.registry.MustExist(append([]string{out}, in...)...); err != nil {
			return nil, fmt.Errorf("agent_templates: Update: %w", err)
		}
	}

	// Build SET clause dynamically.
	args := []any{id}
	sets := []string{}
	addSet := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, fmt.Sprintf("%s = $%d", col, len(args)))
	}
	if p.Name != nil {
		addSet("name", *p.Name)
	}
	if p.Description != nil {
		addSet("description", *p.Description)
	}
	if p.SystemPrompt != nil {
		addSet("system_prompt", *p.SystemPrompt)
	}
	if p.Model != nil {
		addSet("model", *p.Model)
	}
	if p.Temperature != nil {
		addSet("temperature", *p.Temperature)
	}
	if p.MaxTokens != nil {
		addSet("max_tokens", *p.MaxTokens)
	}
	if p.InputTypes != nil {
		addSet("input_types", *p.InputTypes)
	}
	if p.OutputType != nil {
		addSet("output_type", *p.OutputType)
	}
	if p.Tools != nil {
		addSet("tools", *p.Tools)
	}
	if p.Visibility != nil {
		addSet("visibility", *p.Visibility)
	}
	if len(sets) == 0 {
		return r.GetByID(ctx, q, id)
	}
	sets = append(sets, "updated_at = NOW()")

	sql := "UPDATE agent_templates SET " + joinComma(sets) + " WHERE id = $1 RETURNING " +
		"id, owner_id, name, description, system_prompt, model, temperature, max_tokens, " +
		"input_types, output_type, tools, visibility, forked_from, is_system, created_at, updated_at"

	var a AgentTemplate
	err := q.QueryRow(ctx, sql, args...).Scan(
		&a.ID, &a.OwnerID, &a.Name, &a.Description, &a.SystemPrompt,
		&a.Model, &a.Temperature, &a.MaxTokens,
		&a.InputTypes, &a.OutputType, &a.Tools,
		&a.Visibility, &a.ForkedFrom, &a.IsSystem,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, db.ErrNotFound
		}
		return nil, fmt.Errorf("agent_templates: Update: %w", err)
	}
	return &a, nil
}

// Delete removes the row. Returns db.ErrNotFound if nothing matched. The
// owner-check (CLAUDE.md per-resource rules) lives in the handler, not here
// — repos are policy-free.
func (r *AgentTemplatesRepo) Delete(ctx context.Context, q db.Querier, id uuid.UUID) error {
	tag, err := q.Exec(ctx, "DELETE FROM agent_templates WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("agent_templates: Delete: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return db.ErrNotFound
	}
	return nil
}

// Fork copies an existing template into a new row owned by newOwnerID, with
// forked_from set to the source id. is_system is reset to false on forks
// (forks are user-owned even when the source is a system template).
func (r *AgentTemplatesRepo) Fork(ctx context.Context, q db.Querier, sourceID, newOwnerID uuid.UUID) (*AgentTemplate, error) {
	const sql = `
		INSERT INTO agent_templates (
			owner_id, name, description, system_prompt,
			model, temperature, max_tokens,
			input_types, output_type, tools,
			visibility, forked_from, is_system
		)
		SELECT $1,
		       name, description, system_prompt,
		       model, temperature, max_tokens,
		       input_types, output_type, tools,
		       'private', id, FALSE
		FROM agent_templates WHERE id = $2
		RETURNING id, owner_id, name, description, system_prompt,
		          model, temperature, max_tokens,
		          input_types, output_type, tools,
		          visibility, forked_from, is_system,
		          created_at, updated_at
	`
	var a AgentTemplate
	err := q.QueryRow(ctx, sql, newOwnerID, sourceID).Scan(
		&a.ID, &a.OwnerID, &a.Name, &a.Description, &a.SystemPrompt,
		&a.Model, &a.Temperature, &a.MaxTokens,
		&a.InputTypes, &a.OutputType, &a.Tools,
		&a.Visibility, &a.ForkedFrom, &a.IsSystem,
		&a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("agent_templates: Fork: %w", db.MapNoRows(err))
	}
	return &a, nil
}
