// Package types is Sibyl Hub's type registry: the in-memory home for the
// JSON Schemas declared in the type_definitions table. Schemas are loaded
// once at startup and compiled with santhosh-tekuri/jsonschema/v6 so each
// per-step validation in the agent runtime hits a cached compiled schema
// rather than re-parsing the raw JSON Schema text.
//
// The single piece of state that callers should hold across the program is
// a *Registry. Build it once at startup with Load(ctx, q) and pass it where
// it's needed (currently the agent_templates repo, in v1+ the engine and
// the agent runtime).
package types

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// querier is a minimal subset of pgx's Querier. The types package is its own
// dependency island so it doesn't import internal/db; we redeclare the bit we
// use to avoid a circular import (db.Querier -> types -> db).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Definition is the in-memory record of one row from type_definitions.
type Definition struct {
	Name        string
	Version     string
	Description string
	IsCore      bool
	RawSchema   []byte           // verbatim JSON Schema bytes from DB
	Compiled    *jsonschema.Schema // pre-compiled validator
}

// ID returns the canonical "<name>.<version>" identifier (e.g. "thesis.v1").
func (d *Definition) ID() string { return d.Name + "." + d.Version }

// Registry is the read-mostly map of typeID → compiled schema. Safe for
// concurrent use. The map is built once via Load(); Reload() rebuilds it
// atomically and may be called by an admin endpoint in v1+.
type Registry struct {
	mu  sync.RWMutex
	all map[string]*Definition // keyed by ID()
}

// Load reads all rows from type_definitions, compiles each schema, and
// returns a populated registry. Returns an error if any schema fails to
// compile — a registry that's missing a core type would silently break
// agent validation, so we fail fast at startup.
func Load(ctx context.Context, q querier) (*Registry, error) {
	r := &Registry{all: make(map[string]*Definition)}
	if err := r.reload(ctx, q); err != nil {
		return nil, err
	}
	return r, nil
}

// Reload rebuilds the registry from the DB. The new map replaces the old
// atomically; in-flight Get/Validate calls still see the prior compiled
// schema until they next acquire the read lock.
func (r *Registry) Reload(ctx context.Context, q querier) error {
	return r.reload(ctx, q)
}

func (r *Registry) reload(ctx context.Context, q querier) error {
	rows, err := q.Query(ctx, `
		SELECT name, version, json_schema::text, COALESCE(description, ''), is_core
		FROM type_definitions
	`)
	if err != nil {
		return fmt.Errorf("types: load: query: %w", err)
	}
	defer rows.Close()

	next := make(map[string]*Definition)
	for rows.Next() {
		var name, version, schemaText, description string
		var isCore bool
		if err := rows.Scan(&name, &version, &schemaText, &description, &isCore); err != nil {
			return fmt.Errorf("types: load: scan: %w", err)
		}
		def, err := compileDefinition(name, version, description, isCore, []byte(schemaText))
		if err != nil {
			return err
		}
		next[def.ID()] = def
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("types: load: rows: %w", err)
	}

	r.mu.Lock()
	r.all = next
	r.mu.Unlock()
	return nil
}

// compileDefinition compiles one schema and wraps both successful and failed
// outcomes with enough context for a startup-time error to be diagnostic.
func compileDefinition(name, version, description string, isCore bool, raw []byte) (*Definition, error) {
	id := name + "." + version
	parsed, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("types: %s: parse JSON: %w", id, err)
	}
	c := jsonschema.NewCompiler()
	// jsonschema/v6 requires a resource URL; the actual URL doesn't matter as
	// long as it's unique and stable per call.
	resourceURL := "sibylhub://types/" + id + ".json"
	if err := c.AddResource(resourceURL, parsed); err != nil {
		return nil, fmt.Errorf("types: %s: add resource: %w", id, err)
	}
	sch, err := c.Compile(resourceURL)
	if err != nil {
		return nil, fmt.Errorf("types: %s: compile: %w", id, err)
	}
	return &Definition{
		Name:        name,
		Version:     version,
		Description: description,
		IsCore:      isCore,
		RawSchema:   raw,
		Compiled:    sch,
	}, nil
}

// Get returns the Definition for a typeID like "thesis.v1". Returns
// (nil, ErrUnknownType) if not registered.
func (r *Registry) Get(typeID string) (*Definition, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.all[typeID]
	if !ok {
		return nil, &ErrUnknownType{TypeID: typeID}
	}
	return d, nil
}

// All returns a snapshot slice of every registered type, sorted by ID.
// Useful for the GET /api/types handler in Phase 8.
func (r *Registry) All() []*Definition {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Definition, 0, len(r.all))
	for _, d := range r.all {
		out = append(out, d)
	}
	// Stable order so /api/types responses are deterministic.
	sortDefinitionsByID(out)
	return out
}

// MustExist verifies that every typeID in the slice is registered. Returns
// an error listing all unknown typeIDs in one shot — a UI form that submits
// three bad type names should learn about all three at once, not one at a
// time. Used by the agent_templates repo at create/update time.
func (r *Registry) MustExist(typeIDs ...string) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var missing []string
	seen := make(map[string]bool, len(typeIDs))
	for _, id := range typeIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		if _, ok := r.all[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("types: unknown type(s): %s", strings.Join(missing, ", "))
}

// ErrUnknownType is returned by Get for a typeID that isn't in the registry.
// Carries the requested ID so handlers can produce useful 4xx messages.
type ErrUnknownType struct {
	TypeID string
}

func (e *ErrUnknownType) Error() string { return fmt.Sprintf("types: unknown type %q", e.TypeID) }

// sortDefinitionsByID is split out so it can be tested in isolation if the
// stable order ever becomes load-bearing.
func sortDefinitionsByID(defs []*Definition) {
	// Insertion sort — N is small (6 core types in v0, dozens at most). Avoids
	// pulling in sort.Slice's dependency on the reflect package's Value layer
	// in places where this function may be called frequently.
	for i := 1; i < len(defs); i++ {
		j := i
		for j > 0 && defs[j].ID() < defs[j-1].ID() {
			defs[j], defs[j-1] = defs[j-1], defs[j]
			j--
		}
	}
}
