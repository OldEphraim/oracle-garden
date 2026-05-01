# DATABASE_SCHEMA.md — Oracle Garden Database Schema

Companion to CLAUDE.md. Source of truth for all database tables.

UUIDs everywhere. Timestamps in UTC (`TIMESTAMPTZ`). JSONB for typed agent payloads. All migrations under `api/migrations/` using `golang-migrate`.

---

## Auth

JWT-based auth (NextAuth credentials provider with the JWT session strategy, encoded HS256 with a shared `NEXTAUTH_SECRET` that the Go backend also verifies). **No `sessions` table** — JWTs are stateless. Sign-out clears the cookie client-side; revocation before expiry is not supported in v0.

```sql
CREATE TABLE users (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email         TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,           -- bcrypt cost 10
  display_name  TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

---

## Type Registry

Source for the schema-versioning system. Core types are seeded with `is_core = true` and cannot be deleted.

```sql
CREATE TABLE type_definitions (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name        TEXT NOT NULL,             -- e.g. "thesis"
  version     TEXT NOT NULL,             -- e.g. "v1"
  json_schema JSONB NOT NULL,            -- the JSON Schema for this type
  description TEXT,
  is_core     BOOLEAN NOT NULL DEFAULT FALSE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (name, version)
);
```

Seeded core types: `market_target.v1`, `observation.v1`, `news_digest.v1`, `thesis.v1`, `risk_assessment.v1`, `trading_decision.v1`. Schemas in TYPES.md.

---

## Agent Templates

`owner_id` nullable for system templates. Lineage via `forked_from`. No `version` column — versioning happens via fork.

```sql
CREATE TABLE agent_templates (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id        UUID REFERENCES users(id) ON DELETE CASCADE,    -- NULL for system templates
  name            TEXT NOT NULL,
  description     TEXT,
  system_prompt   TEXT NOT NULL,
  model           TEXT NOT NULL DEFAULT 'claude-sonnet-4-6',
  temperature     REAL NOT NULL DEFAULT 0.7,
  max_tokens      INT  NOT NULL DEFAULT 2000,
  -- Type contract:
  input_types     TEXT[] NOT NULL,   -- accepted input types, e.g. ['observation.v1','news_digest.v1']
  output_type     TEXT NOT NULL,     -- emitted output type, e.g. 'thesis.v1'
  tools           TEXT[] NOT NULL DEFAULT '{}',   -- e.g. ['polymarket.gamma_get_market']
  -- Lineage / social:
  visibility      TEXT NOT NULL DEFAULT 'private',  -- 'private' | 'unlisted' | 'public'
  forked_from     UUID REFERENCES agent_templates(id),
  is_system       BOOLEAN NOT NULL DEFAULT FALSE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_agent_templates_owner ON agent_templates(owner_id);
CREATE INDEX idx_agent_templates_visibility ON agent_templates(visibility);
```

---

## Workflows

`owner_id` nullable to allow seeded system workflows (mirrors `agent_templates`). No `version` column.

```sql
CREATE TABLE workflows (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id        UUID REFERENCES users(id) ON DELETE CASCADE,    -- NULL for system workflows
  name            TEXT NOT NULL,
  description     TEXT,
  schedule_cron   TEXT,                  -- NULL = manual-run only
  is_active       BOOLEAN NOT NULL DEFAULT TRUE,
  market_targets  TEXT[] NOT NULL DEFAULT '{}',  -- Polymarket slugs
  visibility      TEXT NOT NULL DEFAULT 'private',
  forked_from     UUID REFERENCES workflows(id),
  is_system       BOOLEAN NOT NULL DEFAULT FALSE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE workflow_nodes (
  id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_id           UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  agent_template_id     UUID NOT NULL REFERENCES agent_templates(id),
  node_key              TEXT NOT NULL,             -- unique within workflow
  position_x            REAL NOT NULL DEFAULT 0,
  position_y            REAL NOT NULL DEFAULT 0,
  config_overrides      JSONB NOT NULL DEFAULT '{}',  -- e.g. {"temperature": 0.5}
  join_strategy         TEXT NOT NULL DEFAULT 'all',  -- 'all' | 'first'
  loop_iteration_limit  INT  NOT NULL DEFAULT 5,
  UNIQUE (workflow_id, node_key)
);

CREATE TABLE workflow_edges (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_id   UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  from_node_id  UUID NOT NULL REFERENCES workflow_nodes(id) ON DELETE CASCADE,
  to_node_id    UUID NOT NULL REFERENCES workflow_nodes(id) ON DELETE CASCADE,
  condition     TEXT NOT NULL DEFAULT 'always',  -- 'always' | 'approved' | 'rejected' | <substring>
  priority      INT  NOT NULL DEFAULT 0
);
CREATE INDEX idx_workflow_edges_from ON workflow_edges(from_node_id);
```

**Edge ordering:** all engine queries that fetch outgoing edges from a node MUST use `ORDER BY priority ASC, id ASC` to guarantee deterministic evaluation. This is non-negotiable; the engine tests rely on it.

---

## Workflow Runs and Agent Steps

```sql
CREATE TABLE workflow_runs (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_id     UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  triggered_by    TEXT NOT NULL,         -- 'schedule' | 'manual'
  status          TEXT NOT NULL,         -- 'pending' | 'running' | 'completed'
                                         -- | 'failed' | 'timed_out' | 'killed' | 'quota_exceeded'
  market_slug     TEXT,                  -- the market this run targeted
  input_snapshot  JSONB NOT NULL,        -- captures market state at fire time
  error_message   TEXT,
  started_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  finished_at     TIMESTAMPTZ
);
CREATE INDEX idx_workflow_runs_workflow ON workflow_runs(workflow_id);
CREATE INDEX idx_workflow_runs_user_started ON workflow_runs(user_id, started_at DESC);

CREATE TABLE agent_steps (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_run_id    UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
  workflow_node_id   UUID NOT NULL REFERENCES workflow_nodes(id),
  iteration          INT  NOT NULL DEFAULT 1,
  status             TEXT NOT NULL,    -- 'pending' | 'running' | 'completed'
                                       -- | 'failed' | 'timed_out'
  input_data         JSONB,            -- merged inputs from upstream nodes
  output_data        JSONB,            -- validated against agent's output_type
  prompt_tokens      INT,
  completion_tokens  INT,
  cost_usd           NUMERIC(12,6),
  latency_ms         INT,
  error_message      TEXT,
  started_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  finished_at        TIMESTAMPTZ
);
CREATE INDEX idx_agent_steps_run ON agent_steps(workflow_run_id);
```

---

## Paper Trades

Inserted by the engine after the Paper Executor's step completes successfully (the engine reads the validated `trading_decision.v1` payload and writes the row). Side effects don't live in the LLM call.

```sql
CREATE TABLE paper_trades (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_run_id     UUID NOT NULL REFERENCES workflow_runs(id),
  user_id             UUID NOT NULL REFERENCES users(id),
  market_slug         TEXT NOT NULL,
  condition_id        TEXT NOT NULL,
  token_id            TEXT NOT NULL,
  market_question     TEXT NOT NULL,
  side                TEXT NOT NULL,    -- 'YES' | 'NO' | 'ABSTAIN'
  size_usd            NUMERIC(12,2) NOT NULL,
  entry_price         NUMERIC(8,6) NOT NULL,
  reasoning           TEXT,
  status              TEXT NOT NULL,    -- 'open' | 'closed' | 'resolved' | 'abstained'
  current_price       NUMERIC(8,6),
  pnl_usd             NUMERIC(12,2),
  entered_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  exited_at           TIMESTAMPTZ,
  resolved_at         TIMESTAMPTZ
);
CREATE INDEX idx_paper_trades_user ON paper_trades(user_id);
```

`status = 'abstained'` with `size_usd = 0` is the no-trade case. The row is still inserted for run lineage.

**`entry_price` semantics across statuses.** The column name suggests "the price you got in at," which is accurate for `'open'` rows. For `'abstained'` rows, `entry_price` holds the **midpoint snapshot at decision time** — informational only, no trade occurred. Analytics queries should filter accordingly:
- `SELECT AVG(entry_price) FROM paper_trades WHERE side IN ('YES','NO')` — meaningful average of executable prices.
- `SELECT AVG(entry_price) FROM paper_trades` — silently includes abstain midpoints; usually not what you want.

Renaming the column to `reference_price` or similar was considered and rejected for v0 — `entry_price` is the natural term once real-money trading lands in v1, and the analytics consideration is small enough to handle with a query filter. Document is the fix.

**v0 status writes:** the engine only ever writes `'open'` (a paper trade was placed) or `'abstained'` (the run completed without trading). The `'closed'` and `'resolved'` values are forward-compat for v1+ features (PnL tracking via periodic price re-fetch, market-resolution monitoring once Polymarket announces the winning outcome). v0 does not transition rows out of `'open'` — every paper trade just stays open forever from the platform's perspective. Document this in DECISION_LOG so a reader doesn't go hunting for the close/resolve logic.

---

## Cost protection / metering

```sql
CREATE TABLE user_usage_daily (
  user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  day            DATE NOT NULL,
  run_count      INT  NOT NULL DEFAULT 0,
  total_tokens   BIGINT NOT NULL DEFAULT 0,
  total_cost_usd NUMERIC(12,4) NOT NULL DEFAULT 0,
  PRIMARY KEY (user_id, day)
);

CREATE TABLE system_config (
  key        TEXT PRIMARY KEY,
  value      JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Seeded keys: 'kill_switch' (bool, default false)
```

---

## Market subscriptions (forward-compat)

Denormalized from `workflows.market_targets`. Updated transactionally with workflow saves: in the same transaction that updates `workflows`, the engine deletes all existing rows for the workflow and inserts the new set. This guarantees the two sources of truth never drift.

v0 doesn't read this for execution decisions — it's seeded so the v1 watcher service can pick up on day one without a migration.

```sql
CREATE TABLE strategy_market_subscriptions (
  workflow_id   UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  market_slug   TEXT NOT NULL,
  PRIMARY KEY (workflow_id, market_slug)
);
```

---

## Migration order

```
000001_init.up.sql                    # users, type_definitions, agent_templates,
                                       # workflows, workflow_nodes, workflow_edges,
                                       # workflow_runs, agent_steps, paper_trades,
                                       # user_usage_daily, system_config,
                                       # strategy_market_subscriptions
000002_seed_core_types.up.sql         # 6 INSERTs into type_definitions
000003_seed_kill_switch.up.sql        # INSERT INTO system_config ('kill_switch', false)
```

(A future migration may seed a synthetic 'system' user if a v1+ feature needs FKs to a user for system-owned content. Currently nothing does — `agent_templates` and `workflows` both have nullable `owner_id`, so seeded system rows just leave it NULL.)

System agents and the happy-path workflow are seeded via `make seed`, not migrations — they live as JSON under `api/seed/` so they're easy to iterate on without writing a new migration each time.