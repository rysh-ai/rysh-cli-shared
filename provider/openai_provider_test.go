package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestOpenAICompatProvider_CompleteWithTools covers the CHAT COMPLETIONS
// translation, so it runs as ollama: that dialect now serves Ollama and the
// Gemini compat layer only, OpenAI proper having moved to the Responses API
// (TestOpenAIProvider_CompleteWithTools_Responses is its counterpart).
func TestOpenAICompatProvider_CompleteWithTools(t *testing.T) {
	var gotReq oaiRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"choices":[{"message":{"role":"assistant","content":"reading the file",
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"file_read","arguments":"{\"path\":\"a.go\"}"}}]},
				"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":120,"completion_tokens":18}}`)
	}))
	defer srv.Close()

	p := NewOpenAIAgenticProvider("ollama", "k", srv.URL, "llama3.1", 1024)
	tools := []ToolSpec{{Name: "file_read", Description: "read a file", Parameters: json.RawMessage(`{"type":"object"}`)}}
	conv := []ConversationTurn{{Role: "user", Content: "read a.go"}}

	resp, err := p.CompleteWithTools(context.Background(), conv, tools, "you are helpful")
	if err != nil {
		t.Fatal(err)
	}

	// Request translation.
	if gotReq.Model != "llama3.1" || len(gotReq.Messages) != 2 {
		t.Fatalf("req model=%q msgs=%d", gotReq.Model, len(gotReq.Messages))
	}
	if gotReq.Messages[0].Role != "system" || gotReq.Messages[1].Role != "user" {
		t.Fatalf("roles = %q,%q", gotReq.Messages[0].Role, gotReq.Messages[1].Role)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Function.Name != "file_read" || gotReq.Tools[0].Function.Description != "read a file" {
		t.Fatalf("tools = %+v", gotReq.Tools)
	}

	// Response translation.
	if resp.StopReason != StopReasonToolUse {
		t.Fatalf("stop = %v", resp.StopReason)
	}
	if len(resp.TextBlocks) != 1 || resp.TextBlocks[0].Text != "reading the file" {
		t.Fatalf("text = %+v", resp.TextBlocks)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "file_read" || resp.ToolCalls[0].ID != "call_1" {
		t.Fatalf("toolcalls = %+v", resp.ToolCalls)
	}
	if !strings.Contains(string(resp.ToolCalls[0].Input), `"path":"a.go"`) {
		t.Fatalf("tool input = %s", resp.ToolCalls[0].Input)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 18 {
		t.Fatalf("usage = %+v", resp.Usage)
	}
}

func TestOpenAIProvider_ToolResultRoundTrip(t *testing.T) {
	// An assistant tool_call turn + a tool result turn translate correctly.
	p := NewOpenAIAgenticProvider("ollama", "", "http://x/v1", "llama3.1", 0)
	assistant := ConversationTurn{Role: "assistant", Content: "", ToolCalls: []ToolCallRequest{{ID: "c1", Name: "grep", Input: json.RawMessage(`{"q":"x"}`)}}}
	toolResult := ConversationTurn{Role: "tool", ToolCallID: "c1", Content: "match found"}

	m1 := turnToOAI(assistant)
	if m1.Role != "assistant" || len(m1.ToolCalls) != 1 || m1.ToolCalls[0].ID != "c1" || m1.ToolCalls[0].Function.Arguments != `{"q":"x"}` {
		t.Fatalf("assistant msg = %+v", m1)
	}
	m2 := turnToOAI(toolResult)
	if m2.Role != "tool" || m2.ToolCallID != "c1" || m2.Content != "match found" {
		t.Fatalf("tool msg = %+v", m2)
	}
	if p.Name() != "ollama" || p.Model() != "llama3.1" {
		t.Fatalf("name/model = %q/%q", p.Name(), p.Model())
	}
	// WithModelEffort returns a copy pinned to the new model.
	p2 := p.WithModelEffort("llama3.2", "high").(*OpenAIAgenticProvider)
	if p2.Model() != "llama3.2" || p.Model() != "llama3.1" {
		t.Fatalf("override leaked: p=%q p2=%q", p.Model(), p2.Model())
	}
}

func TestMapFinishReason(t *testing.T) {
	if mapFinishReason("stop") != StopReasonEndTurn || mapFinishReason("tool_calls") != StopReasonToolUse || mapFinishReason("length") != StopReasonMaxTokens {
		t.Fatal("finish reason mapping wrong")
	}
}
