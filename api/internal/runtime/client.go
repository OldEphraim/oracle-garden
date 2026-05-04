// Package runtime is the agent runtime. client.go is the hand-rolled
// Anthropic Messages API client; agent.go is the per-step Invoke flow that
// composes the prompt, calls this client, and validates the structured
// output. Phase 5 will extend the inner generation loop to handle tool_use
// blocks; the validation+retry wrapper stays untouched.
//
// Why hand-rolled instead of the official `anthropic-sdk-go`:
// see DECISION_LOG.md Phase 4. Short version — our needs are POST /v1/messages
// with text-in / text-out + Phase-5 tool_use blocks, and the SDK's surface
// area (streaming, batches, vision, files, MCP, …) is much larger than that.
// A ~150-line client is simpler to keep correct than tracking what the SDK
// pins.
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
	defaultHTTPTimeout  = 120 * time.Second // Anthropic requests can take ~30s on long completions; 90s is the per-step engine cap, but the HTTP client itself stays slack.
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
	APIKey     string        // required
	BaseURL    string        // default: https://api.anthropic.com
	APIVersion string        // default: 2023-06-01
	HTTPClient *http.Client  // default: &http.Client{Timeout: 120s}
	Logger     *slog.Logger  // default: slog.Default()
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

// MessagesRequest mirrors the request body of POST /v1/messages. Only fields
// we use in v0 are exposed; streaming, batching, vision attachments, and
// thinking tokens are not part of v0 (see CLAUDE.md "Out of Scope").
type MessagesRequest struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature *float64  `json:"temperature,omitempty"`
	System      string    `json:"system,omitempty"`
	Messages    []Message `json:"messages"`
}

// Message is one turn in the conversation. v0 keeps content as a string
// (text-only). Phase 5 will switch to []ContentBlock when tool_use lands.
type Message struct {
	Role    string `json:"role"`    // "user" | "assistant"
	Content string `json:"content"` // text-only in v0
}

// MessagesResponse mirrors the response body. Content is a slice of blocks
// even in v0 (the API always returns it that way), but for text-only calls
// we only ever look at content[0].text.
type MessagesResponse struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Role       string         `json:"role"`
	Model      string         `json:"model"`
	Content    []ContentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      Usage          `json:"usage"`
}

// ContentBlock is one entry in `content[]`. For Phase 4 we only encounter
// `type="text"`; Phase 5 adds `tool_use` and tool-result variants.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// Usage carries the token counts. Anthropic's Messages API uses these field
// names verbatim — input_tokens / output_tokens (some Anthropic surfaces
// use prompt_tokens / completion_tokens, but Messages API does not).
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// AnthropicError mirrors the error envelope returned on non-2xx responses.
// Useful for callers that want to branch on `error.type` (e.g. "rate_limit_error").
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
		// Best-effort error decode; if the body isn't JSON, surface the raw bytes.
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
