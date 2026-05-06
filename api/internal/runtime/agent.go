package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/OldEphraim/sibyl-hub/api/internal/billing"
	"github.com/OldEphraim/sibyl-hub/api/internal/types"
)

// Agent is the per-call view the runtime needs of an agent_template.
// Phase 8's handlers will populate this from a *repos.AgentTemplate; the
// runtime stays decoupled from the DB layer.
type Agent struct {
	Name         string  // for logs only
	Model        string  // chosen Anthropic Claude API ID
	Temperature  float32 // 0..1
	MaxTokens    int
	SystemPrompt string
	OutputType   string // typeID, e.g. "thesis.v1" — must be registered

	// Tools is the list of internal tool names this agent may call (e.g.
	// ["polymarket.gamma_get_market", "web_search"]). The runtime resolves
	// each name through its ToolRegistry; unknown names error at Invoke.
	// v0: agents pick from the fixed registered set; user-built tools are
	// a v2 concern.
	Tools []string
}

// ToolDispatcher is the runtime's view of the tools registry. Defined as an
// interface so the runtime package doesn't import internal/tools (tools
// already imports runtime for the ToolDefinition type — keeps the dependency
// graph one-way).
type ToolDispatcher interface {
	// DefinitionsFor returns the wire-level tool definitions the runtime
	// passes to Anthropic in MessagesRequest.Tools.
	DefinitionsFor(internalNames []string) ([]ToolDefinition, error)

	// LookupByAPIName resolves the tool Anthropic named in a tool_use
	// block. The bool flags an unknown name (registry miss).
	LookupByAPIName(apiName string) (DispatchableTool, bool)
}

// DispatchableTool is the tool-side surface the runtime needs at dispatch
// time. Mirrors tools.Tool but lives here so the runtime stays decoupled.
type DispatchableTool interface {
	Name() string
	IsServerSide() bool
	Invoke(ctx context.Context, input json.RawMessage) (json.RawMessage, error)
}

// InvokeResult is the structured outcome of one agent step. On final failure,
// Output is nil but token/cost fields are still populated — already-paid-for
// tokens don't roll back, and the caller (engine, eventually) needs them to
// debit the user's daily cap.
type InvokeResult struct {
	Output           json.RawMessage // validated agent output, or nil on failure
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	LatencyMS        int
	Attempts         int    // 1 = first try succeeded; 2 = first try failed and retry succeeded; 2 with error = both failed
	Model            string // echoed from the response so callers can confirm
	StopReason       string
}

// Runtime composes the Anthropic client with the type registry and (when
// agents declare tools) a tool dispatcher. Build one at startup and reuse it.
type Runtime struct {
	anthropic *AnthropicClient
	types     *types.Registry
	tools     ToolDispatcher
	logger    *slog.Logger
}

// NewRuntime wires together the dependencies. tools may be nil for runtimes
// that only ever drive tool-less agents (e.g., the Phase 4 Thesis Builder
// driver in agentctl). Logger may be nil (defaults to slog.Default()).
func NewRuntime(client *AnthropicClient, registry *types.Registry, tools ToolDispatcher, logger *slog.Logger) *Runtime {
	if client == nil {
		panic("runtime: NewRuntime: nil AnthropicClient")
	}
	if registry == nil {
		panic("runtime: NewRuntime: nil types.Registry")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runtime{anthropic: client, types: registry, tools: tools, logger: logger}
}

// Invoke runs one agent step end-to-end:
//
//  1. Build system + user prompt from the agent + merged inputs + output schema.
//  2. Call the Anthropic Messages API (one round in v0; Phase 5 wraps this
//     in a tool-dispatch loop).
//  3. Strip ```json fences and JSON-parse the response.
//  4. Validate the parsed payload against agent.OutputType's schema.
//  5. On parse OR validation failure, retry ONCE with the failed text and
//     validation messages appended to the system prompt. 2 attempts total.
//
// Returns (*InvokeResult, error). On final failure, both are non-nil — the
// result holds accumulated cost, the error describes why validation failed.
//
// mergedInputs is the keyed-by-upstream-node map the engine builds during
// fan-in; the runtime never inspects per-key types, only the keys themselves
// (to write the multi-input preamble per CLAUDE.md "Agent Runtime").
func (r *Runtime) Invoke(ctx context.Context, agent Agent, mergedInputs map[string]json.RawMessage) (*InvokeResult, error) {
	start := time.Now()
	def, err := r.types.Get(agent.OutputType)
	if err != nil {
		return nil, fmt.Errorf("runtime: Invoke: %w", err)
	}
	schemaText := string(def.RawSchema)

	systemBase := buildSystemPrompt(agent, mergedInputs)
	userMessage := buildUserMessage(mergedInputs, schemaText)

	result := &InvokeResult{}
	var lastRawText, lastErrSummary string

	const maxAttempts = 2 // CLAUDE.md "validation retry cap" — 1 retry, 2 total attempts
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		systemPrompt := systemBase
		if attempt > 1 {
			systemPrompt = appendRetryNote(systemBase, lastRawText, lastErrSummary)
		}

		text, usage, model, stop, err := r.generateOnce(ctx, agent, systemPrompt, userMessage)
		result.PromptTokens += usage.InputTokens
		result.CompletionTokens += usage.OutputTokens
		result.Model = model
		result.StopReason = stop
		result.Attempts = attempt
		if err != nil {
			// Network / API error — don't retry on these in v0; surface
			// upstream. (Polymarket-style retry on 429 lives in client.go's
			// transport in Phase 5+ if we add it; for now we let the engine
			// timeout govern.)
			return finishResult(result, result.Model, start), fmt.Errorf("runtime: Invoke: anthropic: %w", err)
		}

		text = stripJSONFences(text)
		var parsed json.RawMessage
		if jerr := json.Unmarshal([]byte(text), &parsed); jerr != nil {
			lastRawText = text
			lastErrSummary = "previous response was not valid JSON: " + jerr.Error()
			r.logger.Info("runtime: parse failed, will retry",
				"agent", agent.Name, "attempt", attempt, "error", jerr.Error())
			continue
		}

		errs, verr := r.types.ValidateAgainst(parsed, agent.OutputType)
		if verr != nil {
			// ValidateAgainst returns a non-nil err only on infrastructure
			// problems (typeID unknown, JSON-not-object after parse). Don't
			// retry — this is our bug, not the model's.
			return finishResult(result, result.Model, start), fmt.Errorf("runtime: validate: %w", verr)
		}
		if len(errs) == 0 {
			result.Output = parsed
			return finishResult(result, result.Model, start), nil
		}
		// Validation errors — fold into a compact retry note.
		lastRawText = text
		lastErrSummary = formatValidationErrors(errs)
		r.logger.Info("runtime: validation failed, will retry",
			"agent", agent.Name, "attempt", attempt,
			"first_error", firstString(errs))
	}

	// Both attempts failed.
	return finishResult(result, result.Model, start),
		fmt.Errorf("runtime: agent %q failed validation after %d attempts: %s",
			agent.Name, maxAttempts, lastErrSummary)
}

// MaxToolRounds is the per-step cap on tool-dispatch rounds. CLAUDE.md:
// "Cap at 6 tool-use rounds per step." A round = one Anthropic API call;
// after the 6th call the runtime gives up if a tool_use is still pending.
const MaxToolRounds = 6

// generateOnce is the inner per-attempt API call wrapped by Invoke's
// validation + retry loop. v0 makes a single round when the agent declares
// no tools; v0+tools loops up to MaxToolRounds, dispatching local tool_use
// blocks and feeding tool_result blocks back in until the model returns a
// non-tool stop_reason (typically "end_turn") OR the cap is reached.
//
// Server-side tools (e.g. web_search) are handled by Anthropic itself —
// they appear in the response inline as `server_tool_use` /
// `web_search_tool_result` blocks alongside text, with stop_reason="end_turn".
// They never trigger a tool_use round here.
//
// Returns the final assistant text + accumulated usage across all rounds.
// On API error, accumulated usage up to the failure is returned with a
// non-nil err.
func (r *Runtime) generateOnce(
	ctx context.Context,
	agent Agent,
	systemPrompt, userMessage string,
) (text string, usage Usage, model string, stopReason string, err error) {
	// Resolve the agent's declared tool list to wire-level tool definitions.
	var toolDefs []ToolDefinition
	if len(agent.Tools) > 0 {
		if r.tools == nil {
			return "", Usage{}, "", "", fmt.Errorf("runtime: agent declares %d tool(s) but runtime has no ToolDispatcher", len(agent.Tools))
		}
		toolDefs, err = r.tools.DefinitionsFor(agent.Tools)
		if err != nil {
			return "", Usage{}, "", "", fmt.Errorf("runtime: resolve tools: %w", err)
		}
	}

	temperature := float64(agent.Temperature)
	messages := []Message{
		{Role: "user", Content: []ContentBlock{TextBlock(userMessage)}},
	}

	var totalUsage Usage
	var resp *MessagesResponse

	for round := 1; round <= MaxToolRounds; round++ {
		resp, err = r.anthropic.Messages(ctx, MessagesRequest{
			Model:       agent.Model,
			MaxTokens:   agent.MaxTokens,
			Temperature: &temperature,
			System:      systemPrompt,
			Messages:    messages,
			Tools:       toolDefs,
		})
		if err != nil {
			return "", totalUsage, "", "", err
		}
		totalUsage.InputTokens += resp.Usage.InputTokens
		totalUsage.OutputTokens += resp.Usage.OutputTokens

		// Find local tool_use blocks. Non-"tool_use" stop reasons OR a
		// missing tool_use block both mean we're at the final response.
		toolUseBlocks := collectToolUseBlocks(resp.Content)
		if resp.StopReason != "tool_use" || len(toolUseBlocks) == 0 {
			if resp.StopReason == "tool_use" && len(toolUseBlocks) == 0 {
				r.logger.Warn("runtime: stop_reason=tool_use with no tool_use blocks — treating as final",
					"agent", agent.Name, "round", round)
			}
			return extractText(resp), totalUsage, resp.Model, resp.StopReason, nil
		}

		// Cap reached on this round; no further dispatch is allowed.
		if round == MaxToolRounds {
			return "", totalUsage, resp.Model, resp.StopReason,
				fmt.Errorf("runtime: tool-use cap reached (%d rounds) without final text", MaxToolRounds)
		}

		// Dispatch each local tool_use block, build tool_result blocks.
		results := r.dispatchToolUseBlocks(ctx, agent.Name, toolUseBlocks, round)

		// Extend the conversation: assistant's full response + our tool results.
		messages = append(messages,
			Message{Role: "assistant", Content: resp.Content},
			Message{Role: "user", Content: results},
		)
	}

	// Defensive: the cap return above should fire first; this branch only
	// runs if MaxToolRounds is set to 0, which we don't permit.
	return "", totalUsage, "", "", fmt.Errorf("runtime: tool dispatch loop exited unexpectedly")
}

// dispatchToolUseBlocks invokes every local tool_use block and returns the
// matching tool_result blocks. Tool errors become tool_result blocks with
// is_error=true so the model can adapt; no error from this function ever
// propagates to the caller. Server-side tools requested as tool_use (a
// programmer bug — Anthropic should never produce that combination) are
// surfaced as is_error tool_results AND logged as ERROR.
func (r *Runtime) dispatchToolUseBlocks(
	ctx context.Context,
	agentName string,
	blocks []ContentBlock,
	round int,
) []ContentBlock {
	out := make([]ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		tool, ok := r.tools.LookupByAPIName(b.Name)
		if !ok {
			r.logger.Error("runtime: unknown tool requested by model",
				"agent", agentName, "round", round, "tool_api_name", b.Name)
			out = append(out, ToolResultBlock(b.ID,
				fmt.Sprintf("error: tool %q is not registered", b.Name), true))
			continue
		}
		if tool.IsServerSide() {
			r.logger.Error("runtime: server-side tool emitted as tool_use",
				"agent", agentName, "round", round, "tool", tool.Name())
			out = append(out, ToolResultBlock(b.ID,
				fmt.Sprintf("error: %q is a server-side tool and cannot be locally dispatched", tool.Name()), true))
			continue
		}
		r.logger.Info("runtime: dispatching tool",
			"agent", agentName, "round", round, "tool", tool.Name(),
			"input_bytes", len(b.Input))
		result, ierr := tool.Invoke(ctx, b.Input)
		if ierr != nil {
			r.logger.Info("runtime: tool error",
				"agent", agentName, "round", round, "tool", tool.Name(), "error", ierr.Error())
			out = append(out, ToolResultBlock(b.ID, ierr.Error(), true))
			continue
		}
		out = append(out, ToolResultBlock(b.ID, string(result), false))
	}
	return out
}

// collectToolUseBlocks returns the tool_use blocks from a content list.
// Order preserved so multiple tool_use blocks in one assistant turn dispatch
// in the order the model emitted them.
func collectToolUseBlocks(content []ContentBlock) []ContentBlock {
	var out []ContentBlock
	for _, b := range content {
		if b.Type == "tool_use" {
			out = append(out, b)
		}
	}
	return out
}

// extractText concatenates the `text` content blocks of a response. v0
// expects exactly one text block per call (no tool_use); Phase 5 will need
// to discriminate.
func extractText(resp *MessagesResponse) string {
	var sb strings.Builder
	for _, b := range resp.Content {
		if b.Type == "text" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

// buildSystemPrompt assembles the agent's system prompt + the structured-
// output preamble + (when multi-input) a description of the keys the merged
// input dict carries. Per CLAUDE.md "Agent Runtime", the literal schema
// goes in the USER message, not here — this prompt only contains
// instructions and the multi-input key list.
func buildSystemPrompt(agent Agent, mergedInputs map[string]json.RawMessage) string {
	var sb strings.Builder
	sb.WriteString(strings.TrimRight(agent.SystemPrompt, "\n"))
	sb.WriteString("\n\n")

	if len(mergedInputs) > 1 {
		keys := sortedKeys(mergedInputs)
		fmt.Fprintf(&sb,
			"You receive a JSON object keyed by upstream node. The keys present in this firing are: %s. "+
				"Treat any keys you expect but don't see as optional.\n\n",
			strings.Join(keys, ", "))
	}

	sb.WriteString("Respond with a single JSON object that conforms to the JSON Schema in the user message. ")
	sb.WriteString("Output ONLY that JSON object — no prose, no markdown, no code fences. ")
	sb.WriteString("Every required field must be present and every value must satisfy the listed constraints.")
	return sb.String()
}

// buildUserMessage carries the merged inputs and the literal schema. Per
// CLAUDE.md, the schema is sent in the user message so the structured-
// output rules are immediately adjacent to the data being described.
func buildUserMessage(mergedInputs map[string]json.RawMessage, schemaText string) string {
	body, _ := json.MarshalIndent(mergedInputs, "", "  ")
	if len(mergedInputs) == 0 {
		// Defensive: a workflow's first step still gets seeded with a
		// market_target.v1 by the engine, so this should never empty in
		// practice. Surface "{}" rather than panicking.
		body = []byte(`{}`)
	}
	return fmt.Sprintf(
		"Inputs:\n%s\n\nRespond ONLY with a JSON object matching this schema:\n%s",
		string(body),
		schemaText,
	)
}

// appendRetryNote constructs the system prompt for the second attempt. We
// include the failed raw text AND the validation messages so the model can
// see what it actually said and why it was rejected — re-sending the same
// prompt without this context just produces the same failure.
func appendRetryNote(systemBase, lastText, lastErrSummary string) string {
	const truncTo = 4000 // bound the failed text so a runaway response doesn't blow up the next prompt
	failed := lastText
	if len(failed) > truncTo {
		failed = failed[:truncTo] + "\n…(truncated)"
	}
	return systemBase + "\n\n" +
		"NOTE: Your previous response failed validation. You are being asked to try again.\n" +
		"Previous response:\n" + failed + "\n\n" +
		"Validation problem(s):\n" + lastErrSummary + "\n\n" +
		"Correct these issues and respond ONLY with a JSON object matching the schema. " +
		"Do not repeat the previous mistake."
}

// stripJSONFences trims surrounding markdown code fences AND any leading/
// trailing prose, returning the first balanced JSON object the response
// contains. Handles three messy real-world cases:
//
//   - ```json {...} ``` — markdown-fenced
//   - ``` {...} ```      — fence without language tag
//   - "Now I'll provide the observation. {...}" — prose preamble (some
//     models do this even after explicit "no prose" instructions)
//
// If no balanced object is found, returns the string with fences/whitespace
// stripped — the JSON parser will then fail with a useful error.
func stripJSONFences(s string) string {
	t := strings.TrimSpace(s)

	// Fence handling first.
	if strings.HasPrefix(t, "```") {
		if i := strings.Index(t, "\n"); i >= 0 {
			t = t[i+1:]
		} else {
			t = strings.TrimPrefix(t, "```")
		}
	}
	if strings.HasSuffix(t, "```") {
		t = strings.TrimSuffix(t, "```")
	}
	t = strings.TrimSpace(t)

	// If the text is already a valid object/array shape, return as-is.
	if strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		return t
	}

	// Scan for a balanced top-level JSON object embedded in prose.
	if obj := extractFirstJSONObject(t); obj != "" {
		return obj
	}
	return t
}

// extractFirstJSONObject walks s looking for the first balanced {...} block,
// tracking string state and escape sequences so braces inside JSON strings
// don't confuse the depth counter. Returns the matched substring (inclusive
// of the outer braces) or "" if no balanced object is found.
func extractFirstJSONObject(s string) string {
	start := strings.IndexByte(s, '{')
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escape := false
	for i := start; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if c == '\\' && inString {
			escape = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

// formatValidationErrors flattens registry errors into the bullet list used
// in the retry note. Caps at 5 errors so a payload with dozens doesn't
// produce a 50KB system prompt.
func formatValidationErrors(errs []types.ValidationError) string {
	const maxBullets = 5
	var sb strings.Builder
	for i, e := range errs {
		if i >= maxBullets {
			fmt.Fprintf(&sb, "  - …and %d more.\n", len(errs)-maxBullets)
			break
		}
		fmt.Fprintf(&sb, "  - %s\n", e)
	}
	return strings.TrimRight(sb.String(), "\n")
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func firstString(errs []types.ValidationError) string {
	if len(errs) == 0 {
		return ""
	}
	return errs[0].String()
}

// finishResult fills in the time-and-cost fields just before the result is
// handed back to the caller. Mutates and returns r so the call site stays
// terse.
//
// model is the identifier billing should use — pass result.Model (the form
// the API echoed back, which may be the dated snapshot even when the
// request used an alias). Falls back to a non-empty input if the response
// hasn't populated Model yet (e.g. transport error before any successful
// call).
func finishResult(r *InvokeResult, model string, start time.Time) *InvokeResult {
	r.LatencyMS = int(time.Since(start).Milliseconds())
	bill := model
	if bill == "" {
		bill = r.Model
	}
	r.CostUSD = billing.CostUSD(bill, r.PromptTokens, r.CompletionTokens)
	return r
}

// IsAnthropicError reports whether err originated from the Anthropic API
// non-2xx envelope. Useful for handlers that want to surface the upstream
// type/message verbatim.
func IsAnthropicError(err error) (*AnthropicError, bool) {
	var ae *AnthropicError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}
