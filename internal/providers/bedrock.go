package providers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brdoc "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/document"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/config"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

type bedrockProvider struct {
	model       types.Model
	providerCfg config.ProviderConfig
	apiKey      string
	client      *bedrockruntime.Client
}

func NewBedrockProvider(model types.Model, providerCfg config.ProviderConfig, apiKey string) types.Provider {
	cfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		return &bedrockProvider{model: model, providerCfg: providerCfg, apiKey: apiKey, client: nil}
	}
	return &bedrockProvider{
		model:       model,
		providerCfg: providerCfg,
		apiKey:      apiKey,
		client:      bedrockruntime.NewFromConfig(cfg),
	}
}

func (p *bedrockProvider) API() string { return p.model.API }

func (p *bedrockProvider) Complete(req types.CompletionRequest) (types.CompletionResponse, error) {
	if p.client == nil {
		return types.CompletionResponse{}, fmt.Errorf("bedrock client is not configured; set AWS credentials/profile")
	}

	messages, err := bedrockMessagesFromContext(req.Context, req.Model)
	if err != nil {
		return types.CompletionResponse{}, err
	}
	if len(messages) == 0 {
		messages = append(messages, brtypes.Message{
			Role: brtypes.ConversationRoleUser,
			Content: []brtypes.ContentBlock{
				&brtypes.ContentBlockMemberText{Value: ""},
			},
		})
	}

	baseCtx := req.Options.Context
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, 120*time.Second)
	defer cancel()

	input := &bedrockruntime.ConverseInput{
		ModelId:  &req.Model.ID,
		Messages: messages,
	}
	if prompt := strings.TrimSpace(req.Context.SystemPrompt); prompt != "" {
		input.System = []brtypes.SystemContentBlock{
			&brtypes.SystemContentBlockMemberText{Value: prompt},
		}
	}
	if len(req.Context.Tools) > 0 {
		cfg, err := bedrockToolConfig(req.Context.Tools)
		if err != nil {
			return types.CompletionResponse{}, err
		}
		input.ToolConfig = cfg
	}

	out, err := p.client.Converse(ctx, input)
	if err != nil {
		return types.CompletionResponse{}, err
	}

	assistant := types.Message{
		Role:       types.RoleAssistant,
		Timestamp:  types.NowMillis(),
		API:        req.Model.API,
		Provider:   req.Model.Provider,
		Model:      req.Model.ID,
		StopReason: mapBedrockStopReason(out.StopReason),
	}
	toolCalls := make([]types.ToolCall, 0, 4)
	if out.Output != nil {
		if msg, ok := out.Output.(*brtypes.ConverseOutputMemberMessage); ok {
			for _, c := range msg.Value.Content {
				switch block := c.(type) {
				case *brtypes.ContentBlockMemberText:
					if block.Value != "" {
						assistant.Content = append(assistant.Content, types.ContentBlock{Type: "text", Text: block.Value})
					}
				case *brtypes.ContentBlockMemberToolUse:
					callID := ""
					name := ""
					args := map[string]interface{}{}
					if block.Value.ToolUseId != nil {
						callID = *block.Value.ToolUseId
					}
					if callID == "" {
						callID = "tool_" + randomShortID()
					}
					if block.Value.Name != nil {
						name = *block.Value.Name
					}
					if block.Value.Input != nil {
						_ = block.Value.Input.UnmarshalSmithyDocument(&args)
					}
					assistant.Content = append(assistant.Content, types.ContentBlock{
						Type:      "toolCall",
						ID:        callID,
						Name:      name,
						Arguments: args,
					})
					toolCalls = append(toolCalls, types.ToolCall{
						ID:        callID,
						Name:      name,
						Arguments: args,
					})
				case *brtypes.ContentBlockMemberReasoningContent:
					if textBlock, ok := block.Value.(*brtypes.ReasoningContentBlockMemberReasoningText); ok {
						thinking := ""
						signature := ""
						if textBlock.Value.Text != nil {
							thinking = *textBlock.Value.Text
						}
						if textBlock.Value.Signature != nil {
							signature = *textBlock.Value.Signature
						}
						if thinking != "" {
							assistant.Content = append(assistant.Content, types.ContentBlock{
								Type:             "thinking",
								Thinking:         thinking,
								ThinkingSig:      signature,
								ThoughtSignature: signature,
							})
						}
					}
				}
			}
		}
	}
	if out.Usage != nil {
		var inTok, outTok, totalTok int64
		if out.Usage.InputTokens != nil {
			inTok = int64(*out.Usage.InputTokens)
		}
		if out.Usage.OutputTokens != nil {
			outTok = int64(*out.Usage.OutputTokens)
		}
		if out.Usage.TotalTokens != nil {
			totalTok = int64(*out.Usage.TotalTokens)
		}
		assistant.Usage = types.Usage{
			Input:  inTok,
			Output: outTok,
			Total:  totalTok,
		}
	}
	return types.CompletionResponse{Assistant: assistant, ToolCalls: toolCalls}, nil
}

func bedrockToolConfig(tools []types.Tool) (*brtypes.ToolConfiguration, error) {
	out := make([]brtypes.Tool, 0, len(tools))
	for _, tool := range tools {
		schema := tool.Parameters
		if schema == nil {
			schema = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		spec := brtypes.ToolSpecification{
			Name: aws.String(tool.Name),
			InputSchema: &brtypes.ToolInputSchemaMemberJson{
				Value: brdoc.NewLazyDocument(schema),
			},
		}
		if desc := strings.TrimSpace(tool.Description); desc != "" {
			spec.Description = aws.String(desc)
		}
		out = append(out, &brtypes.ToolMemberToolSpec{Value: spec})
	}
	return &brtypes.ToolConfiguration{Tools: out}, nil
}

func bedrockMessagesFromContext(ctx types.Context, model types.Model) ([]brtypes.Message, error) {
	transformed := transformMessages(ctx.Messages, model, normalizeBedrockToolCallID)
	messages := make([]brtypes.Message, 0, len(transformed))
	for i := 0; i < len(transformed); i++ {
		m := transformed[i]
		switch m.Role {
		case types.RoleUser:
			text := messageText(m)
			if text == "" {
				continue
			}
			messages = append(messages, brtypes.Message{
				Role: brtypes.ConversationRoleUser,
				Content: []brtypes.ContentBlock{
					&brtypes.ContentBlockMemberText{Value: text},
				},
			})
		case types.RoleAssistant:
			blocks := make([]brtypes.ContentBlock, 0, len(m.Content))
			for _, c := range m.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						blocks = append(blocks, &brtypes.ContentBlockMemberText{Value: c.Text})
					}
				case "thinking":
					// Bedrock accepts reasoning blocks only for some models; replay as text for broad compatibility.
					if c.Thinking != "" {
						blocks = append(blocks, &brtypes.ContentBlockMemberText{Value: c.Thinking})
					}
				case "toolCall":
					callID := c.ID
					if callID == "" {
						callID = "tool_" + randomShortID()
					}
					name := c.Name
					if name == "" {
						name = "tool"
					}
					args := c.Arguments
					if args == nil {
						args = map[string]any{}
					}
					blocks = append(blocks, &brtypes.ContentBlockMemberToolUse{
						Value: brtypes.ToolUseBlock{
							ToolUseId: aws.String(callID),
							Name:      aws.String(name),
							Input:     brdoc.NewLazyDocument(args),
						},
					})
				}
			}
			if len(blocks) == 0 {
				continue
			}
			messages = append(messages, brtypes.Message{
				Role:    brtypes.ConversationRoleAssistant,
				Content: blocks,
			})
		case types.RoleTool:
			toolResultBlocks := make([]brtypes.ContentBlock, 0, 4)
			for i < len(transformed) && transformed[i].Role == types.RoleTool {
				tm := transformed[i]
				content := make([]brtypes.ToolResultContentBlock, 0, len(tm.Content))
				for _, cb := range tm.Content {
					if cb.Type == "text" {
						content = append(content, &brtypes.ToolResultContentBlockMemberText{Value: cb.Text})
					}
				}
				if len(content) == 0 {
					content = append(content, &brtypes.ToolResultContentBlockMemberText{Value: "(no output)"})
				}
				status := brtypes.ToolResultStatusSuccess
				if tm.IsError {
					status = brtypes.ToolResultStatusError
				}
				toolCallID := tm.ToolCallID
				if toolCallID == "" {
					toolCallID = "tool_" + randomShortID()
				}
				toolResultBlocks = append(toolResultBlocks, &brtypes.ContentBlockMemberToolResult{
					Value: brtypes.ToolResultBlock{
						ToolUseId: aws.String(toolCallID),
						Content:   content,
						Status:    status,
					},
				})
				i++
			}
			i--
			if len(toolResultBlocks) == 0 {
				continue
			}
			messages = append(messages, brtypes.Message{
				Role:    brtypes.ConversationRoleUser,
				Content: toolResultBlocks,
			})
		}
	}
	return messages, nil
}

func mapBedrockStopReason(reason brtypes.StopReason) string {
	switch reason {
	case brtypes.StopReasonEndTurn, brtypes.StopReasonStopSequence:
		return "stop"
	case brtypes.StopReasonMaxTokens:
		return "length"
	case brtypes.StopReasonToolUse:
		return "toolUse"
	default:
		return "error"
	}
}

func normalizeBedrockToolCallID(id string) string {
	if id == "" {
		return id
	}
	id = sanitizeID(id)
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}

func randomShortID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
