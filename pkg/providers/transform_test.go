package providers

import (
	"testing"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

func TestTransformMessagesAddsSyntheticToolResult(t *testing.T) {
	target := types.Model{
		ID:       "claude-opus-4-6",
		API:      "anthropic-messages",
		Provider: "anthropic",
	}

	msgs := []types.Message{
		{
			Role:     types.RoleAssistant,
			API:      "openai-completions",
			Provider: "openai",
			Model:    "gpt-5.1-codex",
			Content: []types.ContentBlock{
				{Type: "toolCall", ID: "call|bad++", Name: "read", Arguments: map[string]any{"path": "a.txt"}},
			},
		},
		types.TextMessage(types.RoleUser, "continue"),
	}

	out := transformMessages(msgs, target, func(id string) string { return normalizeAnthropicToolCallID(id) })
	if len(out) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(out))
	}
	if out[0].Role != types.RoleAssistant {
		t.Fatalf("expected first message assistant, got %s", out[0].Role)
	}
	if len(out[0].Content) != 1 || out[0].Content[0].Type != "toolCall" {
		t.Fatalf("expected first message to contain tool call, got %#v", out[0].Content)
	}
	normalizedID := out[0].Content[0].ID
	if normalizedID == "call|bad++" {
		t.Fatalf("expected tool call id to be normalized, got %q", normalizedID)
	}

	if out[1].Role != types.RoleTool {
		t.Fatalf("expected synthetic tool result as second message, got %s", out[1].Role)
	}
	if out[1].ToolCallID != normalizedID {
		t.Fatalf("synthetic tool result id mismatch: %q != %q", out[1].ToolCallID, normalizedID)
	}
	if !out[1].IsError {
		t.Fatal("expected synthetic tool result to be error=true")
	}

	if out[2].Role != types.RoleUser {
		t.Fatalf("expected user message last, got %s", out[2].Role)
	}
}

func TestTransformMessagesThinkingConversion(t *testing.T) {
	target := types.Model{
		ID:       "claude-opus-4-6",
		API:      "anthropic-messages",
		Provider: "anthropic",
	}
	msgs := []types.Message{
		{
			Role:     types.RoleAssistant,
			API:      "openai-completions",
			Provider: "openai",
			Model:    "gpt-5.1-codex",
			Content:  []types.ContentBlock{{Type: "thinking", Thinking: "hidden chain"}},
		},
		{
			Role:     types.RoleAssistant,
			API:      "anthropic-messages",
			Provider: "anthropic",
			Model:    "claude-opus-4-6",
			Content:  []types.ContentBlock{{Type: "thinking", Thinking: "native chain", ThinkingSig: "sig"}},
		},
	}

	out := transformMessages(msgs, target, nil)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(out))
	}
	if len(out[0].Content) != 1 || out[0].Content[0].Type != "text" {
		t.Fatalf("expected cross-model thinking converted to text, got %#v", out[0].Content)
	}
	if len(out[1].Content) != 1 || out[1].Content[0].Type != "thinking" {
		t.Fatalf("expected same-model thinking preserved, got %#v", out[1].Content)
	}
}
