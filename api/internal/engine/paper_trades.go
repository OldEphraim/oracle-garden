package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/OldEphraim/sibyl-hub/api/internal/db"
	"github.com/OldEphraim/sibyl-hub/api/internal/db/repos"
)

// tradingDecision mirrors trading_decision.v1 (TYPES.md). Only the fields
// the engine reads at terminal-step finalization. Nullable JSON fields
// (condition_id, token_id) are pointers so we can distinguish "absent" from
// "empty string" — the schema lets ABSTAIN decisions omit those.
type tradingDecision struct {
	MarketSlug    string   `json:"market_slug"`
	ConditionID   *string  `json:"condition_id"`
	TokenID       *string  `json:"token_id"`
	Side          string   `json:"side"`
	SizeUSD       float64  `json:"size_usd"`
	ExecutedPrice float64  `json:"executed_price"`
	Reasoning     *string  `json:"reasoning"`
	Paper         bool     `json:"paper"`
}

// recordPaperTrade reads the validated trading_decision.v1 payload from a
// terminal node's output and inserts a paper_trades row. The mapping is
// per CLAUDE.md "Paper Trades" + AGENT_TEMPLATES.md "Engine-side post-processing":
//
//	side  ∈ {YES, NO} → status='open',     size_usd from decision
//	side  = ABSTAIN   → status='abstained', size_usd = 0
//
// In both branches entry_price = decision.executed_price (an executable
// price for 'open', a midpoint snapshot for 'abstained').
//
// Best-effort condition_id / token_id: the trading_decision.v1 schema
// allows these to be omitted; the paper_trades table requires non-null
// strings. We default to "" so the row inserts; downstream consumers
// (the analytics layer, when v1 lands) can filter by status='abstained'
// before reading these fields.
func recordPaperTrade(
	ctx context.Context,
	q db.Querier,
	repo *repos.PaperTradesRepo,
	runID, userID uuid.UUID,
	marketQuestion string, // from the entry market_target's question, captured at terminal time
	output json.RawMessage,
) (*repos.PaperTrade, error) {
	var d tradingDecision
	if err := json.Unmarshal(output, &d); err != nil {
		return nil, fmt.Errorf("engine: recordPaperTrade: parse decision: %w", err)
	}

	side := d.Side
	status := "open"
	size := d.SizeUSD
	if side == "ABSTAIN" {
		status = "abstained"
		size = 0
	}

	conditionID := ""
	if d.ConditionID != nil {
		conditionID = *d.ConditionID
	}
	tokenID := ""
	if d.TokenID != nil {
		tokenID = *d.TokenID
	}

	return repo.Create(ctx, q, repos.CreatePaperTradeParams{
		WorkflowRunID:  runID,
		UserID:         userID,
		MarketSlug:     d.MarketSlug,
		ConditionID:    conditionID,
		TokenID:        tokenID,
		MarketQuestion: marketQuestion,
		Side:           side,
		SizeUSD:        size,
		EntryPrice:     d.ExecutedPrice,
		Reasoning:      d.Reasoning,
		Status:         status,
	})
}
