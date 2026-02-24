package providers

import (
	"strings"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

// transformMessages normalizes stored message history for cross-provider replay.
// It mirrors core behavior from pi-ai:
// - converts non-native thinking blocks to plain text
// - normalizes tool call IDs when switching providers
// - inserts synthetic tool results for orphaned tool calls
// - drops errored/aborted assistant messages
func transformMessages(
	messages []types.Message,
	targetModel types.Model,
	normalizeToolCallID func(string) string,
) []types.Message {
	idMap := map[string]string{}
	firstPass := make([]types.Message, 0, len(messages))

	for _, msg := range messages {
		switch msg.Role {
		case types.RoleUser:
			firstPass = append(firstPass, msg)
		case types.RoleTool:
			if normalized, ok := idMap[msg.ToolCallID]; ok && normalized != "" {
				msg.ToolCallID = normalized
			}
			firstPass = append(firstPass, msg)
		case types.RoleAssistant:
			if msg.StopReason == "error" || msg.StopReason == "aborted" {
				continue
			}
			isSameModel := msg.Provider == targetModel.Provider &&
				msg.API == targetModel.API &&
				msg.Model == targetModel.ID

			next := msg
			next.Content = make([]types.ContentBlock, 0, len(msg.Content))
			for _, block := range msg.Content {
				switch block.Type {
				case "thinking":
					thinking := strings.TrimSpace(block.Thinking)
					if thinking == "" {
						continue
					}
					if isSameModel {
						next.Content = append(next.Content, block)
					} else {
						next.Content = append(next.Content, types.ContentBlock{
							Type: "text",
							Text: thinking,
						})
					}
				case "toolCall":
					normalized := block
					if !isSameModel {
						normalized.ThoughtSignature = ""
						if normalizeToolCallID != nil {
							id := normalizeToolCallID(block.ID)
							if id != "" && id != block.ID {
								idMap[block.ID] = id
								normalized.ID = id
							}
						}
					}
					next.Content = append(next.Content, normalized)
				default:
					next.Content = append(next.Content, block)
				}
			}
			firstPass = append(firstPass, next)
		default:
			firstPass = append(firstPass, msg)
		}
	}

	result := make([]types.Message, 0, len(firstPass))
	pendingToolCalls := make([]types.ContentBlock, 0)
	existingToolResults := map[string]bool{}

	appendMissingToolResults := func() {
		for _, tc := range pendingToolCalls {
			if tc.Type != "toolCall" {
				continue
			}
			if existingToolResults[tc.ID] {
				continue
			}
			result = append(result, types.Message{
				Role:       types.RoleTool,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				Timestamp:  types.NowMillis(),
				IsError:    true,
				Content:    []types.ContentBlock{{Type: "text", Text: "No result provided"}},
			})
		}
		pendingToolCalls = pendingToolCalls[:0]
		existingToolResults = map[string]bool{}
	}

	for _, msg := range firstPass {
		switch msg.Role {
		case types.RoleAssistant:
			if len(pendingToolCalls) > 0 {
				appendMissingToolResults()
			}
			pendingToolCalls = pendingToolCalls[:0]
			existingToolResults = map[string]bool{}
			for _, block := range msg.Content {
				if block.Type == "toolCall" {
					pendingToolCalls = append(pendingToolCalls, block)
				}
			}
			result = append(result, msg)
		case types.RoleTool:
			existingToolResults[msg.ToolCallID] = true
			result = append(result, msg)
		case types.RoleUser:
			if len(pendingToolCalls) > 0 {
				appendMissingToolResults()
			}
			result = append(result, msg)
		default:
			result = append(result, msg)
		}
	}

	return result
}
