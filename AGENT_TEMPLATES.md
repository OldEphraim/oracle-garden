# AGENT_TEMPLATES.md — Oracle Garden Built-in Agent Templates

Companion to CLAUDE.md. Source of truth for the v0 system agent templates: their I/O contracts, tool requirements, and system prompt sketches. As Oracle Garden grows, new system templates are added here; user-built templates live in the `agent_templates` table with `is_system = false`.

This file pairs with TYPES.md (which specifies the schemas these agents emit and consume) and with CLAUDE.md's Workflow Execution Engine section (which specifies how these agents are wired together).

---

## Conventions

- All v0 system templates seed with `is_system = true`, `owner_id = NULL`, `visibility = 'private'`. Users cannot edit them; users can fork them and edit their forks.
- "System prompt sketch" entries are starting points. They will be iterated on during Phase 14 and during the Phase 14.5 reliability bar — the goal is ≥80% completion-with-paper-trade across diverse markets. When a sketch is revised, update the JSON seed file under `api/seed/agents/` AND this document AND log the change in DECISION_LOG.md.
- Every system prompt must end with explicit JSON-output instructions (the runtime appends a structured-output preamble automatically, but agents reinforce it). The seed JSON contains the full prompt; sketches here capture intent.
- Tool selection is deliberately minimal per agent. An agent that only needs to reason should declare no tools — it speeds runs and reduces failure surface.

---

## v0 System Agent Templates

All five are seeded into `agent_templates` at first migration via `make seed`. See TYPES.md for full I/O schemas.

### 1. Market Watcher

| Field | Value |
|---|---|
| Inputs | `market_target.v1` |
| Output | `observation.v1` |
| Tools | `polymarket.gamma_get_market`, `polymarket.clob_get_midpoint`, `polymarket.clob_get_prices_history` |

**System prompt sketch:** "You observe a Polymarket market and emit a structured observation. Resolve the market by slug, fetch current prices and 24h history, populate every required field of `observation.v1`. Do not interpret — only observe."

### 2. News Scout

| Field | Value |
|---|---|
| Inputs | `market_target.v1` and/or `observation.v1` (either or both accepted) |
| Output | `news_digest.v1` |
| Tools | `web_search` |

**System prompt sketch:** "You search for recent news relevant to a prediction market. Pull 3-7 high-quality recent headlines. Compute a `sentiment_delta` in [-1, 1] from the perspective of the YES outcome. Be conservative on `confidence` — only assign >0.7 if multiple credible sources align."

### 3. Thesis Builder

| Field | Value |
|---|---|
| Inputs | `observation.v1`, `news_digest.v1`, `risk_assessment.v1` (the third is optional, present only on loop iterations) |
| Output | `thesis.v1` |
| Tools | none |

**System prompt sketch:** "You write prediction-market theses. Given an observation (current price, liquidity, time to resolution) and a news digest (recent headlines, sentiment), produce a directional thesis (YES, NO, or ABSTAIN). Confidence must be calibrated — if your direction agrees with current price, your edge is small. ABSTAIN when the price already reflects the news. **If a `risk` field is present in your input, you are being asked to revise your prior thesis in light of the risk assessor's concerns.** Address each concern explicitly in your reasoning, and either lower confidence, change direction, or abstain accordingly."

### 4. Risk Assessor

| Field | Value |
|---|---|
| Inputs | `thesis.v1` |
| Output | `risk_assessment.v1` |
| Tools | none |

**System prompt sketch:** "You gate trade approval. Approve only if confidence > 0.6, time to resolution > 12 hours, and current price doesn't already exceed the implied probability matching the thesis confidence by more than 5%. Max size scales linearly with confidence: $10 at 0.6 → $100 at 0.95. List specific concerns. Output `approved: false` decisively when warranted — your output drives the workflow's branching, so be precise."

### 5. Paper Executor

| Field | Value |
|---|---|
| Inputs | `thesis.v1`, `risk_assessment.v1` |
| Output | `trading_decision.v1` |
| Tools | `polymarket.gamma_get_market`, `polymarket.clob_get_orderbook` |
| Side effect | The engine inserts a `paper_trades` row after this step's output validates. |

**System prompt sketch:** "You record paper trades. If the risk assessment did not approve OR the thesis was ABSTAIN, output a trading_decision with `side: 'ABSTAIN'`, `size_usd: 0`, `paper: true`, `executed_price` set to the current market midpoint (fetch the orderbook for either outcome to get this), and cite the rejection or abstention in `reasoning`. Otherwise: resolve the market via gamma_get_market to obtain `condition_id` and the appropriate `token_id` for the thesis direction (YES or NO), fetch the orderbook to determine an executable price, and record the trade. Always set `paper: true`."

The Paper Executor's tools include `polymarket.gamma_get_market` so it can resolve token IDs from the thesis's market_slug. (Token IDs are deliberately not threaded through `thesis.v1` or `risk_assessment.v1` — that would couple non-execution agents to execution-layer concerns.)

**Note on ABSTAIN cost.** Even on ABSTAIN, the executor fetches the orderbook (one tool round, ~one second) to get a midpoint for `executed_price`. This is intentional — the alternative is making `executed_price` optional in `trading_decision.v1`, which complicates downstream consumers. The tool round is cheap enough that v0 just pays it.

---

## Engine-side post-processing

When the Paper Executor's `agent_steps` row finalizes with valid output, the engine reads the `trading_decision.v1` payload and writes a `paper_trades` row using this mapping:

| `trading_decision.side` | `paper_trades.status` | `paper_trades.size_usd` | `paper_trades.entry_price` |
|---|---|---|---|
| `YES` or `NO` | `open` | `> 0` (from decision) | `executed_price` from decision |
| `ABSTAIN` | `abstained` | `0` | `executed_price` (midpoint at decision time, recorded for lineage) |

In v0 these rows never transition out of `open` or `abstained`. PnL tracking and resolution monitoring are v1+ features (see DATABASE_SCHEMA.md).

---

## Roadmap (informational)

The following agent templates are anticipated for future versions and listed here so the v0 type system stays compatible. They are NOT seeded in v0.

- **Devil's Advocate** (v0 demo agent — built by the user during the demo, not seeded). Inputs: `thesis.v1`, `news_digest.v1`. Output: `thesis.v1`. Tools: none. Re-examines a thesis against the news, looking for dissenting evidence; can lower confidence, flip direction, or pass through. **When Alan iterates on this prompt during Phase 11 or interview prep, append the final version as a code block in `DECISION_LOG.md` under a `## Demo agents` heading**, with a date and a short note on what the prompt is good at. Otherwise the prompt drifts away after the next Claude session.
- **Position Tracker** (v1+). Inputs: portfolio-state.v1 (new type) + observation.v1. Output: position-status.v1. Reads the user's open paper trades and emits status against current prices. Requires a periodic price-refresh job that's out of v0 scope.
- **Whale Watcher** (v1+). Inputs: `market_target.v1`. Output: `whale_signal.v1` (new type). Calls the Polymarket Data API for top-holder positions and large trades; emits a directional signal based on what large traders are doing. Adds a meaningful new data source.
- **Calibration Filter** (v1+). Inputs: `thesis.v1` + `observation.v1`. Output: `thesis.v1`. Dampens confidence when the thesis essentially agrees with the market price.
- **Cross-Market Correlator** (v2+). Multi-market input shape, requires a new type and orchestration support for parallel market_target arrays.
- **Sentiment Drift Detector** (v2+). Compares News Scout output across runs over time to detect changing sentiment ahead of price.

When adding a new system template:
1. Decide its input/output types. If new types are needed, add them to TYPES.md and seed them via a migration. Bump no existing version — register a new one.
2. Add the JSON seed file under `api/seed/agents/` and update the seeder's load order if seeding depends on referenced types.
3. Document here under a new section, with the same Inputs/Output/Tools/Side effect/System prompt sketch shape.
4. Add to the v0 demo flow and reliability bar only if it's intended to ship; otherwise mark as user-fork-only or new-template-only.
5. Update DECISION_LOG.md with rationale.