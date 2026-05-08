// Package runtime is the agent runtime. client.go is the hand-rolled
// Anthropic Messages API client; agent.go is the per-step Invoke flow that
// composes the prompt, calls this client, and validates the structured
// output.
//
// Phase 4 used a string-typed Message.Content (text-only). Phase 5 widens
// Content to []ContentBlock so the conversation can carry tool_use and
// tool_result blocks across rounds — this is what enables agent tool use
// without rewriting the message model.
//
// Why hand-rolled instead of `anthropic-sdk-go`: see DECISION_LOG.md
// Phase 4. Short version — our needs (POST /v1/messages with text + tool
// blocks, no streaming, no batches, no vision) are a small slice of the
// SDK's surface, and managing structs we own is simpler than tracking
// SDK pins.
package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

const (
	anthropicBaseURL    = "https://api.anthropic.com"
	anthropicAPIVersion = "2023-06-01"
	defaultHTTPTimeout  = 120 * time.Second // Anthropic requests can take ~30s on long completions; the per-step engine cap is 90s.
)

// AnthropicClient wraps POST /v1/messages. Safe for concurrent use.
type AnthropicClient struct {
	apiKey     string
	baseURL    string
	apiVersion string
	httpClient *http.Client
	logger     *slog.Logger
}

// AnthropicOptions configures NewAnthropicClient. Zero values fall back to
// sensible defaults; pass BaseURL to point tests at httptest servers.
type AnthropicOptions struct {
	APIKey     string       // required
	BaseURL    string       // default: https://api.anthropic.com
	APIVersion string       // default: 2023-06-01
	HTTPClient *http.Client // default: &http.Client{Timeout: 120s}
	Logger     *slog.Logger // default: slog.Default()
}

func NewAnthropicClient(opts AnthropicOptions) (*AnthropicClient, error) {
	if opts.APIKey == "" {
		return nil, fmt.Errorf("anthropic: APIKey is required")
	}
	c := &AnthropicClient{
		apiKey:     opts.APIKey,
		baseURL:    opts.BaseURL,
		apiVersion: opts.APIVersion,
		httpClient: opts.HTTPClient,
		logger:     opts.Logger,
	}
	if c.baseURL == "" {
		c.baseURL = anthropicBaseURL
	}
	if c.apiVersion == "" {
		c.apiVersion = anthropicAPIVersion
	}
	if c.httpClient == nil {
		c.httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if c.logger == nil {
		c.logger = slog.Default()
	}
	return c, nil
}

// MessagesRequest mirrors the request body of POST /v1/messages.
type MessagesRequest struct {
	Model       string           `json:"model"`
	MaxTokens   int              `json:"max_tokens"`
	Temperature *float64         `json:"temperature,omitempty"`
	System      string           `json:"system,omitempty"`
	Messages    []Message        `json:"messages"`
	Tools       []ToolDefinition `json:"tools,omitempty"` // empty for non-tool-using agents
}

// Message is one turn in the conversation. Content is always an array of
// blocks (the API accepts both a string and an array; we standardize on the
// array form so v0 and v1+ paths share one shape).
type Message struct {
	Role    string         `json:"role"` // "user" | "assistant"
	Content []ContentBlock `json:"content"`
}

// ContentBlock represents one entry in a message's `content` array. Anthropic
// uses different keys per block type; we model them all on one struct with
// `omitempty` everywhere so the on-the-wire JSON only carries the fields
// relevant to each Type.
//
// Block types we use in v0 + v1:
//
//   - "text"        — plain assistant or user text
//     uses: Text
//   - "tool_use"    — assistant requests a tool invocation
//     uses: ID, Name, Input
//   - "tool_result" — caller (us) returns the tool's result to the model
//     uses: ToolUseID, ResultText, IsError
//
// (Server-side tools like web_search produce additional block types at
// runtime — `web_search_tool_result`, `server_tool_use` — that we surface
// transparently; the runtime doesn't dispatch them locally.)
type ContentBlock struct {
	Type string `json:"type"`

	// type=text
	Text string `json:"text,omitempty"`

	// type=tool_use (assistant emits these) AND type=server_tool_use
	// (Anthropic emits these for built-in tools like web_search)
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// type=tool_result (we send these back to the model). Anthropic also
	// emits server-side tool_result variants like `web_search_tool_result`
	// that share this field. The wire payload may be a string (our local-
	// tool case) or an array of content blocks (web_search_tool_result),
	// so we model it as a raw JSON message — the runtime's text extractor
	// ignores these block types entirely.
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// TextBlock is a constructor for the common single-text-block case.
func TextBlock(s string) ContentBlock {
	return ContentBlock{Type: "text", Text: s}
}

// ToolResultBlock builds the user-side reply to an assistant's tool_use
// block. resultText should be the JSON-stringified output (or an error
// message when isError is true). We JSON-encode it here so it serializes
// as a string in the wire `content` field (Anthropic accepts both string
// and array forms; we always send the string form for local-tool results).
func ToolResultBlock(toolUseID, resultText string, isError bool) ContentBlock {
	encoded, _ := json.Marshal(resultText)
	return ContentBlock{
		Type:      "tool_result",
		ToolUseID: toolUseID,
		Content:   encoded,
		IsError:   isError,
	}
}

// ToolDefinition is one element of MessagesRequest.Tools. Two flavors:
//
//   - Local tool: Type empty; Name + InputSchema describe a function the
//     client (us) implements. Anthropic returns tool_use blocks asking us
//     to call it.
//   - Server-side tool: Type set (e.g. "web_search_20250305"); Anthropic
//     handles execution itself and the response carries the result inline.
//     Name is still required (e.g. "web_search").
type ToolDefinition struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	// Server-side-tool-specific knobs (e.g. web_search MaxUses) can be
	// added here as omitempty fields when we adopt them.
	MaxUses int `json:"max_uses,omitempty"`
}

// MessagesResponse mirrors the response body.
type MessagesResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// Usage carries the token counts. Messages API uses input_tokens / output_tokens
// (NOT prompt_tokens / completion_tokens — that naming is in older surfaces).
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnthropicError mirrors the error envelope returned on non-2xx responses.
type AnthropicError struct {
	StatusCode int
	Type       string `json:"type"`
	Err        struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func (e *AnthropicError) Error() string {
	return fmt.Sprintf("anthropic: HTTP %d (%s): %s", e.StatusCode, e.Err.Type, e.Err.Message)
}

// Messages issues POST /v1/messages and returns the parsed response.
// Network/parse errors are returned directly; HTTP non-2xx are wrapped in
// *AnthropicError.
func (c *AnthropicClient) Messages(ctx context.Context, req MessagesRequest) (*MessagesResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("anthropic: marshal: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("anthropic: build request: %w", err)
	}
	httpReq.Header.Set("content-type", "application/json")
	httpReq.Header.Set("x-api-key", c.apiKey)
	httpReq.Header.Set("anthropic-version", c.apiVersion)

	start := time.Now()
	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("anthropic: do: %w", err)
	}
	defer httpResp.Body.Close()

	respBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: read body: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		ae := &AnthropicError{StatusCode: httpResp.StatusCode}
		if jerr := json.Unmarshal(respBody, ae); jerr != nil {
			ae.Err.Message = string(respBody)
		}
		return nil, ae
	}

	var out MessagesResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w (body=%s)", err, truncate(string(respBody), 256))
	}
	c.logger.Debug("anthropic messages",
		"model", out.Model,
		"input_tokens", out.Usage.InputTokens,
		"output_tokens", out.Usage.OutputTokens,
		"stop_reason", out.StopReason,
		"latency_ms", time.Since(start).Milliseconds(),
	)
	return &out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
