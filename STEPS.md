# STEPS.md — Oracle Garden Build Plan

This file is the phased build plan. Each phase has a clear goal, the files it touches, the verification step, and a manual-checkpoint pause for Alan to validate before the next phase begins.

**Read CLAUDE.md, TYPES.md, DATABASE_SCHEMA.md, and AGENT_TEMPLATES.md before starting any phase.** They are the source of truth for architecture; this file is the source of truth for sequencing.

After each phase, append an entry to `DECISION_LOG.md` summarizing decisions made during that phase that diverge from CLAUDE.md, or noting "no deviations" if there are none.

---

## Phase 0 — Pre-flight

**Goal:** Verify the build environment. No code is written yet.

1. Verify required tools:
   - `go version` (>= 1.22)
   - `node --version` (>= 20)
   - `npm --version`
   - `docker --version`, `docker compose version`
   - `psql --version`
2. Install if missing:
   - `go install -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@latest`
3. Verify `ANTHROPIC_API_KEY` and `NEXTAUTH_SECRET` env vars are set (or note they must be added to `.env` before Phase 4 and Phase 9 respectively). Generate `NEXTAUTH_SECRET` with `openssl rand -base64 32`.
4. Create the project skeleton:
   ```
   oracle-garden/
   ├── api/
   ├── web/
   ├── docker-compose.yml          (placeholder — populated in Phase 1)
   ├── .env.example
   ├── Makefile                     (placeholder)
   ├── CLAUDE.md                    (already present)
   ├── TYPES.md                     (already present)
   ├── DATABASE_SCHEMA.md           (already present)
   ├── AGENT_TEMPLATES.md           (already present)
   ├── STEPS.md                     (already present)
   ├── DECISION_LOG.md              (created with seed entries from CLAUDE.md's
   │                                Decision Log section)
   └── README.md                    (stub — populated in Phase 16)
   ```
5. Initialize git, set the remote to `git@github.com:OldEphraim/oracle-garden.git` once the repo exists. Do not push until Phase 1 completes.

6. **Confirm exact Anthropic model strings** by checking current Anthropic API documentation. Resolve whether agent template defaults should use the alias forms (`claude-sonnet-4-6`, `claude-haiku-4-5`, `claude-opus-4-7`) or dated suffix forms (e.g., `claude-haiku-4-5-20251001`). Record the chosen strings in `DECISION_LOG.md` so they're consistent across the agent seed JSON, the migration default, and `pricing.go` written in Phase 4.

**Checkpoint:** Alan confirms directory structure, env vars, tool versions, and the chosen model identifiers. Reviews seed `DECISION_LOG.md` entries.

---

## Phase 1 — Database, migrations, type registry seed

**Goal:** Postgres comes up via Docker. All tables from DATABASE_SCHEMA.md exist. Type registry is seeded with the 6 core types.

**Files created/modified:**
- `docker-compose.yml` — postgres service only for now (api/web added in Phase 17).
- `.env.example` and `.env` — `DATABASE_URL`, `ANTHROPIC_API_KEY`, `NEXTAUTH_SECRET` (with comment: "treat as UTF-8 byte string on both Node and Go sides; do not base64-decode"), `INTERNAL_API_URL=http://localhost:8080` (for dev; overridden to `http://api:8080` in Docker Compose), `MAX_RUNS_PER_USER_PER_DAY=50`, `MAX_COST_USD_PER_USER_PER_DAY=5.00`, `MIN_SCHEDULE_INTERVAL_SECONDS=300`, `EXECUTION_KILL_SWITCH_DEFAULT=false`, `ADMIN_EMAILS=`.
- `api/migrations/000001_init.up.sql` — every table from DATABASE_SCHEMA.md verbatim. **No `sessions` table** (JWT auth, see Phase 9).
- `api/migrations/000001_init.down.sql` — drop all tables in reverse FK order.
- `api/migrations/000002_seed_core_types.up.sql` — INSERT statements for the 6 core types from TYPES.md, copying their JSON schemas verbatim into the `json_schema` column.
- `api/migrations/000002_seed_core_types.down.sql` — DELETE WHERE is_core = true.
- `api/migrations/000003_seed_kill_switch.up.sql` — `INSERT INTO system_config (key, value) VALUES ('kill_switch', 'false'::jsonb)`.
- `Makefile` — `migrate-up`, `migrate-down`, `migrate-create name=...`, `db-shell`.

**Verification:**
1. `docker compose up -d postgres` and the container is healthy.
2. `make migrate-up` succeeds.
3. `psql $DATABASE_URL -c "\dt"` shows all expected tables (no `sessions`).
4. `psql $DATABASE_URL -c "SELECT name, version FROM type_definitions ORDER BY name"` shows all 6 core types.
5. `make migrate-down` succeeds (cleanly reverts).
6. `make migrate-up` again to leave DB seeded.

**Checkpoint:** Alan opens psql, runs `\d agent_templates`, confirms columns. Inspects one of the seeded JSON Schemas. Confirms `users.password_hash` is `TEXT NOT NULL` (no default).

---

## Phase 2 — Polymarket adapter

**Goal:** The Go service can resolve a market by slug, fetch its orderbook midpoint, and pull recent price history. No agents yet — adapter and a manual driver only.

**Files created/modified:**
- `api/internal/polymarket/client.go` — HTTP client wrapping Gamma + CLOB public endpoints. `net/http`, exponential backoff with jitter on 429, in-memory LRU cache via `hashicorp/golang-lru/v2`.
- `api/internal/polymarket/types.go` — Go structs matching Gamma's `Market` shape with stringified-number coercion.
- `api/internal/polymarket/methods.go` — `GetMarket(slug)`, `SearchMarkets(q, limit)`, `GetOrderbook(tokenID)`, `GetMidpoint(tokenID)`, `GetPriceHistory(tokenID, interval, fidelity)`.
- `api/internal/polymarket/cache.go` — TTL-aware wrapper (5-min metadata, 30-sec prices, 60-sec orderbooks).
- `api/internal/polymarket/limiter.go` — token-bucket (50 req/min default).
- `api/cmd/pmctl/main.go` — small CLI: takes a slug, prints the resolved market + midpoint, used for manual verification.
- `api/internal/polymarket/client_test.go` — unit tests with mocked HTTP.

**Verification:**
1. `go test ./internal/polymarket/...` passes.
2. `go run cmd/pmctl/main.go --slug <real-slug>` prints a real market with a real midpoint.
3. Re-running logs a cache hit, not a network call.

**Checkpoint:** Alan runs `pmctl` on 2-3 different live markets, confirms `clobTokenIds` is correctly extracted and the midpoint is a sane 0–1 value.

---

## Phase 3 — Type registry, JSON Schema validator, repositories

**Goal:** Repository layer for users, agent_templates, workflows, runs, etc. Type registry loads from DB at startup; agent template save validates declared `input_types`/`output_type` exist in the registry.

**Files created/modified:**
- `api/internal/db/db.go` — pgx pool initialization.
- `api/internal/db/repos/users.go` — Create, GetByEmail, GetByID, UpdatePassword.
- `api/internal/db/repos/agent_templates.go` — full CRUD with visibility filtering.
- `api/internal/db/repos/workflows.go` — full CRUD; nodes and edges loaded eagerly. **Edge fetches use `ORDER BY priority ASC, id ASC`** — non-negotiable.
- `api/internal/db/repos/runs.go` — Create, AddStep, UpdateStepStatus, GetWithSteps.
- `api/internal/db/repos/usage.go` — IncrementUsage (transactional), GetTodayUsage, GetUsageHistory.
- `api/internal/db/repos/system_config.go` — GetKillSwitch, SetKillSwitch.
- `api/internal/db/repos/subscriptions.go` — `SyncForWorkflow(ctx, tx, workflowID, slugs)` — delete-all-then-insert-all in caller-provided transaction.
- `api/internal/types/registry.go` — load all `type_definitions` at startup; expose `Get(name, version)` returning a compiled JSON Schema validator (using `santhosh-tekuri/jsonschema/v6` or similar pure-Go AJV-equivalent).
- `api/internal/types/validate.go` — `ValidateAgainst(payload []byte, typeID string)` returns a list of structured errors.
- Tests for each repo against a real test database (use a transactional rollback pattern: each test wraps its work in a tx that's rolled back at teardown).

**Verification:**
1. `go test ./internal/db/...` passes.
2. `go test ./internal/types/...` passes.
3. A driver script (`api/cmd/typecheck/main.go`) loads the registry, validates a sample valid `observation.v1` payload (passes), and a sample invalid one (fails with readable errors).

**Checkpoint:** Alan runs the typecheck driver, confirms validation errors are surfaced clearly.

---

## Phase 4 — Agent runtime (Anthropic API client)

**Goal:** Given an `agent_template` and merged inputs, invoke the Anthropic API, validate output, return a structured result with token/cost data. No tools yet (Phase 5).

**Files created/modified:**
- `api/internal/runtime/client.go` — wrapper around the Anthropic Go SDK if suitable; otherwise hand-rolled HTTP client (the API surface is small).
- `api/internal/runtime/agent.go` — `Invoke(ctx, agent, mergedInputs)`. Builds messages, calls API, parses response, validates against `agent.OutputType`, retries **once** on validation failure (2 total attempts).
- `api/internal/billing/pricing.go` — static price table per model. Confirm exact pricing against Anthropic docs at build time. Default model `claude-sonnet-4-6`.
- `api/internal/runtime/agent_test.go` — tests with a mock Anthropic transport.

**Verification:**
1. `go test ./internal/runtime/...` passes.
2. Driver `api/cmd/agentctl/main.go` loads a hardcoded Thesis Builder template, supplies fake observation+news_digest inputs, prints the validated thesis output and the cost.

**Checkpoint:** Alan runs `agentctl`, confirms the API call works against his real key and the cost is plausible.

---

## Phase 5 — Tools registry + tool dispatch loop

**Goal:** Agents can call tools. The runtime handles the API's tool-use round-trip: call → tool_use → tool_result → call again, capped at 6 rounds.

**Files created/modified:**
- `api/internal/tools/registry.go` — `Tool` interface (with `IsServerSide() bool`), registration map.
- `api/internal/tools/polymarket.go` — implementations of the 5 polymarket.* tools, backed by the Phase 2 adapter. All return `IsServerSide() == false`.
- `api/internal/tools/web_search.go` — wrapper that returns `IsServerSide() == true` and supplies the right `ToolDefinition` for Anthropic's built-in tool. `Invoke` panics — server-side tools should never be locally dispatched.
- `api/internal/runtime/agent.go` — extended with the tool-use loop. On `tool_use` blocks: if the tool is server-side, do nothing (Anthropic handled it); otherwise dispatch via the registry, append `tool_result`, continue. Cap at 6 rounds.
- Tests: tool dispatch round-trip, server-side tool passthrough, the 6-round cap.

**Verification:**
1. `agentctl --agent market-watcher --slug <real-slug>` runs a hardcoded Market Watcher (with polymarket tools enabled) end-to-end and produces a valid `observation.v1` with real price data.
2. `agentctl --agent news-scout --slug <real-slug>` runs the News Scout against `web_search` and produces a valid `news_digest.v1`.

**Checkpoint:** Alan runs both, confirms tool integration works. Confirms server-side `web_search` doesn't try to dispatch locally.

---

## Phase 6 — Workflow execution engine

**Goal:** The graph executor. Given a workflow ID and a market_slug, runs the full graph: branching, loops, fan-out, fan-in. Persists `agent_steps`. Enforces all timeouts and step limits. Does not yet emit SSE events.

**Files created/modified:**
- `api/internal/engine/graph.go` — load workflow into an in-memory graph; identify entry and terminal nodes; per-node `join_strategy`, `loop_iteration_limit`.
- `api/internal/engine/executor.go` — main loop: ready queue, step fire, edge evaluation. Implements the rules from CLAUDE.md (always edges always fire; first-match for non-always conditional edges).
- `api/internal/engine/edges.go` — `conditionMatches(output, condition)` per CLAUDE.md's logic. Validates approved/rejected at workflow save: source output_type must equal `risk_assessment.v1`.
- `api/internal/engine/inputs.go` — multi-input merge into the keyed dict; "accepts" type semantics (latest output from each upstream that has any output).
- `api/internal/engine/timeouts.go` — per-step (90s), per-run (10min), per-step-count (50) enforcement.
- `api/internal/engine/executor_test.go` — extensive test matrix:
  - linear flow
  - fan-in (`all` and `first`)
  - fan-out (multiple `always` edges from one node)
  - branching (mutually exclusive conditional edges)
  - mixed fan-out + branching (worked example from CLAUDE.md)
  - loop (Risk → Thesis with rejection)
  - loop with iteration limit hit
  - approved/rejected validation rejecting bad workflow at save time
  - per-step timeout
  - per-run timeout
  - 50-step run cap
  - mid-run kill switch flip

**Verification:**
1. `go test ./internal/engine/...` passes including all matrix entries.
2. Driver `api/cmd/runctl/main.go` seeds the happy-path workflow (just enough to run; full seed comes in Phase 14), executes it against a real market_slug end-to-end without an HTTP server.
3. `psql` shows the persisted run + agent_steps rows with valid token counts.

**Checkpoint:** Alan runs `runctl --slug <real-slug>`, watches the logs, confirms a paper trade was eventually recorded. He inspects `agent_steps` to see the full trace. He intentionally runs against a market the Risk Assessor will reject to confirm the loop fires.

---

## Phase 7 — Cost protection + scheduler

**Goal:** Quota enforcement is live; the scheduler can fire scheduled runs.

**Files created/modified:**
- `api/internal/billing/quota.go` — `CheckQuota(userID)` reads `user_usage_daily` and env caps; returns ok-or-reason. Called before every run start.
- `api/internal/billing/accounting.go` — `RecordStep(userID, cost, tokens)` transactional increment.
- `api/internal/scheduler/scheduler.go` — wraps `robfig/cron`. Loads all `workflows` with non-null `schedule_cron` and `is_active = true`. **For each cron tick, iterates the workflow's `market_targets` and creates one run per target.** Skips firings if kill-switch is set.
- `api/internal/scheduler/validation.go` — `ValidateCronInterval(expr)` — parses with robfig/cron, calls `Next(now)` twice, computes the diff, rejects if below `MIN_SCHEDULE_INTERVAL_SECONDS`. **Pattern-matching on the cron string is wrong — `*/2 * * * *` evaluates to 2 minutes; the regex approach misses it.**
- `api/internal/scheduler/scheduler_test.go` — verifies kill-switch behavior, quota enforcement, multi-target firing, min-interval rejection (including the `*/2 * * * *` case).

**Verification:**
1. Tests pass.
2. Driver: `runctl --repeat 5` with `MAX_RUNS_PER_USER_PER_DAY=2` produces 2 successful runs and 3 `quota_exceeded` runs.
3. Saving a workflow with `schedule_cron = '*/2 * * * *'` is rejected with a clear error message.

**Checkpoint:** Alan validates quota and interval-validation behavior, then resets caps to defaults.

---

## Phase 8 — HTTP server, Chi router, SSE with replay buffer

**Goal:** All backend endpoints from CLAUDE.md's Backend API section are live. SSE events stream during runs with a replay buffer so late subscribers don't miss events.

**Files created/modified:**
- `api/cmd/server/main.go` — wire up DB, registry, scheduler, HTTP server, pprof on `127.0.0.1:6060`.
- `api/internal/api/router.go` — Chi routes, middleware (JWT auth, request ID, structured logging).
- `api/internal/api/handlers/users.go` — `POST /api/users` (signup: bcrypt-hash, insert user row, return 200; **does not issue a JWT**), `POST /api/users/verify-credentials` (internal endpoint called server-to-server by NextAuth's `authorize` callback: takes email + password, verifies the hash, returns 200 with `{user_id, email, display_name}` on success, 401 otherwise). **No Go-side `/api/auth/*` endpoints, no Go-side signout handler.** NextAuth on the Next.js side owns the entire `/api/auth/*` namespace and handles signout by clearing the cookie client-side, which is sufficient for stateless JWTs.
- `api/internal/api/handlers/agents.go` — agent_template CRUD + fork.
- `api/internal/api/handlers/workflows.go` — workflow CRUD + fork + `/run`. Validates type-compat AND approved/rejected source-type rule on save. Updates `strategy_market_subscriptions` in the same transaction as the workflow update.
- `api/internal/api/handlers/runs.go` — run detail, runs list. **Both `GET /api/runs/:id` and `GET /api/runs/:id/events` verify ownership before returning data or subscribing.** The JWT middleware authenticates; per-resource access checks happen in the handlers. Per-resource rules:
  - **Strictly owned** (`runs`, `paper_trades`, `agent_steps`, `user_usage_daily`): allow only if `user_id = ctx.user_id`. Unauthorized → 404 (avoid leaking existence).
  - **Shareable** (`agent_templates`, `workflows`): allow if `owner_id = ctx.user_id` OR `is_system = true` OR `visibility IN ('public', 'unlisted')`. Unauthorized → 404. In v0 only the `is_system` branch actually fires (visibility UI doesn't exist), but coding the rule correctly from day one matches the data model and avoids retrofitting in v2.
  
  Mutating endpoints on shareable resources (`PATCH`, `DELETE`) require `owner_id = ctx.user_id` regardless of visibility — system templates and other people's public agents are read-only.

  **Cross-resource queries:** `GET /api/workflows/:id/runs` is special — the workflow is shareable but runs are strictly owned. Result set is filtered to `workflow_id = :id AND user_id = ctx.user_id`. So a user fetching the runs of a system workflow sees only their own runs of it, never anyone else's. Same pattern for any future "list child resources of a shareable parent."
- `api/internal/api/handlers/markets.go` — Polymarket proxy.
- `api/internal/api/handlers/me.go` — current user, usage.
- `api/internal/api/handlers/admin.go` — env-gated admin routes.
- `api/internal/api/handlers/types.go` — list types, get schema.
- `api/internal/sse/broadcaster.go` — per-run SSE channel with **replay buffer**: a ring buffer of all events since `RunStarted`, flushed to each new subscriber before live events stream. Engine fires its first agent step immediately — no startup delay.
- `api/internal/auth/middleware.go` — JWT cookie validation using `golang-jwt/jwt/v5` with HS256 + `NEXTAUTH_SECRET`. Attaches `user_id` to request context.

**Verification:**
1. `curl` walks the full happy path: `POST /api/users` (create) → `POST /api/users/verify-credentials` (manual call simulating what NextAuth does internally; must return 200 with user info) → manually craft a JWT cookie matching NextAuth's expected shape (or defer this verification to Phase 9 when NextAuth itself is available) → `GET /api/me` → list-agents → create-workflow → run-workflow → SSE stream the run.
2. Tests for handlers using `httptest`.
3. SSE replay test: subscribe to a run that's already in flight, verify all prior events are received before live events.

**Checkpoint:** Alan walks the curl flow himself; confirms SSE events arrive during a run with reasonable timing.

---

## Phase 9 — Frontend skeleton with JWT auth

**Goal:** Next.js 14 scaffolded. NextAuth configured with credentials provider + **JWT strategy + custom HS256 encode/decode**. Sessions are stateless. The shell of every authenticated route exists with placeholder content. Tailwind themed.

**Files created/modified:**
- `web/package.json` — Next 14, React 18, TypeScript, Tailwind, NextAuth, React Flow, Recharts, AJV, `jsonwebtoken`. (No `bcryptjs` — password hashing happens server-side in Go; the frontend never touches plaintext or hashed passwords beyond what the user types into the signin form.)
- `web/tailwind.config.ts`, `web/app/globals.css` — calm palette with garden-tinted neutrals.
- `web/lib/auth.ts` — NextAuth config:
  - Credentials provider with `authorize` callback that POSTs to **`${process.env.INTERNAL_API_URL}/api/users/verify-credentials`** (server-to-server, absolute URL — bypasses the rewrite). Returns the user object on 200, null on 401.
  - **`session: { strategy: 'jwt' }`** explicitly.
  - **`jwt.encode` and `jwt.decode` overridden** to use `jsonwebtoken.sign(payload, NEXTAUTH_SECRET, { algorithm: 'HS256' })` and `jsonwebtoken.verify`. This produces JWS, not JWE — Go can verify with a standard library.
  - **Secret encoding:** the JS side passes `NEXTAUTH_SECRET` directly as a string to `jsonwebtoken` (no base64 decoding). The Go side does `[]byte(os.Getenv("NEXTAUTH_SECRET"))` (no base64 decoding). Both sides treat the secret as a UTF-8 byte string used directly as the HMAC key. This convention is non-negotiable; mismatches produce silent "invalid signature" errors. Comment in `.env.example` makes this explicit.
  - JWT `maxAge: 7 * 24 * 60 * 60`.
- `web/app/api/auth/[...nextauth]/route.ts` — handler.
- `web/app/layout.tsx`, `web/app/(auth)/layout.tsx`, `web/app/(app)/layout.tsx` — chrome.
- `web/app/(auth)/signin/page.tsx`, `web/app/(auth)/signup/page.tsx`.
- `web/app/(app)/dashboard/page.tsx`, `web/app/(app)/agents/page.tsx`, `web/app/(app)/workflows/page.tsx`, `web/app/(app)/markets/page.tsx`, `web/app/(app)/account/page.tsx` — placeholders.
- `web/components/ui/*` — Button, Input, Card, Dialog, Select, Tabs.
- `web/lib/api.ts` — typed fetch client; calls relative paths only (`fetch('/api/me')`, never absolute URLs); sets `credentials: 'include'` so the JWT cookie travels with same-origin requests.
- `web/next.config.js` — rewrites every `/api/*` path to the Go backend. Next.js's App Router `[...nextauth]` route at `web/app/api/auth/[...nextauth]/route.ts` claims `/api/auth/*` and takes precedence over rewrites for unmatched routes, so a single catch-all rewrite is sufficient:
  ```js
  async rewrites() {
    return [
      // /api/auth/* is served by the App Router (NextAuth catch-all);
      // Next.js's static route matching wins over rewrites, so this rule
      // only fires for non-/api/auth paths.
      { source: '/api/:path*', destination: `${process.env.INTERNAL_API_URL}/api/:path*` },
    ];
  }
  ```
- `INTERNAL_API_URL` added to `.env.example`: `http://localhost:8080` for dev, `http://api:8080` for Docker Compose.
- `web/lib/ajv.ts` — AJV instance singleton with custom-format support.

**Verification:**
1. `cd web && npm run dev` starts cleanly.
2. Signup flow: form `POST`s to `/api/users` (rewritten to Go, sets no cookie), then frontend calls `signIn()` which goes through NextAuth → NextAuth's `authorize` calls Go's `/api/users/verify-credentials` server-to-server → NextAuth issues the JWT cookie. User lands on dashboard.
3. The JWT cookie is set (inspectable in devtools); decoding it manually with the secret yields a JWS with `sub`, `email`, `iat`, `exp`.
4. The Go backend accepts the same cookie via the rewrite — hitting `/api/me` from the browser succeeds.
5. Auth-gated routes redirect to `/signin` when not authenticated.
6. Signout via NextAuth (frontend) clears the cookie; subsequent `/api/me` returns 401.

**Checkpoint:** Alan signs up a test account, confirms cookie shape and Go-side acceptance. Confirms the chrome looks acceptable.

---

## Phase 10 — Markets browser

**Goal:** The Markets page is fully functional. Users can search Polymarket and view market details.

**Files created/modified:**
- `web/app/(app)/markets/page.tsx` — search bar, results grid.
- `web/app/(app)/markets/[slug]/page.tsx` — market detail with current prices, liquidity, end date, "Run a strategy on this market" CTA (deep-links to the workflow run modal with the slug pre-filled).
- `web/components/markets/MarketCard.tsx`, `web/components/markets/MarketSearch.tsx`.

**Verification:** Search across multiple queries, confirm freshness and cache behavior. Click into a market, confirm detail loads.

**Checkpoint:** Alan tests several searches.

---

## Phase 11 — Agent builder

**Goal:** Users can create their own agent templates. Validation surfaces type-system errors before submission.

**Files created/modified:**
- `web/app/(app)/agents/page.tsx` — list (System / Mine / Forked).
- `web/app/(app)/agents/[id]/page.tsx` — view and edit own agents.
- `web/app/(app)/agents/new/page.tsx` — agent builder form.
- `web/components/agents/AgentForm.tsx` — name, description, system prompt textarea (monospace), model dropdown (sonnet-4-6, haiku-4-5, opus-4-7), temperature slider, max_tokens, output_type dropdown (with schema preview side panel), input_types multi-select, tools multi-select.
- `web/components/agents/SchemaPreview.tsx` — pretty-prints a JSON Schema with field descriptions; uses AJV for the live "validate sample input" panel.
- `web/lib/schemas.ts` — fetches and caches type registry from `/api/types`. AJV-compiled validators are cached in-memory.

**Verification:** Create a custom agent ("Test Agent") with output_type `thesis.v1`, save, verify it appears in "Mine" tab and is selectable in the workflow builder.

**Checkpoint:** Alan creates the demo Devil's Advocate agent here and iterates on its system prompt.

---

## Phase 12 — Workflow builder (React Flow)

**Goal:** The visual canvas. Users can build a workflow from scratch, fork the happy-path template, configure edges and nodes, validate, and trigger runs.

**Files created/modified:**
- `web/app/(app)/workflows/page.tsx` — list.
- `web/app/(app)/workflows/[id]/page.tsx` — builder + recent runs sidebar.
- `web/app/(app)/workflows/new/page.tsx` — empty canvas.
- `web/components/flow/WorkflowCanvas.tsx` — React Flow root.
- `web/components/flow/AgentNode.tsx` — custom node renderer.
- `web/components/flow/EdgeWithCondition.tsx` — custom edge renderer.
- `web/components/flow/NodeConfigPanel.tsx` — right panel: node_key, join_strategy, loop_iteration_limit, config_overrides.
- `web/components/flow/EdgeConfigPanel.tsx` — right panel: condition (with presets `always`, `approved`, `rejected`, custom), priority. Surfaces a warning if the user picks `approved`/`rejected` for an edge whose source isn't `risk_assessment.v1`.
- `web/components/flow/AgentDrawer.tsx` — left drawer: categorized agent list, drag-to-canvas.
- `web/components/flow/RunDialog.tsx` — modal for picking market_slug.
- `web/components/flow/ValidateButton.tsx` — client-side type-compat check + approved/rejected source check before save.

**Verification:** Build a workflow from scratch using the 5 system agents → save → run → land on run detail. Save an intentionally invalid workflow (Watcher → Risk, skipping Thesis) → see a clear validation error citing the type mismatch. Save a workflow with an `approved` edge from Watcher → see a clear validation error.

**Checkpoint:** Alan rebuilds the happy-path workflow once. Validates fork + insert (the Devil's Advocate demo flow) works smoothly.

---

## Phase 13 — Run detail + monitoring UI

**Goal:** The monitoring page is alive and impressive. The graph mirrors the canvas with live status; the timeline shows step-by-step progress with expandable JSON I/O.

**Files created/modified:**
- `web/app/(app)/workflows/[id]/runs/[runId]/page.tsx` — full run detail.
- `web/components/runs/RunGraph.tsx` — read-only React Flow with status-tinted nodes.
- `web/components/runs/Timeline.tsx` — vertical event stream.
- `web/components/runs/StepCard.tsx` — expandable card showing input_data, output_data (syntax-highlighted JSON), tokens, cost, latency, errors.
- `web/components/runs/PaperTradeBanner.tsx` — prominent banner if `PaperTradeRecorded` event fires.
- `web/lib/sse.ts` — `useRunEvents(runId)` hook. The replay buffer is server-side (Phase 8), so this hook just attaches and processes events as they arrive.

**Verification:** Trigger a run from the workflow page → run detail loads → events stream in → paper trade banner appears at the end. Open a second tab on the same run → both tabs show all events, including those before the second tab subscribed (replay buffer).

**Checkpoint:** Alan runs the demo flow end-to-end, iterating on visual polish. Confirms the experience is good enough for a job-interview demo.

---

## Phase 14 — Seed system templates

**Goal:** The 5 system agent templates and the happy-path workflow are seeded into a fresh database via `make seed`.

**Files created/modified:**
- `api/cmd/seed/main.go` — reads JSON files from `api/seed/`, upserts into DB. Uses `owner_id = NULL` and `is_system = true` for seeded rows.
- `api/seed/agents/01_market_watcher.json` — full template per AGENT_TEMPLATES.md, incl. system prompt and tools `[polymarket.gamma_get_market, polymarket.clob_get_midpoint, polymarket.clob_get_prices_history]`.
- `api/seed/agents/02_news_scout.json` — per AGENT_TEMPLATES.md, tools `[web_search]`.
- `api/seed/agents/03_thesis_builder.json` — per AGENT_TEMPLATES.md; input_types `['observation.v1','news_digest.v1','risk_assessment.v1']`, no tools, system prompt explicitly handles the optional `risk` input.
- `api/seed/agents/04_risk_assessor.json` — per AGENT_TEMPLATES.md, no tools.
- `api/seed/agents/05_paper_executor.json` — per AGENT_TEMPLATES.md, tools `[polymarket.gamma_get_market, polymarket.clob_get_orderbook]`. System prompt explicitly instructs resolving token_ids via gamma_get_market.
- `api/seed/workflows/happy_path.json` — workflow + 5 nodes + 5 edges (Watcher→Thesis, Scout→Thesis, Thesis→Risk, Risk→Executor `approved`, Risk→Thesis `rejected`).
- `Makefile` — `seed` target.

**Verification:**
1. `make migrate-down && make migrate-up && make seed` produces a fully-populated database.
2. The frontend Agents page shows all 5 system agents.
3. Forking the happy-path workflow and running it against a real market produces a paper trade reliably.

**Checkpoint:** Alan runs the seed flow several times to verify idempotency. Iterates on system prompts based on real-run quality.

---

## Phase 14.5 — Reliability bar

**Goal:** Establish a measured reliability baseline before declaring v0 demo-ready.

**Approach:** Run the happy-path workflow 10 times across diverse markets — different domains (politics, sports, crypto, culture), different liquidity tiers, different times to resolution. Target: **≥80% completion rate ending in a recorded paper trade** (whether YES, NO, or ABSTAIN — all three are "completion"; only `failed`/`timed_out` runs count against the rate).

**Files created/modified:**
- `api/cmd/reliability/main.go` — orchestration script: takes a list of slugs, runs each via the API, collects outcomes, prints a report.
- `seeds/reliability_markets.txt` — the curated test list.
- `DECISION_LOG.md` — entry recording the result and the chosen "demo market" (the most reliably-completing one for the recorded fallback).

**Iteration:** if reliability is below 80%, iterate on the system prompts in `api/seed/agents/` and re-run. **When a prompt changes, update the corresponding sketch in AGENT_TEMPLATES.md and add a DECISION_LOG entry.** Common failure modes worth addressing in prompts:
- Validation failures from agents producing slightly malformed JSON → tighten the "respond ONLY with JSON matching this schema" instruction.
- Risk Assessor approving too eagerly or too rarely → calibrate thresholds.
- Paper Executor failing to resolve token_ids → make the gamma_get_market step explicit in its prompt.

**Verification:** 10 sequential runs with ≥80% completion. The successful runs include at least one APPROVED-and-traded path and at least one REJECTED-then-loop path.

**Checkpoint:** Alan reviews the reliability report. Records the demo market and a recorded fallback run video for interview presentations.

---

## Phase 15 — Account / usage / admin

**Goal:** Users can see their daily cost. Alan as platform owner can flip the kill switch.

**Files created/modified:**
- `web/app/(app)/account/page.tsx` — display name, change password, usage table (last 30 days).
- `web/components/account/UsageChart.tsx` — Recharts area chart of daily cost.
- `web/components/layout/UsageIndicator.tsx` — top-right "$X.XX today" pill, yellow at 75% of cap, red at 100%.
- `web/app/(app)/account/admin/page.tsx` — env-gated by `ADMIN_EMAILS`; total runs today, total cost today, top users, kill-switch toggle.

**Verification:** Run a few workflows, watch the indicator climb. Toggle kill-switch from admin; confirm subsequent runs return 503.

**Checkpoint:** Alan validates the kill-switch UX from both admin and user-side.

---

## Phase 16 — Tests, lint, README

**Goal:** Test coverage is reasonable. Linters pass. README is real.

**Files created/modified:**
- Backend: ensure each package has tests; integration test for the full happy-path against a test database.
- Frontend: a few component tests for the workflow builder (drag-drop, edge creation, validation surfacing) and the agent form. End-to-end: one Playwright test driving signup → fork happy path → run.
- `golangci-lint` config; `eslint`/`prettier` config.
- `README.md` populated: project overview, screenshot/gif placeholder, "what it does," local setup, architecture diagram (described in markdown for now), pointer to CLAUDE.md / TYPES.md / DATABASE_SCHEMA.md / AGENT_TEMPLATES.md / STEPS.md / DECISION_LOG.md.
- `Makefile` — `make test`, `make lint`, `make ci`.

**Verification:** `make ci` is green from a clean checkout.

**Checkpoint:** Alan reviews README for tone and accuracy.

---

## Phase 17 — Local Docker Compose for the full stack

**Goal:** `docker compose up` from a clean clone brings up postgres + api + web fully wired, with a one-command seed.

**Files created/modified:**
- `api/Dockerfile` — multi-stage Go build.
- `web/Dockerfile` — Next.js standalone output.
- `docker-compose.yml` — postgres + api + web; api depends_on postgres healthy; web depends_on api. `NEXTAUTH_SECRET` and `ANTHROPIC_API_KEY` injected from `.env`.
- `docker-compose.dev.yml` — hot-reload variant for development (volumes mounted, `air` for Go, `npm run dev` for Next).
- Makefile: `up`, `down`, `logs`.

**Verification:** Clean clone + `make up && make seed` produces a working app at `http://localhost:3000`.

**Checkpoint:** Alan does this from a fresh clone in a different directory to verify nothing is hardcoded to his dev path.

---

## Phase 18 (deferred, out of v0 scope) — VPS deployment

Not included in this build. When v0 is feature-complete and Alan decides to deploy, this becomes its own dedicated session: Hetzner VPS provisioning, Postgres + Go binary + Next.js (`next start`) as systemd services, Nginx reverse proxy, Cloudflare DNS, Certbot TLS. The pattern from `oldephraimlearnstocode.wordpress.com/2024/11/25/tarot-project-deployment/` is the template — Hetzner instead of EC2, three systemd services instead of two.

---

## Cross-phase principles

- **Don't skip ahead.** Each phase has a checkpoint for a reason.
- **`DECISION_LOG.md` gets an entry every phase.** Even if the entry is "no deviations from CLAUDE.md."
- **Tests that prove behavior > tests that prove implementation.** Prefer testing through public interfaces.
- **No third-party services beyond Anthropic and Polymarket.** No SMTP, no analytics, no error reporting.
- **The 90-second per-step timeout, 10-minute per-run timeout, 50-step run cap, 6-round tool cap, and 1-retry validation cap are non-negotiable.** They exist because of the Apacen surprise-billing incident. Do not loosen without an explicit DECISION_LOG entry.
- **Edge fetches are always `ORDER BY priority ASC, id ASC`.** Drop this and tests will be flaky.
- **`strategy_market_subscriptions` always sync transactionally** with `workflows` updates.
- **When CLAUDE.md/TYPES.md/DATABASE_SCHEMA.md/AGENT_TEMPLATES.md and STEPS.md disagree, the architecture docs win.** STEPS.md describes sequencing; the others describe the system.

---

## Estimated effort

This is **the Phase 6 risk**: the engine has fan-in, fan-out, branching, loops, timeouts, and per-step retry, all in one component, all needing thorough tests. The original "4-5 day" estimate was aggressive. Realistic budget at a comfortable pace, with Claude Code typing and Alan reviewing/checkpointing:

- Phases 0–3: half a day to a full day (foundation).
- Phases 4–5: half a day (runtime + tools).
- **Phase 6: 1–2 days.** This is the danger zone.
- Phase 7: half a day.
- Phase 8: half a day to a full day.
- Phases 9–13: 2–3 days (frontend, including all the React Flow work).
- Phases 14–14.5: half a day (seeds + reliability iteration may take longer if prompts need work).
- Phases 15–17: half a day.

**Total: 1 to 1.5 weeks at a comfortable pace.** Plan accordingly. If anything gets blocked or boring, recalibrate rather than push through — Oracle Garden has no external deadline, so optimizing for "does this still feel good to work on tomorrow" is the right move.