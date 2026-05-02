-- 000001_init.up.sql
-- Sibyl Hub initial schema. Tables verbatim from DATABASE_SCHEMA.md.
-- No `sessions` table: auth is JWT-only (NextAuth credentials provider with
-- JWT session strategy, HS256, shared NEXTAUTH_SECRET; see CLAUDE.md "Auth Model").

-- pgcrypto provides gen_random_uuid(). Postgres 16 ships gen_random_uuid()
-- in core, but enabling the extension is harmless and works on older managed
-- hosts that haven't migrated the function out of pgcrypto yet.
CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ---------------------------------------------------------------------------
-- Auth
-- ---------------------------------------------------------------------------

CREATE TABLE users (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email         TEXT UNIQUE NOT NULL,
  password_hash TEXT NOT NULL,           -- bcrypt cost 10
  display_name  TEXT,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ---------------------------------------------------------------------------
-- Type registry
-- ---------------------------------------------------------------------------

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

-- ---------------------------------------------------------------------------
-- Agent templates
-- ---------------------------------------------------------------------------

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
  input_types     TEXT[] NOT NULL,                                -- accepted input types, e.g. ['observation.v1','news_digest.v1']
  output_type     TEXT NOT NULL,                                  -- emitted output type, e.g. 'thesis.v1'
  tools           TEXT[] NOT NULL DEFAULT '{}',                   -- e.g. ['polymarket.gamma_get_market']
  -- Lineage / social:
  visibility      TEXT NOT NULL DEFAULT 'private',                -- 'private' | 'unlisted' | 'public'
  forked_from     UUID REFERENCES agent_templates(id),
  is_system       BOOLEAN NOT NULL DEFAULT FALSE,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_agent_templates_owner      ON agent_templates(owner_id);
CREATE INDEX idx_agent_templates_visibility ON agent_templates(visibility);

-- ---------------------------------------------------------------------------
-- Workflows
-- ---------------------------------------------------------------------------

CREATE TABLE workflows (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  owner_id        UUID REFERENCES users(id) ON DELETE CASCADE,    -- NULL for system workflows
  name            TEXT NOT NULL,
  description     TEXT,
  schedule_cron   TEXT,                                           -- NULL = manual-run only
  is_active       BOOLEAN NOT NULL DEFAULT TRUE,
  market_targets  TEXT[] NOT NULL DEFAULT '{}',                   -- Polymarket slugs
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
  node_key              TEXT NOT NULL,                            -- unique within workflow
  position_x            REAL NOT NULL DEFAULT 0,
  position_y            REAL NOT NULL DEFAULT 0,
  config_overrides      JSONB NOT NULL DEFAULT '{}',              -- e.g. {"temperature": 0.5}
  join_strategy         TEXT NOT NULL DEFAULT 'all',              -- 'all' | 'first'
  loop_iteration_limit  INT  NOT NULL DEFAULT 5,
  UNIQUE (workflow_id, node_key)
);

CREATE TABLE workflow_edges (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_id   UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  from_node_id  UUID NOT NULL REFERENCES workflow_nodes(id) ON DELETE CASCADE,
  to_node_id    UUID NOT NULL REFERENCES workflow_nodes(id) ON DELETE CASCADE,
  condition     TEXT NOT NULL DEFAULT 'always',                   -- 'always' | 'approved' | 'rejected' | <substring>
  priority      INT  NOT NULL DEFAULT 0
);

CREATE INDEX idx_workflow_edges_from ON workflow_edges(from_node_id);

-- Engine queries that fetch outgoing edges from a node MUST use
-- ORDER BY priority ASC, id ASC for deterministic evaluation. The ordering
-- is enforced at query time, not by an index — but the from-node index
-- above keeps the lookup itself cheap.

-- ---------------------------------------------------------------------------
-- Workflow runs and agent steps
-- ---------------------------------------------------------------------------

CREATE TABLE workflow_runs (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_id     UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  triggered_by    TEXT NOT NULL,                                  -- 'schedule' | 'manual'
  status          TEXT NOT NULL,                                  -- 'pending' | 'running' | 'completed'
                                                                  -- | 'failed' | 'timed_out' | 'killed' | 'quota_exceeded'
  market_slug     TEXT,                                           -- the market this run targeted
  input_snapshot  JSONB NOT NULL,                                 -- captures market state at fire time
  error_message   TEXT,
  started_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  finished_at     TIMESTAMPTZ
);

CREATE INDEX idx_workflow_runs_workflow     ON workflow_runs(workflow_id);
CREATE INDEX idx_workflow_runs_user_started ON workflow_runs(user_id, started_at DESC);

CREATE TABLE agent_steps (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_run_id    UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
  workflow_node_id   UUID NOT NULL REFERENCES workflow_nodes(id),
  iteration          INT  NOT NULL DEFAULT 1,
  status             TEXT NOT NULL,                               -- 'pending' | 'running' | 'completed'
                                                                  -- | 'failed' | 'timed_out'
  input_data         JSONB,                                       -- merged inputs from upstream nodes
  output_data        JSONB,                                       -- validated against agent's output_type
  prompt_tokens      INT,
  completion_tokens  INT,
  cost_usd           NUMERIC(12,6),
  latency_ms         INT,
  error_message      TEXT,
  started_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  finished_at        TIMESTAMPTZ
);

CREATE INDEX idx_agent_steps_run ON agent_steps(workflow_run_id);

-- ---------------------------------------------------------------------------
-- Paper trades
-- ---------------------------------------------------------------------------
-- FK to workflow_runs / users intentionally has no ON DELETE CASCADE — paper
-- trades are audit records and should not silently vanish when a parent run
-- is removed. v0 never deletes runs anyway, so RESTRICT is harmless.

CREATE TABLE paper_trades (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  workflow_run_id     UUID NOT NULL REFERENCES workflow_runs(id),
  user_id             UUID NOT NULL REFERENCES users(id),
  market_slug         TEXT NOT NULL,
  condition_id        TEXT NOT NULL,
  token_id            TEXT NOT NULL,
  market_question     TEXT NOT NULL,
  side                TEXT NOT NULL,                              -- 'YES' | 'NO' | 'ABSTAIN'
  size_usd            NUMERIC(12,2) NOT NULL,
  entry_price         NUMERIC(8,6) NOT NULL,
  reasoning           TEXT,
  status              TEXT NOT NULL,                              -- 'open' | 'closed' | 'resolved' | 'abstained'
  current_price       NUMERIC(8,6),
  pnl_usd             NUMERIC(12,2),
  entered_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  exited_at           TIMESTAMPTZ,
  resolved_at         TIMESTAMPTZ
);

CREATE INDEX idx_paper_trades_user ON paper_trades(user_id);

-- ---------------------------------------------------------------------------
-- Cost protection / metering
-- ---------------------------------------------------------------------------

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

-- ---------------------------------------------------------------------------
-- Market subscriptions (forward-compat for v1 watcher service)
-- ---------------------------------------------------------------------------

CREATE TABLE strategy_market_subscriptions (
  workflow_id   UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
  market_slug   TEXT NOT NULL,
  PRIMARY KEY (workflow_id, market_slug)
);
