package providers

import (
	"testing"

	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

func TestBedrockMessagesFromContextGroupsToolResults(t *testing.T) {
	ctx := types.Context{
		Messages: []types.Message{
			types.TextMessage(types.RoleUser, "Use tools"),
			{
				Role: types.RoleAssistant,
				Content: []types.ContentBlock{
					{Type: "toolCall", ID: "call_a", Name: "read", Arguments: map[string]any{"path": "a.txt"}},
				},
			},
			types.ToolResultMessage("call_a", "read", types.ToolResult{
				Content: []types.ContentBlock{{Type: "text", Text: "first"}},
				IsError: false,
			}),
			types.ToolResultMessage("call_b", "grep", types.ToolResult{
				Content: []types.ContentBlock{{Type: "text", Text: "second"}},
				IsError: true,
			}),
			types.TextMessage(types.RoleUser, "continue"),
		},
	}

	msgs, err := bedrockMessagesFromContext(ctx, types.Model{
		ID:       "us.anthropic.claude-opus-4-6-v1",
		API:      "bedrock-converse-stream",
		Provider: "amazon-bedrock",
	})
	if err != nil {
		t.Fatalf("bedrockMessagesFromContext returned error: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 bedrock messages, got %d", len(msgs))
	}

	if msgs[2].Role != brtypes.ConversationRoleUser {
		t.Fatalf("expected grouped tool-result message with user role, got %s", msgs[2].Role)
	}
	if len(msgs[2].Content) != 2 {
		t.Fatalf("expected 2 grouped tool-result blocks, got %d", len(msgs[2].Content))
	}

	first, ok := msgs[2].Content[0].(*brtypes.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("expected first grouped block to be tool_result, got %T", msgs[2].Content[0])
	}
	if first.Value.ToolUseId == nil || *first.Value.ToolUseId != "call_a" {
		t.Fatalf("unexpected first tool_use_id: %#v", first.Value.ToolUseId)
	}
	if first.Value.Status != brtypes.ToolResultStatusSuccess {
		t.Fatalf("unexpected first tool result status: %s", first.Value.Status)
	}

	second, ok := msgs[2].Content[1].(*brtypes.ContentBlockMemberToolResult)
	if !ok {
		t.Fatalf("expected second grouped block to be tool_result, got %T", msgs[2].Content[1])
	}
	if second.Value.ToolUseId == nil || *second.Value.ToolUseId != "call_b" {
		t.Fatalf("unexpected second tool_use_id: %#v", second.Value.ToolUseId)
	}
	if second.Value.Status != brtypes.ToolResultStatusError {
		t.Fatalf("unexpected second tool result status: %s", second.Value.Status)
	}
}

func TestBedrockToolConfig(t *testing.T) {
	cfg, err := bedrockToolConfig([]types.Tool{
		{
			Name:        "read",
			Description: "Read a file",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
				"required": []string{"path"},
			},
		},
	})
	if err != nil {
		t.Fatalf("bedrockToolConfig returned error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil tool config")
	}
	if len(cfg.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(cfg.Tools))
	}
	toolSpec, ok := cfg.Tools[0].(*brtypes.ToolMemberToolSpec)
	if !ok {
		t.Fatalf("expected ToolMemberToolSpec, got %T", cfg.Tools[0])
	}
	if toolSpec.Value.Name == nil || *toolSpec.Value.Name != "read" {
		t.Fatalf("unexpected tool name: %#v", toolSpec.Value.Name)
	}
	if toolSpec.Value.InputSchema == nil {
		t.Fatal("expected input schema to be present")
	}
}

func TestMapBedrockStopReason(t *testing.T) {
	cases := map[brtypes.StopReason]string{
		brtypes.StopReasonToolUse:         "toolUse",
		brtypes.StopReasonEndTurn:         "stop",
		brtypes.StopReasonStopSequence:    "stop",
		brtypes.StopReasonMaxTokens:       "length",
		brtypes.StopReasonContentFiltered: "error",
	}
	for in, want := range cases {
		got := mapBedrockStopReason(in)
		if got != want {
			t.Fatalf("mapBedrockStopReason(%s) = %q, want %q", in, got, want)
		}
	}
}
