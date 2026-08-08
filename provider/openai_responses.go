package provider

// openai_responses.go — the OpenAI Responses API (`POST /v1/responses`).
//
// Why a second dialect exists. rysh spoke only Chat Completions, and OpenAI's
// current reasoning models refuse function tools there:
//
//	400: Function tools with reasoning_effort are not supported for
//	gpt-5.6-sol in /v1/chat/completions. To use function tools, use
//	/v1/responses or set reasoning_effort to 'none'.
//
// Since rysh's whole agentic loop is tool calls, that made the GPT-5.6 tier
// (sol / terra / luna) unusable — not degraded, unusable. Setting
// reasoning_effort to "none" would technically satisfy the endpoint while
// switching off the reasoning those models are bought for, so the real fix is
// to speak Responses.
//
// Routing: the `openai` family goes here; Ollama and the Gemini compat layer
// stay on Chat Completions (openai_provider.go), because they implement that
// dialect and not this one. Verified against the live API — gpt-4o, and all
// three GPT-5.6 tiers, answer correctly here, so this is not a
// reasoning-models-only path.
//
// The dialects differ in more than the URL, which is why this is a separate
// translation rather than a flag on the old one:
//
//	Chat Completions                    Responses
//	messages: [...]                     input: [...]
//	system message in messages          instructions: "..."
//	max_completion_tokens               max_output_tokens
//	tools[].function.{name,...}         tools[].{name,...}   (flat)
//	choices[0].message.content          output[].content[].text  (output_text)
//	choices[0].message.tool_calls[]     output[] items of type function_call
//	tool result: role:"tool" message    input item of type function_call_output
//	usage.{prompt,completion}_tokens    usage.{input,output}_tokens

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ---------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------

// respRequest is the POST /v1/responses body.
type respRequest struct {
	Model        string      `json:"model"`
	Input        []respInput `json:"input"`
	Instructions string      `json:"instructions,omitempty"`
	Tools        []respTool  `json:"tools,omitempty"`
	// MaxOutputTokens is this dialect's output cap — neither max_tokens nor
	// max_completion_tokens is accepted here.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// Store is sent explicitly as false: the endpoint defaults to TRUE, which
	// would persist every rysh conversation on OpenAI's side for later
	// retrieval. Opting out keeps the default privacy posture the Chat
	// Completions path had, where no such retention exists.
	Store bool `json:"store"`
	// Stream is set only by the streaming path (openai_responses_streaming.go).
	// Unlike Chat Completions there is no stream_options companion — usage
	// always rides the terminal event.
	Stream bool `json:"stream,omitempty"`
}

// respInput is one item in the `input` array. The array is heterogeneous: a
// conversational turn carries Role+Content, while a tool call and its result
// are bare items identified by Type. Exactly one shape is populated per item.
type respInput struct {
	Type    string `json:"type,omitempty"`
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
	// Type "function_call": the assistant's request, echoed back on the next
	// turn so the model sees its own call alongside the result.
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// Type "function_call_output": the result, matched to the call by CallID.
	Output string `json:"output,omitempty"`
}

// respTool is a function tool. Note the flat shape — Chat Completions nests
// these fields under a "function" object, Responses does not.
type respTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// respResponse is the POST /v1/responses reply.
type respResponse struct {
	Status string `json:"status"` // "completed" | "incomplete" | "failed"
	Output []struct {
		Type    string `json:"type"` // "message" | "function_call" | "reasoning"
		Content []struct {
			Type string `json:"type"` // "output_text"
			Text string `json:"text"`
		} `json:"content"`
		// type "function_call"
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"` // e.g. "max_output_tokens"
	} `json:"incomplete_details"`
	Usage struct {
		InputTokens        int `json:"input_tokens"`
		OutputTokens       int `json:"output_tokens"`
		InputTokensDetails struct {
			CachedTokens     int `json:"cached_tokens"`
			CacheWriteTokens int `json:"cache_write_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// ---------------------------------------------------------------------------
// Dialect routing
// ---------------------------------------------------------------------------

// usesResponsesAPI reports whether this provider should speak Responses rather
// than Chat Completions. True for OpenAI proper only: Ollama and the Gemini
// compat layer expose Chat Completions and would 404 here.
func (c *OpenAIAgenticProvider) usesResponsesAPI() bool {
	return strings.EqualFold(strings.TrimSpace(c.name), "openai")
}

// ---------------------------------------------------------------------------
// Request assembly
// ---------------------------------------------------------------------------

// buildResponsesRequest translates the neutral conversation into the Responses
// dialect.
func (c *OpenAIAgenticProvider) buildResponsesRequest(conversation []ConversationTurn, tools []ToolSpec, systemPrompt string) respRequest {
	r := respRequest{
		Model:           c.model,
		Instructions:    systemPrompt,
		MaxOutputTokens: c.maxTokens,
		Store:           false,
	}
	for _, t := range conversation {
		r.Input = append(r.Input, turnToResponses(t)...)
	}
	for _, ts := range tools {
		r.Tools = append(r.Tools, respTool{
			Type: "function", Name: ts.Name,
			Description: ts.Description, Parameters: ts.Parameters,
		})
	}
	return r
}

// buildTurnResponsesRequest is buildResponsesRequest for the native
// ChatProvider path: neutral Turns straight to the Responses dialect, with no
// ConversationTurn in between. Equivalent to
// buildResponsesRequest(ConversationFromTurns(turns), ...) byte-for-byte —
// flattenTurns reproduces the same message fan-out and unitToResponses mirrors
// turnToResponses' per-item mapping. Pinned by the differential tests in
// chat_native_test.go, which diff the two paths' raw request bodies.
func (c *OpenAIAgenticProvider) buildTurnResponsesRequest(turns []Turn, tools []ToolSpec, systemPrompt string) respRequest {
	r := respRequest{
		Model:           c.model,
		Instructions:    systemPrompt,
		MaxOutputTokens: c.maxTokens,
		Store:           false,
	}
	for _, u := range flattenTurns(turns) {
		r.Input = append(r.Input, unitToResponses(u)...)
	}
	for _, ts := range tools {
		r.Tools = append(r.Tools, respTool{
			Type: "function", Name: ts.Name,
			Description: ts.Description, Parameters: ts.Parameters,
		})
	}
	return r
}

// turnToResponses converts one neutral turn into input items. It returns a
// slice because an assistant turn that made tool calls expands into its text
// (when any) plus one function_call item per call — this dialect has no
// single message object that carries both.
func turnToResponses(t ConversationTurn) []respInput {
	switch t.Role {
	case "tool":
		// A tool result is a bare item keyed by call id, not a role message.
		// IsError is folded into the text: the dialect has no error flag, and
		// dropping it would leave the model unable to tell a failure from a
		// successful empty result.
		out := t.Content
		if t.IsError {
			out = "ERROR: " + out
		}
		return []respInput{{Type: "function_call_output", CallID: t.ToolCallID, Output: out}}

	case "assistant":
		var items []respInput
		if t.Content != "" {
			items = append(items, respInput{Role: "assistant", Content: t.Content})
		}
		for _, tc := range t.ToolCalls {
			args := string(tc.Input)
			if args == "" {
				args = "{}"
			}
			items = append(items, respInput{
				Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: args,
			})
		}
		return items

	default:
		return []respInput{{Role: "user", Content: t.Content}}
	}
}

// unitToResponses converts one wire unit into input items — the native mirror
// of turnToResponses (same fan-out, same IsError folding, same "{}" default for
// empty tool arguments, same drops: this dialect carries neither thinking
// blocks nor image/text attachments).
func unitToResponses(u wireTurn) []respInput {
	switch u.role {
	case "tool":
		out := u.result.Content
		if u.result.IsError {
			out = "ERROR: " + out
		}
		return []respInput{{Type: "function_call_output", CallID: u.result.CallID, Output: out}}

	case "assistant":
		var items []respInput
		if u.content != "" {
			items = append(items, respInput{Role: "assistant", Content: u.content})
		}
		for _, tc := range u.toolCalls {
			args := string(tc.ParamsJSON)
			if args == "" {
				args = "{}"
			}
			items = append(items, respInput{
				Type: "function_call", CallID: tc.ID, Name: tc.Name, Arguments: args,
			})
		}
		return items

	default:
		return []respInput{{Role: "user", Content: u.content}}
	}
}

// ---------------------------------------------------------------------------
// Transport
// ---------------------------------------------------------------------------

// doResponses performs one non-streaming round trip against /v1/responses and
// maps the reply onto the neutral AgenticResponse.
func (c *OpenAIAgenticProvider) doResponses(ctx context.Context, reqBody respRequest) (*AgenticResponse, error) {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiURL+"/responses", bytes.NewReader(body))
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
		return nil, newOpenAIHTTPError(c.name, "status", resp.StatusCode, resp.Header, string(respBody))
	}

	var out respResponse
	if err := json.Unmarshal(respBody, &out); err != nil {
		return nil, fmt.Errorf("%s: decode: %w", c.name, err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %s", c.name, out.Error.Message)
	}
	return responsesToAgentic(&out), nil
}

// responsesToAgentic maps a decoded reply onto the neutral response shape.
//
// Stop reason is derived from the output rather than a dedicated field: this
// dialect reports "completed" whether or not tools were called, so a
// function_call in the output is what tells the orchestrator to keep looping.
// A truncated reply arrives as status "incomplete" with a reason instead.
func responsesToAgentic(out *respResponse) *AgenticResponse {
	ar := &AgenticResponse{
		StopReason: StopReasonEndTurn,
		Usage: Usage{
			InputTokens:              out.Usage.InputTokens,
			OutputTokens:             out.Usage.OutputTokens,
			CacheReadInputTokens:     out.Usage.InputTokensDetails.CachedTokens,
			CacheCreationInputTokens: out.Usage.InputTokensDetails.CacheWriteTokens,
		},
	}
	for _, item := range out.Output {
		switch item.Type {
		case "message":
			for _, ct := range item.Content {
				if ct.Type == "output_text" && ct.Text != "" {
					ar.TextBlocks = append(ar.TextBlocks, TextBlock{Text: ct.Text})
				}
			}
		case "function_call":
			args := item.Arguments
			if args == "" {
				args = "{}"
			}
			ar.ToolCalls = append(ar.ToolCalls, ToolCallRequest{
				ID: item.CallID, Name: item.Name, Input: json.RawMessage(args),
			})
		}
		// "reasoning" items carry no client-visible content (the raw chain of
		// thought is not returned) and are skipped rather than mapped.
	}
	if len(ar.ToolCalls) > 0 {
		ar.StopReason = StopReasonToolUse
	}
	if out.Status == "incomplete" && out.IncompleteDetails != nil &&
		out.IncompleteDetails.Reason == "max_output_tokens" {
		ar.StopReason = StopReasonMaxTokens
	}
	return ar
}
