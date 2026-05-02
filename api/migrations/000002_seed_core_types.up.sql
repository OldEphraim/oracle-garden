-- 000002_seed_core_types.up.sql
-- Seed the 6 core type_definitions from TYPES.md, verbatim.
-- All schemas use additionalProperties: false (including the nested headlines
-- item object in news_digest.v1).

INSERT INTO type_definitions (name, version, description, is_core, json_schema)
VALUES
  (
    'market_target',
    'v1',
    'Entry input from the engine to the first agent of any workflow.',
    TRUE,
    $json${
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
    }$json$::jsonb
  ),
  (
    'observation',
    'v1',
    'Output of the Market Watcher.',
    TRUE,
    $json${
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
    }$json$::jsonb
  ),
  (
    'news_digest',
    'v1',
    'Output of the News Scout.',
    TRUE,
    $json${
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
    }$json$::jsonb
  ),
  (
    'thesis',
    'v1',
    'Output of Thesis Builder, Devil''s Advocate, and any user-built thesis-revising agent.',
    TRUE,
    $json${
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
    }$json$::jsonb
  ),
  (
    'risk_assessment',
    'v1',
    'Output of the Risk Assessor. The only type whose output legitimately matches approved/rejected edge conditions.',
    TRUE,
    $json${
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
    }$json$::jsonb
  ),
  (
    'trading_decision',
    'v1',
    'Output of the Paper Executor. The terminal type — every workflow must end at a node whose output_type = trading_decision.v1.',
    TRUE,
    $json${
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
    }$json$::jsonb
  );
