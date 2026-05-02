# CLAUDE.md — Sibyl Hub

This file is the architectural source of truth. Three companion files extract dense reference material that is likely to grow or be referenced independently:

- **TYPES.md** — type system concept, all core type schemas, validation rules, multi-input merge semantics, loop semantics for typing.
- **DATABASE_SCHEMA.md** — full SQL schema for all tables.
- **AGENT_TEMPLATES.md** — built-in agent template specifications, system prompt sketches, the engine-side `trading_decision.v1` → `paper_trades` mapping, and the roadmap for future templates.

When CLAUDE.md and STEPS.md disagree, CLAUDE.md wins. When CLAUDE.md and TYPES.md / DATABASE_SCHEMA.md / AGENT_TEMPLATES.md disagree on a topic those files own (types, schema, agent specifications), the companion file wins.

---

## Project Context

Sibyl Hub is a platform where users build trading strategies for prediction markets out of composable AI agents. Each strategy is a workflow on a visual canvas; each node is an agent with a typed input and output schema; agents pass structured data to each other; the terminal node of any strategy emits a `trading_decision.v1` that the platform records as a paper trade.

The conceptual frame is "GitHub for agent workflows," with prediction-market trading as the v0 domain. Workflows and agents both have owners, visibility settings, and fork lineage from day one — even though the v0 UI shows none of that. The marketplace/social layer is a v2 concern; the data model needs to be ready for it.

The name comes from the metaphor that **each agent is a sibyl** — a prophetess delivering oracular advice — and the platform is the **hub** where many sibyls live and operate. Each sibyl has her own voice, cultivated by her keeper; disagreement among sibyls is a feature, not a bug — the platform earns its keep by helping users find which sibyls speak truer than others, on which markets, under which conditions.

Workflows remain "workflows" — we considered "consultations" and decided against, because a sibyl's workflow can take independent action (a paper trade), not just report back. The terminal `trading_decision.v1` may be referred to as a *responsum* in code-comment flavor, but UI text says "response" or "trading decision" plainly.

For v0 we ship: auth, Polymarket-only integration (read-only), 5 built-in agent templates, an agent builder, a workflow builder with branching and loops, a typed schema system with versioning, scheduled execution, paper trading, a monitoring UI, and cost-protection guardrails. We do **not** ship: real-money trading, backtesting, price/event triggers, the social UI, custom tool wiring per user-built agent, deployment to production, Kalshi integration, or news-source customization.

The v0 demo target — what every architectural decision must enable — is this:

> Sign up. Browse the 5 built-in agents. Build a 6th custom agent in the agent builder ("Devil's Advocate — re-examines a thesis against the news, looking for dissenting evidence, and emits a revised thesis"). Drop it into a fork of the happy-path strategy between Thesis Builder and Risk Assessor. Run on a live Polymarket market. Watch the monitoring UI as agents fire across the canvas. See the paper trade recorded with the full reasoning chain. Trigger a re-run to demonstrate scheduled execution and loop semantics.

If that flow works smoothly end-to-end, v0 is shipped.

---

## Repository Structure

Single monorepo:

```
sibyl-hub/
├── api/                        # Go backend
│   ├── cmd/server/main.go      # Entry point
│   ├── internal/
│   │   ├── auth/               # JWT verification middleware
│   │   ├── agents/             # Agent template CRUD
│   │   ├── workflows/          # Workflow CRUD + type-compat validation
│   │   ├── engine/             # Graph executor: branching, loops, fan-out/fan-in
│   │   ├── runtime/            # Anthropic API client wrapper, tool dispatch
│   │   ├── tools/              # Built-in tool implementations
│   │   ├── types/              # Type registry, JSON Schema validator
│   │   ├── polymarket/         # Polymarket adapter (Gamma + CLOB public)
│   │   ├── runs/               # Run lifecycle, agent_steps tracking
│   │   ├── scheduler/          # robfig/cron loop, kill-switch enforcement
│   │   ├── billing/            # Cost metering, daily quota enforcement
│   │   ├── sse/                # Run event broadcaster with replay buffer
│   │   ├── api/                # HTTP handlers, Chi router
│   │   └── db/                 # pgx pool, migrations, repositories
│   ├── migrations/             # golang-migrate SQL files
│   ├── seed/                   # Built-in agent templates + happy-path workflow JSON
│   └── Dockerfile
├── web/                        # Next.js 14 frontend
│   ├── app/
│   │   ├── (auth)/             # signin, signup
│   │   ├── (app)/              # authenticated routes
│   │   │   ├── dashboard/
│   │   │   ├── agents/         # list, [id], new
│   │   │   ├── workflows/      # list, [id], new, [id]/runs/[runId]
│   │   │   ├── markets/        # Polymarket browser
│   │   │   └── account/        # profile, usage view
│   │   └── api/auth/[...nextauth]/route.ts
│   ├── components/
│   │   ├── flow/               # React Flow nodes/edges
│   │   ├── agents/             # Agent form, schema picker
│   │   ├── runs/               # Run timeline, agent step cards
│   │   └── markets/            # Market card, search
│   ├── lib/
│   │   ├── api.ts              # Backend API client
│   │   ├── sse.ts              # SSE hook
│   │   ├── auth.ts             # NextAuth config (JWT strategy, custom encode/decode)
│   │   └── ajv.ts              # Runtime JSON Schema validator
│   └── tailwind.config.ts
├── docker-compose.yml          # postgres + api + web for local dev
├── .env.example
├── CLAUDE.md
├── TYPES.md
├── DATABASE_SCHEMA.md
├── AGENT_TEMPLATES.md
├── STEPS.md
├── DECISION_LOG.md
└── README.md
```

---

## Stack

| Layer | Technology | Why |
|---|---|---|
| Backend language | Go 1.22+ | Concurrency primitives suit the graph executor and SSE broadcaster. Familiar from Maestro/Apacen. |
| HTTP router | Chi | Idiomatic, no framework magic. |
| DB driver | pgx/v5 | Direct SQL; no ORM. |
| Migrations | golang-migrate | SQL-first, versioned. |
| Database | PostgreSQL 16 | Workflow state, agent steps, paper trades, type registry. JSONB for output payloads. |
| Scheduler | robfig/cron | In-process cron loop; sufficient for v0. |
| Agent runtime | Anthropic Messages API direct | Sonnet 4.6 default, Haiku 4.5 cheap option, Opus 4.7 premium option. Structured outputs validated against the type registry; tool use for agents that need it. No Goose. |
| Web search | Anthropic's built-in `web_search` server-side tool | No third-party search provider. |
| Frontend | Next.js 14 (App Router) + TypeScript | SSR for auth-gated pages. |
| Workflow canvas | React Flow | Familiar from Maestro. |
| Schema validation | AJV on frontend, `santhosh-tekuri/jsonschema/v6` on backend | Runtime validation against schemas from the type registry. No Zod, no build-time generation. (AJV is JS-only.) |
| Auth | NextAuth credentials provider, **JWT session strategy** | HS256 JWT signed with `NEXTAUTH_SECRET`, verified Go-side with the same secret. No `sessions` table. NextAuth's `jwt.encode`/`jwt.decode` are overridden with `jsonwebtoken` HS256 (JWS, not JWE) so Go can verify with a standard library. |
| Styling | Tailwind | One small in-house component library; no shadcn or Radix. |
| Observability | `slog` + `net/http/pprof` (admin port) + `runs`/`agent_steps` token/cost metering | All in-process. |
| Profile | `pprof` mounted on `:6060` (loopback only) | Same pattern as Apacen. |

Approximate model identifiers used in agent template defaults: `claude-sonnet-4-6`, `claude-haiku-4-5`, `claude-opus-4-7`. **Confirm exact strings (alias vs. dated suffix forms like `claude-haiku-4-5-20251001`) against current Anthropic API documentation in Phase 1**, before writing the seed migrations or `pricing.go`. Easier to write once than to update three times across migrations, seeds, and the pricing table.

---

## Polymarket API Reference

Polymarket exposes three public APIs. We use two; we never write to any of them.

**Gamma API — `https://gamma-api.polymarket.com`** — public, no auth. Discovery layer.
- `GET /markets` — list with filters (active, closed, archived, liquidity_min, etc.). Keyset pagination.
- `GET /markets/{slug-or-id}` — single market.
- `GET /events` — event groupings.
- `GET /search` — full-text search.

A **Market** object includes `id`, `slug`, `question`, `conditionId`, `description`, `outcomes`, `outcomePrices`, `clobTokenIds` (the ones to use against CLOB), `liquidity`, `volume24hr`, `endDate`, `active`, `closed`, `archived`, `tags`. Some fields stringify numbers; the adapter coerces.

**CLOB API — `https://clob.polymarket.com`** — public read endpoints, auth required only for orders. We use only the public reads.
- `GET /book?token_id=...` — full order book.
- `GET /price?token_id=...&side=BUY|SELL` — best price for a side.
- `GET /midpoint?token_id=...` — midpoint between bid and ask.
- `GET /spread?token_id=...` — spread.
- `GET /prices-history?market={token_id}&interval=1h&fidelity=60` — historical prices.

**Critical gotcha:** CLOB endpoints take `token_id` (one of the values in Gamma's `clobTokenIds` array — typically two, one per outcome), **not** the `conditionId`. The adapter always resolves slug → conditionId + clobTokenIds via Gamma, then uses the token_id against CLOB.

**Data API — `https://data-api.polymarket.com`** — public, no auth. **Not used in v0.** Reserved for v1+ (positions, trade history, leaderboards).

**Rate limits.** Approximate per Polymarket's published guidance: ~60 req/min Gamma unauthenticated, ~100+/min CLOB. Limits have changed historically — the adapter is defensive: in-memory LRU cache (5-min TTL for market metadata, 30-sec TTL for prices, 60-sec TTL for orderbooks), a token-bucket limiter (50 req/min default, configurable), exponential backoff on 429 with jitter.

**What we do NOT use in v0.** WebSocket subscriptions (polling on schedule is sufficient). The price/event-trigger feature in v1 will introduce a single shared WebSocket consumer.

---

## Workflow Execution Engine

The engine runs a graph executor — branching, loops, fan-out, and fan-in are all first-class.

### Execution model

A workflow run executes as follows:

1. **Initialize.** Create a `workflow_runs` row with status `running`. Build an in-memory graph from `workflow_nodes` and `workflow_edges` (fetched with `ORDER BY priority ASC, id ASC` on edges). Identify entry nodes (no incoming edges). Seed each entry node with `market_target.v1` derived from the run's `market_slug`.
2. **Ready queue.** A node N is "ready" for its next iteration when, per its `join_strategy`:
   - **`all`:** every upstream node has produced ≥1 output total. Subsequent iterations fire whenever a *new* output arrives at any upstream.
   - **`first`:** at least one upstream has produced output since N's last firing.
   In all cases, the merged input contains the **latest** output from each upstream node, regardless of which iteration it came from.
3. **Step loop.** Pop a node, gather its inputs, invoke the agent with a 90-second timeout, validate the output against the registered output type, persist an `agent_steps` row, broadcast SSE events.
4. **Edge evaluation.** After a node completes, evaluate its outgoing edges in priority order (see "Edge condition rules" below).
5. **Termination.** Run terminates when the ready queue empties (success), a step fails after retries (`failed`), the run timeout (10 minutes) expires (`timed_out`), the global step limit (50) is hit (`failed`), or the kill switch is flipped mid-run (`killed`).

### Edge condition rules

Outgoing edges of a node are evaluated in priority order, with two kinds of conditions:

- **`always` edges always fire.** Multiple `always` edges from the same node enable fan-out — all targets are activated.
- **Conditional edges (non-`always`) are mutually exclusive.** Among them, the first match by priority wins; remaining conditional edges from this node are not evaluated for this firing.

Both kinds can coexist on the same node: every `always` edge fires AND the first matching conditional edge fires.

**Worked example.** A Risk Assessor (the only node type whose output legitimately matches `approved`/`rejected`) with edges:
- A: `condition = always`, priority 0 → fan-out to logger
- B: `condition = approved`, priority 1 → terminal
- C: `condition = rejected`, priority 2 → loop back to upstream

If the agent's output has `approved: true`: A fires (fan-out), B fires (first matching conditional). C is not evaluated.

If the agent's output has `approved: false`: A fires, B does not match, C fires.

### Approved / Rejected — restricted to risk_assessment.v1

The conditions `approved` and `rejected` are first-class because they're how risk gating is expressed. **Workflow save validates that any edge with `condition ∈ {approved, rejected}` has a source node whose `output_type = risk_assessment.v1`.** Hard error otherwise. This prevents the silent-failure mode of wiring `approved` to a node whose output has no `approved` field.

```go
func conditionMatches(output []byte, condition string) bool {
    cond := strings.ToLower(strings.TrimSpace(condition))
    switch cond {
    case "always", "":
        return true
    case "approved":
        return jsonHasField(output, "approved") && jsonBoolField(output, "approved", false)
    case "rejected":
        return jsonHasField(output, "approved") && !jsonBoolField(output, "approved", true)
    default:
        // Substring match against the JSON-stringified output. Brittle for v0; v1
        // will introduce structured edge conditions ({field, op, value}).
        return strings.Contains(strings.ToLower(string(output)), cond)
    }
}
```

Substring matching is a known v0 limitation. Document on the edge config UI: "matches if this text appears anywhere in the agent's output JSON." Recommend users write conditions that target distinctive output values (e.g., `direction:YES`, not `yes`).

### Loops

A loop edge is any edge whose target node has already completed in the current run. When followed, the target node's iteration counter increments and a new `agent_steps` row is created. If the new iteration would exceed `loop_iteration_limit`, the run is marked `failed` with error "loop iteration limit reached at node {node_key}".

The default Risk → Thesis feedback loop pattern uses a limit of 5 on Thesis.

### Per-step timeout

Every agent step is wrapped in `context.WithTimeout(ctx, 90*time.Second)`. On timeout the step is marked `timed_out` and the run fails. Same fail-fast philosophy as Maestro's PSP analogy.

---

## Agent Runtime

The agent runtime calls the Anthropic Messages API directly. No Goose. No third-party agent framework.

### Per-step flow

```
1. Resolve agent_template (with config_overrides applied per-node).
2. Build messages:
   - System = agent's system_prompt
            + structured-output instructions
            + (if multi-input) a description of the input keys present
   - User   = JSON-stringified merged inputs
            + "Respond ONLY with a JSON object matching this schema: {schema}"
3. If agent declares tools:
     - Pass tool definitions in the API call.
     - Loop: call API; for tool_use blocks, dispatch via tool registry, append tool_result, call again.
     - Cap at 6 tool-use rounds per step.
4. Extract final assistant text. Strip ```json fences if present.
5. JSON-parse. Validate against output_type's JSON Schema (`santhosh-tekuri/jsonschema/v6` server-side; AJV in the frontend type-preview UI).
6. On parse/validate failure, retry ONCE with the validation error appended as a system note.
7. Record token counts (from the API usage block) and compute cost from a static price table.
```

Worst-case API call count per run: 50 steps × 2 attempts × 6 tool rounds = 600 calls. Typical happy-path run is 5-7 steps × ~1.2 attempts × ~2 tool rounds ≈ 15-20 calls ≈ $0.05-0.20. The $5 daily cap allows ~25-100 demo runs.

Combined with the 10-minute per-run timeout and 90-second per-step timeout, a single run cannot exceed ~6-7 maximum-length steps. Any run that hits even one 90-second step has burned 15% of its run budget; this is a known constraint.

### Cost calculation

`api/internal/billing/pricing.go` carries a static map of model → ($/Mtoken input, $/Mtoken output). Cost per step = `(prompt_tokens * input_rate + completion_tokens * output_rate) / 1_000_000`. Persisted on the `agent_steps` row and aggregated into `user_usage_daily` via a transaction (cost increment + step finalize in one BEGIN/COMMIT).

### Tools registry

`api/internal/tools/` exposes:

```go
type Tool interface {
    Name() string
    Definition() anthropic.ToolDefinition
    IsServerSide() bool      // true = handled by Anthropic (e.g., web_search); engine doesn't dispatch locally
    Invoke(ctx context.Context, args json.RawMessage) (json.RawMessage, error)
}
```

Server-side tools are passed through to the API but never locally dispatched. v0 ships:

| Tool name | Server-side? | Backed by | Purpose |
|---|---|---|---|
| `polymarket.gamma_get_market` | No | Gamma adapter | Resolve a market by slug → metadata, conditionId, clobTokenIds. |
| `polymarket.gamma_search_markets` | No | Gamma adapter | Free-text search. |
| `polymarket.clob_get_orderbook` | No | CLOB adapter | Full orderbook by token_id. |
| `polymarket.clob_get_midpoint` | No | CLOB adapter | Midpoint price by token_id. |
| `polymarket.clob_get_prices_history` | No | CLOB adapter | Historical prices by token_id. |
| `web_search` | Yes | Anthropic built-in | News / research. |

User-built agents in v0 select from this fixed set. Custom tools are a v2 feature.

---

## Built-in Agent Templates

See **AGENT_TEMPLATES.md** for the full specification of all v0 system agent templates: Market Watcher, News Scout, Thesis Builder, Risk Assessor, and Paper Executor. That file owns the per-agent I/O contracts, tool requirements, system prompt sketches, and the engine-side `trading_decision.v1` → `paper_trades` mapping. It also documents the roadmap for future agents (Devil's Advocate, Position Tracker, Whale Watcher, etc.) so the type system stays forward-compatible.

When AGENT_TEMPLATES.md and CLAUDE.md disagree on agent specifics, AGENT_TEMPLATES.md wins; this file owns workflow-engine semantics, not agent content.

---

## Built-in Workflow Template

One workflow seeded as `is_system = true`:

### "Happy Path" — fan-in + risk feedback loop

```
[Market Watcher] ───┐
                    ├──► [Thesis Builder] ──► [Risk Assessor] ──┬─[approved]──► [Paper Executor]
[News Scout]    ───┘                                            │
                                                                └─[rejected]──► [Thesis Builder]  (loop, max 5)
```

- Watcher → Thesis: `condition = always`, `priority = 0`.
- Scout → Thesis: `condition = always`, `priority = 0`.
- Thesis Builder: `join_strategy = all`, `loop_iteration_limit = 5`.
- Thesis → Risk: `condition = always`.
- Risk → Executor: `condition = approved`, `priority = 0`.
- Risk → Thesis (loop back): `condition = rejected`, `priority = 1`.

This template exercises every engine feature: branching (Risk → Executor vs Thesis), loops (Risk rejection back to Thesis), fan-in (Watcher + Scout → Thesis with all-join). Fan-out is not used in this template but is supported.

The user's demo customization — adding a "Devil's Advocate" agent — slots in by forking this workflow, removing the existing Thesis → Risk edge, and adding three new edges (Thesis → DA, Scout → DA, DA → Risk):

```
[Market Watcher] ───┐
                    ├──► [Thesis Builder] ──► [Devil's Advocate] ──► [Risk Assessor] ──► ...
[News Scout]    ───┴──────────────────────────────▲
                                                  │
                                                  └─── (Scout → DA edge, supplies news_digest.v1)
```

Devil's Advocate accepts `thesis.v1` (from Thesis Builder) and `news_digest.v1` (from Scout, via the new edge), emits `thesis.v1`, and re-examines the original thesis against the news looking for dissenting evidence. It can lower confidence, flip direction, or pass the thesis through unchanged. The Risk Assessor's `rejected` loop edge still targets Thesis Builder (not DA), so on retries the original Thesis Builder revises and DA re-evaluates.

---

## Cost Protection

Hard mechanisms, all enforced server-side:

1. **Per-user daily run quota.** `MAX_RUNS_PER_USER_PER_DAY` env, default `50`. Enforced via the same atomic upsert pattern as the cost cap (mechanism #10):
    ```sql
    INSERT INTO user_usage_daily (user_id, day, run_count)
    VALUES ($1, CURRENT_DATE, 1)
    ON CONFLICT (user_id, day) DO UPDATE
      SET run_count = user_usage_daily.run_count + 1
    RETURNING run_count;
    ```
    The handler reads the returned value and, if it exceeds the cap, immediately marks the run `quota_exceeded` and short-circuits before any agent step fires. Two concurrent runs by the same user can't both pass a pre-check and overshoot. Over-counting in `quota_exceeded` cases is acceptable (the conservative direction); we don't decrement.
2. **Per-user daily cost cap.** `MAX_COST_USD_PER_USER_PER_DAY` env, default `5.00`.
3. **Minimum schedule interval.** `MIN_SCHEDULE_INTERVAL_SECONDS` env, default `300`. Validated at workflow save by parsing the cron expression with robfig/cron, calling `Next(now)` twice, and rejecting if the diff is below threshold. (Pattern-matching on the literal cron string is wrong — `*/2 * * * *` evaluates to 2 minutes, which the regex approach misses.)
4. **Global kill switch.** `system_config['kill_switch']` (bool). When `true`, the scheduler skips all firings and the manual-run endpoint returns 503. Toggleable via admin API. Boot value gated by `EXECUTION_KILL_SWITCH_DEFAULT` for fail-safe.
5. **Per-step timeout.** 90 seconds.
6. **Per-run total step limit.** 50.
7. **Per-run timeout.** 10 minutes.
8. **Tool-use round cap.** 6 per step.
9. **Validation retry cap.** 1 retry per step (so 2 total attempts).
10. **Cost accounting transaction.** When an `agent_steps` row finalizes with cost data, the same transaction increments `user_usage_daily.total_cost_usd` using an atomic upsert that returns the post-increment value:
    ```sql
    INSERT INTO user_usage_daily (user_id, day, run_count, total_tokens, total_cost_usd)
    VALUES ($1, CURRENT_DATE, 0, $2, $3)
    ON CONFLICT (user_id, day) DO UPDATE
      SET total_tokens   = user_usage_daily.total_tokens   + EXCLUDED.total_tokens,
          total_cost_usd = user_usage_daily.total_cost_usd + EXCLUDED.total_cost_usd
    RETURNING total_cost_usd;
    ```
    The handler reads the returned value and, if it exceeds the cap, marks the run `quota_exceeded` so subsequent steps don't fire. This is atomic at the row level under PostgreSQL's default isolation, so two concurrent runs by the same user can't both pass a pre-check and double-spend the budget. Already-paid-for tokens don't roll back.

Admin UI lives at `/account/admin` for users in `ADMIN_EMAILS`. Shows today's totals, top users by cost, kill-switch toggle.

---

## Auth Model

NextAuth on the frontend, credentials provider with bcrypt-hashed passwords stored in `users`, **JWT session strategy**.

**JWT setup.** NextAuth's default JWT strategy with credentials provider produces JWE (encrypted), awkward to verify outside Node. Override `jwt.encode` and `jwt.decode` to use **`jsonwebtoken` HS256 with `NEXTAUTH_SECRET`** — produces standard JWS that `golang-jwt/jwt/v5` verifies cleanly. Claims: `sub` = user_id, `email`, `iat`, `exp` (7 days). Cookie: `__Secure-next-auth.session-token` (prod), `next-auth.session-token` (dev).

**Secret encoding.** Both sides treat `NEXTAUTH_SECRET` as a **UTF-8 byte string used directly as the HMAC key — no base64 decoding.** This matches `jsonwebtoken`'s default and Go's `[]byte(os.Getenv(...))`. If generated with `openssl rand -base64 32`, the resulting base64 string itself is the key. Mismatched encoding produces silent "invalid signature" errors. Document in `.env.example`.

**Go middleware.** Reads the JWT cookie, verifies HS256 signature against `NEXTAUTH_SECRET`, attaches `user_id` to request context. No DB lookup per request. Cookie name: tries `__Secure-next-auth.session-token` first (production HTTPS), falls back to `next-auth.session-token` (dev). Same code runs in both. Override via `SESSION_COOKIE_NAME` env if needed.

**Trade-off:** no server-side revocation. A user signs out → cookie clears client-side, but the JWT remains valid until expiry. Acceptable for v0 paper trading; document in DECISION_LOG. v1+ can add a revocation list (small Redis or DB-backed deny list keyed by JWT `jti`) without changing the rest of the system.

**Sign-up flow.**
1. User submits email + password (min 8 chars) to **`POST /api/users`** (Go, not under `/api/auth/*`).
2. Server hashes with bcrypt (cost 10), inserts the `users` row, returns 200 (no JWT, no cookie).
3. Frontend immediately calls NextAuth's `signIn('credentials', { email, password, redirect: false })`. NextAuth's `authorize` callback in turn calls **`POST /api/users/verify-credentials`** on the Go backend (server-to-server, internal endpoint), receives a 200 with user info on valid credentials, and NextAuth produces the JWT cookie.

**Single JWT issuance path:** all JWTs are issued by NextAuth. The Go signup handler never produces a JWT. This avoids drift between two issuers' claim shapes (`sub`/`email`/`iat`/`exp`) and means there's exactly one place where token format can break.

The trade-off is one extra round trip on signup (signup → 200 → signIn → cookie). Acceptable.

No email verification in v0 (would require SMTP). No password reset in v0.

---

## Backend API

All routes prefixed `/api`. JSON in/out unless noted. JWT cookie validated by middleware except where marked `(no auth)`.

**Routing namespace.** NextAuth owns `/api/auth/*` exclusively (Next.js catch-all `[...nextauth]` route). The Go backend lives at every other `/api/*` path; in particular, user creation and credential verification are NOT under `/api/auth/*` on the Go side.

```
# === User account management (Go, NOT under /api/auth/*) ===
POST   /api/users                            (no auth) — create user
POST   /api/users/verify-credentials         (no auth, internal — called by NextAuth's
                                              authorize callback over server-to-server HTTP;
                                              returns 200 + user info if credentials valid,
                                              401 otherwise. Not for browser use.)

# === Sign-in / sign-out (NextAuth, frontend only) ===
# /api/auth/signin       — NextAuth credentials route (browser → Next.js)
# /api/auth/signout      — NextAuth (browser → Next.js, clears cookie)
# /api/auth/[...nextauth] — NextAuth catchall
# Go has no signout handler. JWTs are stateless; NextAuth's signout clears the cookie
# client-side, which is sufficient for v0.

GET    /api/me                            current user + today's usage
GET    /api/me/usage                      30-day usage history

GET    /api/agent-templates               list (mine + system + public)
POST   /api/agent-templates               create
GET    /api/agent-templates/:id
PATCH  /api/agent-templates/:id
DELETE /api/agent-templates/:id           (only if owner_id = me)
POST   /api/agent-templates/:id/fork

GET    /api/workflows                     list (mine)
POST   /api/workflows                     create — runs full type-compat validation
GET    /api/workflows/:id
PATCH  /api/workflows/:id                 update — re-runs type-compat validation,
                                          syncs strategy_market_subscriptions in same transaction
DELETE /api/workflows/:id
POST   /api/workflows/:id/fork
POST   /api/workflows/:id/run             body: {market_slug?: string} → {run_id}

GET    /api/runs/:id                      run detail with all agent_steps
GET    /api/runs/:id/events               SSE stream (with replay buffer)
GET    /api/workflows/:id/runs            list (paginated)

GET    /api/markets/search?q=...          proxy to Gamma /search
GET    /api/markets/:slug                 proxy to Gamma /markets/{slug}

GET    /api/types                         list registered types
GET    /api/types/:name/:version          schema

GET    /api/admin/usage                   (admin only)
POST   /api/admin/kill-switch             (admin only)
```

### Manual run with multi-target workflows

A workflow may declare multiple `market_targets`. Behavior:

- **Manual run, body has `market_slug`:** that slug is used (must be one of `market_targets` or accepted as ad-hoc).
- **Manual run, body omits `market_slug`, single target:** that single target is used.
- **Manual run, body omits `market_slug`, multiple targets:** 400 error, "specify market_slug".
- **Scheduled run:** the scheduler iterates `market_targets` and creates **one run per target** per cron tick.

### SSE event shape

Events emitted on `/api/runs/:id/events`:

```ts
type RunEvent =
  | { type: 'RunStarted',       run_id, workflow_id, started_at }
  | { type: 'NodeReady',        node_key, iteration }
  | { type: 'StepStarted',      step_id, node_key, iteration }
  | { type: 'StepProgress',     step_id, message }                  // tool-use rounds
  | { type: 'StepCompleted',    step_id, node_key, output_type, output_data, cost_usd, latency_ms }
  | { type: 'StepFailed',       step_id, node_key, error_message }
  | { type: 'EdgeFollowed',     from_node_key, to_node_key, condition, iteration }
  | { type: 'PaperTradeRecorded', paper_trade_id, market_slug, side, size_usd, entry_price }
  | { type: 'RunCompleted',     run_id, status, finished_at }
```

**Replay buffer.** The broadcaster keeps a per-run ring buffer of all events since `RunStarted`. When a client subscribes, the buffer flushes to it before live events stream. This eliminates the SSE-attachment race and lets the engine fire its first agent step immediately (no 2-second delay needed).

---

## Frontend Architecture

Next.js 14 App Router, TypeScript, Tailwind. React Flow for the canvas. Three top-level page groups:

- **`(auth)`** — `/signin`, `/signup`. No chrome.
- **`(app)`** — authenticated. Layout includes nav (Dashboard / Markets / Agents / Workflows / Account) and a usage indicator.
- **`api/auth/[...nextauth]`** — NextAuth route handler with custom JWT encode/decode.

Key pages:

- **Dashboard (`/dashboard`)** — recent runs, active workflows, daily usage chart (Recharts), kill-switch indicator.
- **Markets (`/markets`)** — search, result cards, market detail with "Run a strategy on this market" CTA.
- **Agents (`/agents`)** — split tabs: System / Mine / Forked. Each card shows name, output_type, tool list, "Use in workflow", "Fork".
- **Agent Builder (`/agents/new`, `/agents/:id`)** — form: name, description, system prompt textarea, model dropdown (sonnet-4-6, haiku-4-5, opus-4-7), temperature slider, max_tokens, output_type dropdown (with schema preview), input_types multi-select, tools multi-select.
- **Workflow Builder (`/workflows/new`, `/workflows/:id`)** — React Flow canvas. Side panel of agents (drag to canvas). Right panel for node config (`node_key`, `join_strategy`, `loop_iteration_limit`, `config_overrides`) or edge config (`condition`, `priority`, presets). Validate button surfaces type-compat errors near affected edges. Run button opens a market_slug modal.
- **Run Detail (`/workflows/:id/runs/:run_id`)** — left half: read-only graph with status-tinted nodes. Right half: time-ordered timeline of steps and edges followed, expandable to show full input/output JSON. Paper-trade banner if recorded. Live updates via SSE; replay buffer ensures no events are missed even on late attach.
- **Account (`/account`, `/account/admin` if in `ADMIN_EMAILS`)** — display name, change password, daily-usage history; admin sub-page for kill-switch and platform-wide stats.

Schema validation in the agent builder uses **AJV** loaded from the type registry at runtime. No build-time Zod generation. The same AJV instances that validate user-built agent forms also validate the JSON-schema preview side panel.

### Cross-process routing

Next.js (port 3000) and the Go API (port 8080) run as separate processes. Browser requests to `/api/*` reach the right process via `next.config.js` rewrites: `/api/auth/*` stays on Next.js for NextAuth; everything else under `/api/*` rewrites to `${process.env.INTERNAL_API_URL}/api/*` (the Go backend). The frontend calls relative paths only (`fetch('/api/me')`) — no absolute URLs, no CORS configuration. NextAuth's `authorize` callback runs server-side on Next.js and calls Go's `/api/users/verify-credentials` directly via the absolute internal URL (server-to-server, bypasses rewrites). Exact rewrites config in STEPS.md Phase 9.

`INTERNAL_API_URL` is `http://localhost:8080` in dev, `http://api:8080` in Docker Compose, and the production VPS internal URL in deployment.

---

## Local Development

```bash
cp .env.example .env
# fill in ANTHROPIC_API_KEY and NEXTAUTH_SECRET at minimum

docker compose up -d postgres
make migrate-up
make seed
cd api && go run cmd/server/main.go     # :8080
cd web && npm install && npm run dev    # :3000
```

Or `docker compose up` to bring all three services up.

`Makefile` targets: `migrate-up`, `migrate-down`, `seed`, `test`, `lint`, `pprof` (opens `:6060` admin), `up`, `down`, `logs`.

---

## Observability

- **`slog`** structured JSON logs from the Go service. One log entry per major event (run start/end, step complete, tool invocation, error). Always include `run_id`, `step_id`, `user_id`, `node_key` when in scope.
- **`net/http/pprof`** mounted on `:6060`, bound to `127.0.0.1`. Production debugging value, same as Apacen.
- **`runs` + `agent_steps` tables** are the in-DB observability layer. Token counts, latency, cost — queryable via psql and via the admin UI.
- No Datadog, no Sentry, no third-party observability.

---

## Out of Scope for v0 (explicit list)

- Real-money trading / wallet integration / Polymarket order placement.
- Backtesting against historical Polymarket prices-history data.
- Price/event triggers — only `schedule_cron` in v0.
- Public/unlisted UI — workflows and agents are technically `visibility = 'private'` always; field exists, UI doesn't toggle it.
- Forking UI — API endpoint exists; UI doesn't yet.
- Custom tools registered by users — registry is hardcoded in v0.
- Email verification, password reset, OAuth providers.
- Server-side session revocation / JWT deny list.
- Kalshi or other prediction-market integrations.
- WebSocket-driven real-time market subscriptions.
- Production deployment — handled separately after v0 is feature-complete.
- Comparative analytics / leaderboards / "top forks".
- Structured edge conditions ({field, op, value}) — v0 uses substring matching for custom conditions.
- Build-time Zod generation from JSON Schema — v0 uses AJV at runtime.
- Prompt-evaluation regression harness — flagged in DECISION_LOG.

If a question comes up about scope, the rule is: **does this contribute to the v0 demo target?** If not, defer.

---

## Decision Log

A separate `DECISION_LOG.md` tracks architectural decisions made during the build. Phase 0 seeds it with decisions from the design conversation:

- Hetzner over EC2 for production VPS (lower flat cost, no surprise data-transfer billing).
- NextAuth credentials provider with JWT session strategy, custom HS256 encode/decode.
- No Goose runtime — direct Anthropic API calls.
- JSON Schema as type-system source of truth, AJV at runtime on both sides.
- 5 system agents, happy-path-with-loop as the demo workflow, Devil's Advocate as the demo custom-agent.
- `input_types` is "accepts" (not "requires"); join_strategy + edges determine runtime presence.
- `approved` / `rejected` edge conditions are restricted to risk_assessment.v1 sources.
- Replay buffer in SSE broadcaster (no startup-delay hack).
- Cost protection: 50 runs/day, $5/day, 5-min minimum schedule interval, kill switch.
- Defer real money, backtesting, price/event triggers, prompt eval harness, structured edge conditions to v1+.

New entries get added as STEPS.md proceeds.