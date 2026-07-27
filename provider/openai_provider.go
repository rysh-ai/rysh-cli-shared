package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// OpenAIAgenticProvider implements AgenticProvider against any OpenAI-compatible
// Chat Completions endpoint — OpenAI itself, and crucially local Ollama
// (http://127.0.0.1:11434/v1), which powers rysh's air-gapped mode (design 002).
//
// It translates rysh's neutral ConversationTurn / ToolSpec shapes to the OpenAI
// dialect at the edge, so the agentic loop (orchestrator) stays provider-neutral
// with no refactor. SSE streaming lives in openai_streaming.go
// (CompleteWithToolsStream), which satisfies the same StreamingProvider
// interface the Claude provider implements.
type OpenAIAgenticProvider struct {
	apiKey    string
	apiURL    string // base, e.g. https://api.openai.com/v1 or http://127.0.0.1:11434/v1
	model     string
	maxTokens int
	name      string // "openai" | "ollama" (for attribution/pricing)
	client    *http.Client
}

// NewOpenAIAgenticProvider builds a provider. name distinguishes ollama vs
// openai for usage attribution; apiURL should include the /v1 suffix.
func NewOpenAIAgenticProvider(name, apiKey, apiURL, model string, maxTokens int) *OpenAIAgenticProvider {
	if apiURL == "" {
		apiURL = "https://api.openai.com/v1"
	}
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	if name == "" {
		name = "openai"
	}
	return &OpenAIAgenticProvider{
		apiKey: apiKey, apiURL: strings.TrimRight(apiURL, "/"), model: model,
		maxTokens: maxTokens, name: name,
		client: &http.Client{Timeout: 0},
	}
}

func (c *OpenAIAgenticProvider) Name() string  { return c.name }
func (c *OpenAIAgenticProvider) Model() string { return c.model }

// WithModelEffort returns a copy pinned to model (effort is a no-op for the
// OpenAI dialect). Satisfies the ModelEffortOverridable seam used by
// WithSessionDefaults so non-Claude providers slot into the same wrapper.
func (c *OpenAIAgenticProvider) WithModelEffort(model, _ string) AgenticProvider {
	cp := *c
	if model != "" {
		cp.model = model
	}
	return &cp
}

// WithMaxTokens returns a copy with the max_tokens cap applied
// (maxTokens <= 0 keeps the configured cap). Satisfies the
// MaxTokensOverridable seam behind ChatRequest.MaxTokens; the cap reaches
// the wire via buildRequest on both the one-shot and streaming paths.
func (c *OpenAIAgenticProvider) WithMaxTokens(maxTokens int) AgenticProvider {
	cp := *c
	if maxTokens > 0 {
		cp.maxTokens = maxTokens
	}
	return &cp
}

// Complete is the single-turn shim (no tools).
func (c *OpenAIAgenticProvider) Complete(ctx context.Context, prompt string) (string, error) {
	resp, err := c.CompleteWithTools(ctx, []ConversationTurn{{Role: "user", Content: prompt}}, nil, "")
	if err != nil {
		return "", err
	}
	var parts []string
	for _, tb := range resp.TextBlocks {
		parts = append(parts, tb.Text)
	}
	return strings.Join(parts, ""), nil
}

// ---- OpenAI wire shapes -----------------------------------------------------

type oaiFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
	Params      json.RawMessage `json:"parameters,omitempty"`
}

type oaiToolCall struct {
	ID       string      `json:"id"`
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    string        `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiTool struct {
	Type     string      `json:"type"`
	Function oaiFunction `json:"function"`
}

type oaiRequest struct {
	Model     string       `json:"model"`
	Messages  []oaiMessage `json:"messages"`
	Tools     []oaiTool    `json:"tools,omitempty"`
	MaxTokens int          `json:"max_tokens,omitempty"`
	// Streaming fields (openai_streaming.go). StreamOptions asks the server to
	// append a final usage chunk; only ever set alongside Stream=true.
	Stream        bool              `json:"stream,omitempty"`
	StreamOptions *oaiStreamOptions `json:"stream_options,omitempty"`
}

// oaiStreamOptions mirrors OpenAI's stream_options request block.
type oaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type oaiResponse struct {
	Choices []struct {
		Message      oaiMessage `json:"message"`
		FinishReason string     `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// buildRequest translates the neutral conversation/tool shapes into one
// Chat Completions request body. Shared by the non-streaming and streaming
// paths so the two stay byte-identical apart from the stream fields.
func (c *OpenAIAgenticProvider) buildRequest(conversation []ConversationTurn, tools []ToolSpec, systemPrompt string) oaiRequest {
	msgs := make([]oaiMessage, 0, len(conversation)+1)
	if systemPrompt != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: systemPrompt})
	}
	for _, t := range conversation {
		msgs = append(msgs, turnToOAI(t))
	}

	var oaiTools []oaiTool
	for _, ts := range tools {
		oaiTools = append(oaiTools, oaiTool{Type: "function", Function: oaiFunction{
			Name: ts.Name, Description: ts.Description, Params: ts.Parameters,
		}})
	}

	return oaiRequest{Model: c.model, Messages: msgs, Tools: oaiTools, MaxTokens: c.maxTokens}
}

// CompleteWithTools runs one Chat Completions turn with tool support.
func (c *OpenAIAgenticProvider) CompleteWithTools(ctx context.Context, conversation []ConversationTurn, tools []ToolSpec, systemPrompt string) (*AgenticResponse, error) {
	return c.doComplete(ctx, c.buildRequest(conversation, tools, systemPrompt))
}

// doComplete performs one non-streaming HTTP round trip for an assembled
// request body and parses the response. Shared transport for the shim path
// (CompleteWithTools) and the native ChatProvider path (Chat).
func (c *OpenAIAgenticProvider) doComplete(ctx context.Context, reqBody oaiRequest) (*AgenticResponse, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%s: request: %w", c.name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: status %d: %s", c.name, resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var out oaiResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", c.name, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %s", c.name, out.Error.Message)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("%s: response had no choices", c.name)
	}

	choice := out.Choices[0]
	ar := &AgenticResponse{
		StopReason: mapFinishReason(choice.FinishReason),
		Usage: Usage{
			InputTokens:  out.Usage.PromptTokens,
			OutputTokens: out.Usage.CompletionTokens,
		},
	}
	if choice.Message.Content != "" {
		ar.TextBlocks = append(ar.TextBlocks, TextBlock{Text: choice.Message.Content})
	}
	for _, tc := range choice.Message.ToolCalls {
		args := tc.Function.Arguments
		if args == "" {
			args = "{}"
		}
		ar.ToolCalls = append(ar.ToolCalls, ToolCallRequest{
			ID: tc.ID, Name: tc.Function.Name, Input: json.RawMessage(args),
		})
	}
	return ar, nil
}

// turnToOAI converts a neutral ConversationTurn to an OpenAI message.
func turnToOAI(t ConversationTurn) oaiMessage {
	switch t.Role {
	case "tool":
		return oaiMessage{Role: "tool", ToolCallID: t.ToolCallID, Content: t.Content}
	case "assistant":
		m := oaiMessage{Role: "assistant", Content: t.Content}
		for _, tc := range t.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, oaiToolCall{
				ID: tc.ID, Type: "function",
				Function: oaiFunction{Name: tc.Name, Arguments: string(tc.Input)},
			})
		}
		return m
	default:
		return oaiMessage{Role: "user", Content: t.Content}
	}
}

func mapFinishReason(fr string) StopReason {
	switch fr {
	case "tool_calls":
		return StopReasonToolUse
	case "length":
		return StopReasonMaxTokens
	case "stop":
		return StopReasonEndTurn
	default:
		return StopReasonEndTurn
	}
}
