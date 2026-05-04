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
	// Tools is reserved for Phase 5 (tool_use blocks). v0 ignores it.
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

// Runtime composes the Anthropic client with the type registry. Build one at
// startup and reuse it.
type Runtime struct {
	anthropic *AnthropicClient
	types     *types.Registry
	logger    *slog.Logger
}

// NewRuntime wires together the dependencies. Logger may be nil (defaults
// to slog.Default()).
func NewRuntime(client *AnthropicClient, registry *types.Registry, logger *slog.Logger) *Runtime {
	if client == nil {
		panic("runtime: NewRuntime: nil AnthropicClient")
	}
	if registry == nil {
		panic("runtime: NewRuntime: nil types.Registry")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Runtime{anthropic: client, types: registry, logger: logger}
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
			return finishResult(result, agent.Model, start), fmt.Errorf("runtime: Invoke: anthropic: %w", err)
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
			return finishResult(result, agent.Model, start), fmt.Errorf("runtime: validate: %w", verr)
		}
		if len(errs) == 0 {
			result.Output = parsed
			return finishResult(result, agent.Model, start), nil
		}
		// Validation errors — fold into a compact retry note.
		lastRawText = text
		lastErrSummary = formatValidationErrors(errs)
		r.logger.Info("runtime: validation failed, will retry",
			"agent", agent.Name, "attempt", attempt,
			"first_error", firstString(errs))
	}

	// Both attempts failed.
	return finishResult(result, agent.Model, start),
		fmt.Errorf("runtime: agent %q failed validation after %d attempts: %s",
			agent.Name, maxAttempts, lastErrSummary)
}

// generateOnce is the inner per-attempt API call. v0 makes a single
// Messages.New call. Phase 5 will replace the body of this function with a
// tool-dispatch loop (call → if tool_use blocks: dispatch, append tool_results,
// call again; cap at 6 rounds; final assistant text is what we return).
// The validation+retry loop in Invoke stays untouched.
func (r *Runtime) generateOnce(
	ctx context.Context,
	agent Agent,
	systemPrompt, userMessage string,
) (text string, usage Usage, model string, stopReason string, err error) {
	temperature := float64(agent.Temperature)
	resp, err := r.anthropic.Messages(ctx, MessagesRequest{
		Model:       agent.Model,
		MaxTokens:   agent.MaxTokens,
		Temperature: &temperature,
		System:      systemPrompt,
		Messages: []Message{
			{Role: "user", Content: userMessage},
		},
	})
	if err != nil {
		return "", Usage{}, "", "", err
	}
	return extractText(resp), resp.Usage, resp.Model, resp.StopReason, nil
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

// stripJSONFences trims surrounding markdown code fences that some models
// add despite explicit instructions. Handles ```json...```, ```...```, and
// stray leading/trailing whitespace. If the text isn't fence-wrapped, it's
// returned unchanged.
func stripJSONFences(s string) string {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "```") {
		// Drop the opening fence line.
		if i := strings.Index(t, "\n"); i >= 0 {
			t = t[i+1:]
		} else {
			t = strings.TrimPrefix(t, "```")
		}
	}
	if strings.HasSuffix(t, "```") {
		t = strings.TrimSuffix(t, "```")
	}
	return strings.TrimSpace(t)
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
func finishResult(r *InvokeResult, model string, start time.Time) *InvokeResult {
	r.LatencyMS = int(time.Since(start).Milliseconds())
	r.CostUSD = billing.CostUSD(model, r.PromptTokens, r.CompletionTokens)
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
