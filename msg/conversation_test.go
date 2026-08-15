// SPDX-License-Identifier: Apache-2.0

package msg

import (
	"encoding/json"
	"testing"
)

func TestConversationMessage_JSONRoundTrip(t *testing.T) {
	cm := &ConversationMessage{
		TurnID:           "test-turn-123",
		TurnType:         TurnQuestion,
		ConversationType: ConvShell,
		InputType:        InputShell,
		MessageSource:    SourceHuman,
		Content:          "ls -la",
		TimestampMs:      1700000000000,
		SubjectToShare:   true,
	}

	data, err := json.Marshal(cm)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded ConversationMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.TurnID != cm.TurnID {
		t.Errorf("TurnID: got %q, want %q", decoded.TurnID, cm.TurnID)
	}
	if decoded.TurnType != TurnQuestion {
		t.Errorf("TurnType: got %q, want %q", decoded.TurnType, TurnQuestion)
	}
	if decoded.ConversationType != ConvShell {
		t.Errorf("ConversationType: got %q, want %q", decoded.ConversationType, ConvShell)
	}
	if decoded.Content != "ls -la" {
		t.Errorf("Content: got %q, want %q", decoded.Content, "ls -la")
	}
	if !decoded.SubjectToShare {
		t.Error("SubjectToShare: got false, want true")
	}
	if decoded.Sensitive {
		t.Error("Sensitive: got true, want false (omitempty)")
	}
}

func TestConversationMessage_StreamingAnswer(t *testing.T) {
	cm := NewAIAnswer("turn-456", "partial output...", true)

	if cm.TurnType != TurnAnswer {
		t.Errorf("TurnType: got %q, want %q", cm.TurnType, TurnAnswer)
	}
	if cm.ConversationType != ConvAI {
		t.Errorf("ConversationType: got %q, want %q", cm.ConversationType, ConvAI)
	}
	if !cm.Streaming {
		t.Error("Streaming: got false, want true")
	}
	if cm.MessageSource != SourceAI {
		t.Errorf("MessageSource: got %q, want %q", cm.MessageSource, SourceAI)
	}
}

func TestConversationType_IsDualPublish(t *testing.T) {
	tests := []struct {
		ct   ConversationType
		want bool
	}{
		{ConvShell, true},
		{ConvAI, true},
		{ConvRysh, false},
		{ConvChat, false},
		{ConvEmail, false},
		{ConvSlack, false},
		{ConvChatbot, false},
	}
	for _, tt := range tests {
		if got := tt.ct.IsDualPublish(); got != tt.want {
			t.Errorf("IsDualPublish(%q): got %v, want %v", tt.ct, got, tt.want)
		}
	}
}

func TestConversationType_HasMemory(t *testing.T) {
	tests := []struct {
		ct   ConversationType
		want bool
	}{
		{ConvShell, false},
		{ConvAI, true},
		{ConvRysh, false},
		{ConvChat, false},
		{ConvEmail, true},
		{ConvSlack, true},
		{ConvChatbot, true},
	}
	for _, tt := range tests {
		if got := tt.ct.HasMemory(); got != tt.want {
			t.Errorf("HasMemory(%q): got %v, want %v", tt.ct, got, tt.want)
		}
	}
}

func TestNewTurnID(t *testing.T) {
	id1 := NewTurnID()
	id2 := NewTurnID()

	if id1 == "" {
		t.Error("NewTurnID returned empty string")
	}
	if id1 == id2 {
		t.Error("NewTurnID returned duplicate IDs")
	}
}

func TestConversationConstructors(t *testing.T) {
	turnID := "test-turn"

	shell := NewShellQuestion(turnID, "echo hello")
	if shell.TurnType != TurnQuestion {
		t.Errorf("ShellQuestion.TurnType: got %q", shell.TurnType)
	}
	if shell.ConversationType != ConvShell {
		t.Errorf("ShellQuestion.ConversationType: got %q", shell.ConversationType)
	}

	rysh := NewRyshAnswer(turnID, "Tab created")
	if rysh.TurnType != TurnAnswer {
		t.Errorf("RyshAnswer.TurnType: got %q", rysh.TurnType)
	}
	if rysh.ConversationType != ConvRysh {
		t.Errorf("RyshAnswer.ConversationType: got %q", rysh.ConversationType)
	}

	chat := NewChatMessage(turnID, TurnQuestion, "hello", "visitor", SourceExternal)
	if chat.Role != "visitor" {
		t.Errorf("ChatMessage.Role: got %q", chat.Role)
	}
	if chat.MessageSource != SourceExternal {
		t.Errorf("ChatMessage.MessageSource: got %q", chat.MessageSource)
	}

	channel := NewChannelMessage(turnID, TurnAnswer, ConvEmail, "reply", "assistant", SourceHumanoid, true)
	if channel.ConversationType != ConvEmail {
		t.Errorf("ChannelMessage.ConversationType: got %q", channel.ConversationType)
	}
	if !channel.Sensitive {
		t.Error("ChannelMessage.Sensitive: got false, want true")
	}
}

func TestMsgConversationAppend_JSONRoundTrip(t *testing.T) {
	cm := NewShellAnswer("turn-789", "drwxr-xr-x  5 user staff 160 Jan  1 00:00 .\n")
	wrapped := &MsgConversationAppend{Message: cm}

	data, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded MsgConversationAppend
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.Message == nil {
		t.Fatal("decoded.Message is nil")
	}
	if decoded.Message.TurnID != "turn-789" {
		t.Errorf("TurnID: got %q", decoded.Message.TurnID)
	}
	if decoded.Message.ConversationType != ConvShell {
		t.Errorf("ConversationType: got %q", decoded.Message.ConversationType)
	}
}

func TestCodecRegistry_ConversationTypes(t *testing.T) {
	reg := DefaultCodecRegistry()

	// Verify new types are registered.
	cm := &MsgConversationAppend{Message: NewAIAnswer("t1", "hello", false)}
	tag := reg.TagOf(cm)
	if tag != TagConversationAppend {
		t.Errorf("TagOf(MsgConversationAppend): got %q, want %q", tag, TagConversationAppend)
	}

	hm := &MsgConversationHistoryAppend{Message: NewShellQuestion("t2", "ls")}
	htag := reg.TagOf(hm)
	if htag != TagConversationHistoryAppend {
		t.Errorf("TagOf(MsgConversationHistoryAppend): got %q, want %q", htag, TagConversationHistoryAppend)
	}

	// Verify decode works.
	payload, _ := json.Marshal(cm)
	decoded, err := reg.Decode(TagConversationAppend, payload)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	result, ok := decoded.(*MsgConversationAppend)
	if !ok {
		t.Fatalf("Decode: expected *MsgConversationAppend, got %T", decoded)
	}
	if result.Message.Content != "hello" {
		t.Errorf("decoded content: got %q", result.Message.Content)
	}
}
