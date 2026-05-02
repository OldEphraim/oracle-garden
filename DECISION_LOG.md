# DECISION_LOG.md — Sibyl Hub

Architectural decisions made during design and build, with rationale. The goal of this file is to make sure that future Alan, future Claude sessions, and any reviewer can reconstruct *why* something was built the way it was, not just *what* was built.

Each entry has a date, a topic, the decision, and a one-or-two-sentence rationale. Reversing a decision means appending a new entry, not editing the old one — the trail of reasoning matters.

---

## Pre-build (design conversation)

- **Domain & framing.** Sibyl Hub is "GitHub for agent workflows," with prediction-market trading as the v0 domain. The "compose your own analyst" framing is the v0 user story; "competing agents" is one level up, a feature users can build with the tooling rather than the premise of it. *Rationale:* tooling products generate their own content as users surprise the platform; tournament/leaderboard products are content products gated on a curator. Aligns with the platform-owner-blogs-the-findings model.
- **Polymarket-only for v0.** Kalshi and other prediction markets deferred. *Rationale:* deep familiarity with Polymarket from Apacen; data model designed so a Kalshi adapter is a clean later addition.
- **Paper trading only for v0.** Real-money trading is v1. *Rationale:* eliminates wallet management, custody, and runaway-bot blast radius for the v0 demo; architecture is cleanly capable of swapping in a real executor later.
- **Hetzner over EC2 for production VPS.** *Rationale:* flat ~$5/month with no data-transfer surprises (the Apacen lesson); identical Debian/systemd/Nginx/Cloudflare/Certbot pattern. Stays out of the AWS billing event horizon.
- **No third-party services beyond Anthropic and Polymarket.** No SMTP, no analytics, no Sentry/Datadog. *Rationale:* friction-of-suspicion against compounding vendor lock-in; Apacen's surprise was an external dependency. v0 has the budget to do everything in-house.
- **Single monorepo, not split repos.** *Rationale:* the original Apacen frontend/backend split made cross-cutting bugs annoying to fix; co-location with Claude Code is much smoother in a single repo.

## Renaming

- **2026-05-01: project renamed from "Oracle Garden" to "Sibyl Hub"** (mid-Phase 1, before any code beyond migrations had shipped). *Rationale:* "Oracle" collides with Oracle Corporation; we don't want any branding association with that company. "Sibyl Hub" preserves the spirit of the original framing — many divinatory voices in one place — without the corporate baggage. The mythological frame is now: a single agent is a *sibyl* (a prophetess delivering oracular advice), and the platform is the *hub* where many sibyls live and operate. Workflows remain "workflows" (we considered "consultations" and decided against — workflows can take independent action, not just report back). The terminal `trading_decision.v1` may be referred to as a *responsum* in code-comment flavor, but UI text uses "response" or "trading decision" plainly. *How to apply:* never reintroduce "Oracle Garden" or "oracle-garden" in committed code/docs; use "sibyl" lowercase when reaching for the divinatory metaphor in inline comments or naming.

## Architecture

- **Go (Chi, pgx/v5, golang-migrate, robfig/cron) + PostgreSQL + Next.js 14 App Router.** *Rationale:* matches the proven Maestro stack; Go concurrency suits the graph executor and SSE broadcaster.
- **No Goose runtime — direct Anthropic Messages API calls.** *Rationale:* Maestro's Goose integration cost significant debugging on output parsing and prompt-shape coherence. Direct API calls give cleaner structured-output validation, native tool-use round-tripping, and provider-agnosticism for v2+ when users want OpenAI/Gemini/local models.
- **JSON Schema (Draft 2020-12) as type-system source of truth.** AJV in the frontend (browser, runtime), `santhosh-tekuri/jsonschema/v6` on the backend. No build-time Zod generation. *Rationale:* type registry must support runtime registration of user-defined types in v2+; AJV is JS-only so backend uses the equivalent Go library.
- **`additionalProperties: false` on all v0 core schemas.** *Rationale:* a misnamed field in agent output is exactly the kind of bug that hides when extras are tolerated; cost of strictness is low.
- **`input_types` is "accepts," not "requires."** A node may declare types it can handle; the workflow's edges + `join_strategy` determine which subset is actually present at runtime. *Rationale:* cleanest way to support optional loop-back inputs (e.g., Thesis Builder accepting `risk_assessment.v1` only on iteration ≥ 2) without SCC analysis or special-case loop edges.
- **`approved` / `rejected` edge conditions restricted to `risk_assessment.v1` sources.** Validated at workflow save. *Rationale:* prevents the silent-failure mode of wiring `approved` to an output without an `approved` field.
- **Substring matching for custom edge conditions in v0; structured `{field, op, value}` conditions deferred to v1.** *Rationale:* substring is brittle but adequate for v0; structured conditions deserve their own design pass.
- **Replay buffer in the SSE broadcaster, not startup delay.** *Rationale:* eliminates the SSE-attachment race cleanly; engine fires its first agent step immediately. Maestro's 2-second delay was a hack worth retiring.
- **Drop `version` INT columns from `agent_templates` and `workflows`.** *Rationale:* versioning happens via fork (`forked_from`); the bare `version` field had no associated logic and was noise.

## Auth

- **NextAuth credentials provider with JWT session strategy.** No `sessions` table. JWTs encoded HS256 via overridden `jwt.encode` / `jwt.decode` (using `jsonwebtoken`, not NextAuth's default JWE). *Rationale:* Go can verify HS256 with `golang-jwt/jwt/v5` standard library; JWE would require `go-jose` and more ceremony. Trade-off: no server-side revocation in v0; cookie clear is sufficient for paper-trading platform.
- **`NEXTAUTH_SECRET` is a UTF-8 byte string used directly as the HMAC key on both sides — no base64 decoding.** *Rationale:* matches `jsonwebtoken`'s default and Go's `[]byte(env)` pattern; mismatched encoding produces silent "invalid signature" errors. Documented in `.env.example`.
- **Single JWT issuance path: NextAuth only.** Go signup creates the user and returns 200; frontend then calls `signIn()` to get the JWT. *Rationale:* avoids drift between two issuers' claim shapes.
- **Go auth endpoints under `/api/users/*`, not `/api/auth/*`.** NextAuth's catch-all owns the latter. *Rationale:* avoids collision; `/api/users` for signup, `/api/users/verify-credentials` for the internal check NextAuth's `authorize` calls server-to-server.

## Engine & cost protection

- **5 system agents seeded:** Market Watcher, News Scout, Thesis Builder, Risk Assessor, Paper Executor. **Demo custom-agent is Devil's Advocate** (built by Alan during the demo, not seeded). *Rationale:* replaces the originally-considered Contrarian Filter, which would have required the Polymarket Data API (not in v0).
- **Happy-path workflow** as the seeded demo: Watcher + Scout → Thesis → Risk → (approved) Executor / (rejected loop) Thesis. *Rationale:* exercises every engine feature (branching, loop, fan-in) with a meaningful end-to-end story.
- **Cost protection caps:** 50 runs/user/day, $5/user/day, 5-min minimum schedule interval, kill switch, 90-sec per-step timeout, 10-min per-run timeout, 50-step per-run cap, 6-round per-step tool cap, 1 validation retry. *Rationale:* the Apacen surprise was data transfer; Sibyl Hub's analogous risk is LLM spend. Bound it from every direction.
- **Atomic cost and run-count accounting via `INSERT … ON CONFLICT … RETURNING`.** *Rationale:* prevents two concurrent runs by the same user from both passing a pre-check and double-spending the cap.
- **Per-resource access rules:** strictly-owned (`runs`, `paper_trades`, `agent_steps`, `user_usage_daily`) require `user_id` match; shareable (`agent_templates`, `workflows`) allow `is_system` and `visibility IN ('public','unlisted')` reads. *Rationale:* system templates need to be readable by all users; runs are sensitive.
- **`paper_trades.entry_price` is the executable price for `'open'` rows and a midpoint snapshot for `'abstained'` rows.** Column not renamed. *Rationale:* analytics queries should filter by status; the term `entry_price` is natural for v1 real-money trading.

## Deferred to v1+ (with reasons noted in CLAUDE.md "Out of Scope")

Real-money trading; backtesting against historical Polymarket data; price/event triggers; public/unlisted UI; forking UI; user-contributed tools; email verification + password reset + OAuth; JWT revocation; Kalshi integration; WebSocket-driven real-time market subscriptions; production deployment; comparative analytics & leaderboards; structured edge conditions; build-time Zod generation; prompt-evaluation regression harness.

- **Dynamic per-model pricing in `pricing.go`.** v0 hardcodes per-MTok input/output rates for the three models chosen in Phase 0; v1+ should fetch pricing from Anthropic's `/v1/models` endpoint with a 24h cache and last-known fallback. *Why defer:* pricing has been stable enough that hardcoding works for v0 demos, and the runtime already measures actual `usage.input_tokens` / `usage.output_tokens` from API responses — so tokenizer differences (notably Opus 4.7's new tokenizer producing up to ~35% more tokens per request than Opus 4.6) are already reflected in cost calculations. Only the per-token rate constants are stale-able, and the failure mode of stale constants is at most cents per run.

---

## Phase entries

(Append one entry per phase as STEPS.md proceeds. If a phase makes no architectural decisions, write "no deviations from CLAUDE.md.")

### Phase 0 — Pre-flight (2026-05-01)

- **Repo skeleton in place.** Created empty `api/` and `web/` directories (each with a `.gitkeep` so the directory is tracked before later phases populate it), placeholder `docker-compose.yml` (services added in Phase 1 + Phase 17), placeholder `Makefile` with only a `help` target (real targets added per phase), and `.env.example` listing every variable named in STEPS.md Phase 1.
- **`.env.example` documents the `NEXTAUTH_SECRET` UTF-8 byte-string convention** (no base64 decoding on either side; Node's `jsonwebtoken` and Go's `[]byte(os.Getenv(...))` both consume the secret as raw UTF-8 bytes — mismatched encoding produces silent "invalid signature" errors). Also documents `INTERNAL_API_URL` dev (`http://localhost:8080`) vs Compose (`http://api:8080`) values.
- **Anthropic model identifiers chosen — use whatever Anthropic publishes as the canonical "Claude API ID" for each model.** Verified against `https://docs.anthropic.com/en/docs/about-claude/models/overview` on 2026-05-01:

  | Slot | Model | Chosen string | Pricing (per MTok) |
  |---|---|---|---|
  | Default / balanced | Sonnet 4.6 | `claude-sonnet-4-6` | $3 in / $15 out |
  | Cheap | Haiku 4.5 | `claude-haiku-4-5-20251001` | $1 in / $5 out |
  | Premium | Opus 4.7 | `claude-opus-4-7` | $5 in / $25 out |

  Opus 4.7 and Sonnet 4.6 do not currently have a separate dated snapshot form — Anthropic publishes the alias as the canonical API ID. Haiku 4.5's canonical API ID is the dated form `claude-haiku-4-5-20251001` (alias `claude-haiku-4-5` also resolves, but the published canonical is the dated string). Using each model's published canonical ID gives us a deterministic snapshot for Haiku (good for prompt-reliability calibration in Phase 14.5) without inventing dated strings for Opus/Sonnet that don't exist yet.

  *Why:* the seed JSON (Phase 14), the `agent_templates.model` default in the schema (`'claude-sonnet-4-6'`, already in DATABASE_SCHEMA.md), and `pricing.go` (Phase 4) must all agree. Recording the chosen strings now, before any of those files are written, prevents a three-way rename later.

  *How to apply:* the `pricing.go` table keys, the agent seed JSON `model` fields, the `agent_templates.model` migration default, and the model dropdown options on the agent builder UI (Phase 11) must all use exactly these three strings. If Anthropic publishes a dated snapshot for Sonnet 4.6 or Opus 4.7 later, we evaluate whether to pin and update this entry — do not silently change the strings.

- **Opus 4.7 tokenizer FYI.** Opus 4.7 ships a new tokenizer; for the same input it can emit up to ~35% more tokens than Opus 4.6, which means effective cost per request can be up to ~35% higher than Opus 4.6 even at identical headline pricing ($5/$25 per MTok). This is automatic and correct in our metering — `pricing.go` multiplies the rate by the actual `usage.input_tokens`/`usage.output_tokens` returned by the API — but worth flagging so future-Alan doesn't get confused by Opus runs costing more than back-of-envelope predicted. The agent builder UI surfaces this inline when Opus 4.7 is selected (STEPS.md Phase 11).

### Phase 1 — Database, migrations, type registry seed (2026-05-01)

- **No deviations from CLAUDE.md / DATABASE_SCHEMA.md / TYPES.md.** Every table from DATABASE_SCHEMA.md is in `000001_init.up.sql` verbatim with the same column types, defaults, FK behavior, and indexes. The 6 core type schemas in `000002_seed_core_types.up.sql` match TYPES.md byte-for-byte (dollar-quoted JSON literals — no escaping changes the schema content). `additionalProperties: false` is present on every core schema, including the nested headlines item object in `news_digest.v1` (verified via `psql` after migration).
- **`pgcrypto` extension enabled in 000001_init.up.sql.** Postgres 16 ships `gen_random_uuid()` in core, but `CREATE EXTENSION IF NOT EXISTS pgcrypto;` is harmless and works on older managed hosts. *Why:* belt-and-suspenders for production hosting we haven't picked yet. *How to apply:* if a future migration uses `pgcrypto`-only functions (e.g., `digest()`), no extra setup required.
- **`paper_trades` FKs intentionally have no `ON DELETE CASCADE`** (matches DATABASE_SCHEMA.md verbatim). *Why:* paper trades are audit records; they should not silently vanish if a parent run row is deleted. v0 never deletes runs anyway, so RESTRICT is harmless and forward-compatible. *How to apply:* if v1+ ever needs to delete runs, write an explicit cleanup that handles the paper_trades rows first; do not retrofit CASCADE on this FK.
- **`migrate-down` defaults to `down -all`.** Reverts every applied migration without an interactive prompt. *Why:* STEPS.md Phase 1 verification expects `make migrate-down` to "cleanly revert"; the default `migrate down` (no args) prompts for confirmation, which breaks scripted use. *How to apply:* if a single-step rollback is needed during dev, run `migrate -path api/migrations -database "$DATABASE_URL" down 1` directly. The Makefile help mentions this.
- **Local dev convention: stop `maestro-postgres-1` while running Sibyl Hub's postgres on host port 5432.** Both projects bind 5432 by default; running them concurrently isn't supported on this machine. *Why:* Sibyl Hub is the Maestro successor (Alan's confirmation, 2026-05-01) — they aren't expected to run side-by-side, and keeping the standard `localhost:5432` convention in `.env.example` is worth more than the ability to dual-boot. *How to apply:* `docker stop maestro-postgres-1` before `docker compose up -d postgres` here; `docker start maestro-postgres-1` later if needed. Not a project-config decision — only flagged so future-me doesn't waste time re-deciding the port mapping.
- **Migration verification (recorded for reproducibility):** `docker compose up -d postgres` healthy in <2s; `make migrate-up` applies 3 migrations cleanly against the `sibyl_hub` database; `psql \dt` shows 12 expected tables + `schema_migrations` and **no `sessions` table**; `SELECT name, version FROM type_definitions ORDER BY name` returns the 6 core types; `SELECT key, value FROM system_config` returns `kill_switch | false`; `make migrate-down` reverts to a clean DB (only `schema_migrations` left); `make migrate-up` re-seeds back to the verified state. Initial verification ran against the pre-rename `oracle_garden` DB; the rename was applied to all migration files, .env.example (`postgres://sibyl:sibyl@localhost:5432/sibyl_hub`), docker-compose.yml (container `sibyl-hub-postgres`, role `sibyl`), and the run was repeated against the renamed DB before commit.

---

## Demo agents

Custom agents Alan builds during demos or interview prep. Captured as full prompts so they're not lost between sessions.

### Devil's Advocate

*(Append the final prompt as a code block under this heading once iterated during Phase 11 or interview prep. Include date and a short note on what the prompt is good at.)*
