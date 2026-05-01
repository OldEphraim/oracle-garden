# TYPES.md — Oracle Garden Type System

Companion to CLAUDE.md. Source of truth for all type schemas, validation rules, and runtime semantics around typed agent I/O.

---

## Concept

Every agent declares an **output type** (exactly one) and a set of **accepted input types** (zero or more). When two nodes are connected by an edge, the producer's `output_type` must be present in the consumer's `input_types`. The engine validates this at workflow-save time and again at runtime.

`input_types` is an **"accepts"** set, not a "requires" set. A node may declare `['observation.v1', 'news_digest.v1', 'risk_assessment.v1']` and at runtime only see `observation.v1` + `news_digest.v1` on its first iteration, then all three on iteration 2 (because a loop edge from a Risk Assessor fires). The agent's system prompt is responsible for handling the cases where some declared inputs are absent.

Workflow save-time validation is in two parts:
- **Hard error:** an edge whose producer type is not in the consumer's `input_types`.
- **Soft warning:** a declared input type with no producer anywhere upstream. Could be intentional (loop-only inputs, optional inputs from forks), so we warn rather than block.

For the soft-warning check, "upstream" is the **transitive closure of all incoming edges, regardless of cycle position.** An edge from X to Y makes X upstream of Y for this purpose, even when the edge is part of a loop. This means Thesis Builder declaring `risk_assessment.v1` as an accepted input does not warn in the happy path, because Risk Assessor has an edge into Thesis Builder (the rejection loop edge).

Type identifiers are strings of the form `<name>.<version>` — e.g., `thesis.v1`. Versions are immutable. To evolve a schema, register `thesis.v2`. Downstream agents that wish to accept both versions list both explicitly.

JSON Schema (Draft 2020-12) is the source of truth. The frontend uses **AJV** at runtime to validate against schemas fetched from `/api/types`; the backend uses `santhosh-tekuri/jsonschema/v6`. We do not generate Zod from schemas at build time.

**All v0 core types set `additionalProperties: false`** — strict validation. A misnamed field in agent output is exactly the kind of bug that hides when extras are tolerated, and the cost of writing an extra word in a schema is much smaller than the cost of debugging a silently-dropped field. User-registered custom types should follow the same convention.

---

## Multi-input merge

When a node has multiple incoming edges (fan-in), the engine merges upstream outputs into a single object keyed by each upstream node's `node_key`:

```json
{
  "watcher": { /* observation.v1 payload */ },
  "scout":   { /* news_digest.v1 payload */ },
  "risk":    { /* risk_assessment.v1 payload, present only on iteration ≥ 2 */ }
}
```

The system prompt for any multi-input agent must reference these keys explicitly. The engine documents the input shape in a prompt preamble it injects automatically: a list of which upstream node_keys are present in this firing, with their declared output types. Agents should treat absent keys as optional.

---

## Loop semantics

The engine treats loops as a natural extension of fan-in:

- A node N fires its **first** iteration when, per its `join_strategy`, all required upstreams have produced ≥1 output.
- For subsequent iterations, N fires whenever a *new* output arrives at any upstream and the join condition remains satisfied.
- The merged input always contains the **latest** output from each upstream node, regardless of which iteration that output came from.

For the happy-path workflow:
- Iteration 1 of Thesis Builder: triggered when Watcher and Scout have both produced output. Merged input = `{watcher: <obs>, scout: <news>}`.
- If Risk Assessor rejects on iteration 1, the loop edge fires.
- Iteration 2 of Thesis Builder: triggered by the new Risk output. Merged input = `{watcher: <obs from iter 1>, scout: <news from iter 1>, risk: <rejection>}`.
- Thesis Builder's system prompt says: "If a `risk` key is present, you are being asked to revise your prior thesis in light of the risk assessment's concerns."

This avoids SCC analysis and keeps the engine logic uniform.

---

## Core types (seeded into `type_definitions` with `is_core = true`)

### `market_target.v1`
Entry input from the engine to the first agent of any workflow.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["market_slug"],
  "properties": {
    "market_slug": { "type": "string" },
    "user_intent": {
      "type": "string",
      "description": "Optional free-text user goal — e.g., 'evaluate fade opportunity'"
    }
  },
  "additionalProperties": false
}
```

### `observation.v1`
Output of the Market Watcher.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": [
    "market_slug", "condition_id", "question",
    "current_price_yes", "current_price_no",
    "volume_24h_usd", "liquidity_usd",
    "time_to_resolution_hours",
    "yes_token_id", "no_token_id"
  ],
  "properties": {
    "market_slug":               { "type": "string" },
    "condition_id":              { "type": "string" },
    "question":                  { "type": "string" },
    "current_price_yes":         { "type": "number", "minimum": 0, "maximum": 1 },
    "current_price_no":          { "type": "number", "minimum": 0, "maximum": 1 },
    "volume_24h_usd":            { "type": "number", "minimum": 0 },
    "liquidity_usd":             { "type": "number", "minimum": 0 },
    "time_to_resolution_hours":  { "type": "number" },
    "recent_price_change_24h_pct": { "type": "number" },
    "yes_token_id":              { "type": "string" },
    "no_token_id":               { "type": "string" }
  },
  "additionalProperties": false
}
```

### `news_digest.v1`
Output of the News Scout.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["market_slug", "headlines", "sentiment_delta", "confidence"],
  "properties": {
    "market_slug": { "type": "string" },
    "headlines": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["title", "summary", "source"],
        "properties": {
          "title":        { "type": "string" },
          "summary":      { "type": "string" },
          "source":       { "type": "string" },
          "url":          { "type": "string" },
          "published_at": { "type": "string" }
        },
        "additionalProperties": false
      }
    },
    "sentiment_delta": {
      "type": "number", "minimum": -1, "maximum": 1,
      "description": "Negative = bearish for YES outcome; positive = bullish for YES outcome"
    },
    "confidence": { "type": "number", "minimum": 0, "maximum": 1 }
  },
  "additionalProperties": false
}
```

### `thesis.v1`
Output of Thesis Builder, Devil's Advocate, and any user-built thesis-revising agent.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["market_slug", "direction", "confidence", "reasoning"],
  "properties": {
    "market_slug": { "type": "string" },
    "direction":   { "type": "string", "enum": ["YES", "NO", "ABSTAIN"] },
    "confidence":  { "type": "number", "minimum": 0, "maximum": 1 },
    "reasoning":   { "type": "string" },
    "evidence":    { "type": "array", "items": { "type": "string" } }
  },
  "additionalProperties": false
}
```

### `risk_assessment.v1`
Output of the Risk Assessor. **The only type whose output legitimately matches `approved` / `rejected` edge conditions** (see CLAUDE.md's edge condition rules).

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": ["approved", "max_size_usd", "reasoning"],
  "properties": {
    "approved":     { "type": "boolean" },
    "max_size_usd": { "type": "number", "minimum": 0 },
    "reasoning":    { "type": "string" },
    "concerns":     { "type": "array", "items": { "type": "string" } }
  },
  "additionalProperties": false
}
```

### `trading_decision.v1`
Output of the Paper Executor. The terminal type — every workflow must end at a node whose `output_type = trading_decision.v1`.

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "required": [
    "market_slug", "side", "size_usd",
    "executed_price", "paper", "reasoning"
  ],
  "properties": {
    "market_slug":    { "type": "string" },
    "condition_id":   { "type": "string" },
    "token_id":       { "type": "string" },
    "side":           { "type": "string", "enum": ["YES", "NO", "ABSTAIN"] },
    "size_usd":       { "type": "number", "minimum": 0 },
    "executed_price": { "type": "number", "minimum": 0, "maximum": 1 },
    "executed_at":    { "type": "string" },
    "paper":          { "type": "boolean", "const": true },
    "reasoning":      { "type": "string" }
  },
  "additionalProperties": false
}
```

`side: "ABSTAIN"` with `size_usd: 0` is the canonical "no trade" decision (used when the Risk Assessor rejected and the loop ran out of iterations, or the Thesis was ABSTAIN).

---

## Validation contract

### Workflow save time
1. For each edge: producer's `output_type` must be in consumer's `input_types`. Hard error if not.
2. For each declared `input_type` of every node: warn if no upstream produces it (allows for legitimate optional/loop inputs).
3. For each edge with `condition` in `{approved, rejected}`: the source node's `output_type` must equal `risk_assessment.v1`. Hard error if not.
4. The workflow must have at least one node whose `output_type = trading_decision.v1` and which has no outgoing edges (the terminal node).

### Run time
- Each agent step's raw response is parsed as JSON and validated against the registered schema for the agent's `output_type`.
- On parse or validation failure, the engine retries the agent **once** with the validation error appended as a system note. Two failures total → step fails → run fails.
- No silent type coercion. If a number arrives as a string, it's a validation failure.