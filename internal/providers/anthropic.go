package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/config"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

type anthropicProvider struct {
	model       types.Model
	providerCfg config.ProviderConfig
	apiKey      string
	httpClient  *http.Client
}

func NewAnthropicProvider(model types.Model, providerCfg config.ProviderConfig, apiKey string) types.Provider {
	return &anthropicProvider{
		model:       model,
		providerCfg: providerCfg,
		apiKey:      apiKey,
		httpClient:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *anthropicProvider) API() string { return p.model.API }

type anthropicRequest struct {
	Model     string           `json:"model"`
	System    string           `json:"system,omitempty"`
	Messages  []map[string]any `json:"messages"`
	Tools     []map[string]any `json:"tools,omitempty"`
	MaxTokens int              `json:"max_tokens"`
	Stream    bool             `json:"stream"`
}

type anthropicResponse struct {
	Content []struct {
		Type  string                 `json:"type"`
		Text  string                 `json:"text,omitempty"`
		ID    string                 `json:"id,omitempty"`
		Name  string                 `json:"name,omitempty"`
		Input map[string]interface{} `json:"input,omitempty"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Error any `json:"error,omitempty"`
}

func (p *anthropicProvider) Complete(req types.CompletionRequest) (types.CompletionResponse, error) {
	payload := anthropicRequest{
		Model:     req.Model.ID,
		System:    req.Context.SystemPrompt,
		Messages:  anthropicMessagesFromContext(req.Context, req.Model),
		Tools:     anthropicTools(req.Context.Tools),
		MaxTokens: req.Model.MaxTokens,
		Stream:    false,
	}
	if req.Options.MaxTokens != nil {
		payload.MaxTokens = *req.Options.MaxTokens
	}
	if payload.MaxTokens == 0 {
		payload.MaxTokens = 4096
	}

	b, _ := json.Marshal(payload)
	base := strings.TrimSuffix(req.Model.BaseURL, "/")
	if base == "" {
		base = strings.TrimSuffix(p.providerCfg.BaseURL, "/")
	}
	if base == "" {
		base = "https://api.anthropic.com"
	}
	url := base + "/v1/messages"

	ctx := req.Options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("anthropic-version", "2023-06-01")
	apiKey := req.Options.APIKey
	if apiKey == "" {
		apiKey = p.apiKey
	}
	if apiKey == "" {
		apiKey = p.providerCfg.APIKey
	}
	hreq.Header.Set("x-api-key", apiKey)
	for k, v := range mergeHeaders(req.Model.Headers, req.Options.Headers, p.providerCfg.Headers) {
		hreq.Header.Set(k, v)
	}

	resp, err := p.httpClient.Do(hreq)
	if err != nil {
		return types.CompletionResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return types.CompletionResponse{}, fmt.Errorf("provider error %d: %s", resp.StatusCode, string(body))
	}

	var out anthropicResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return types.CompletionResponse{}, err
	}

	assistant := types.Message{
		Role:       types.RoleAssistant,
		Timestamp:  types.NowMillis(),
		API:        req.Model.API,
		Provider:   req.Model.Provider,
		Model:      req.Model.ID,
		StopReason: out.StopReason,
		Usage: types.Usage{
			Input:  out.Usage.InputTokens,
			Output: out.Usage.OutputTokens,
			Total:  out.Usage.InputTokens + out.Usage.OutputTokens,
		},
	}
	toolCalls := make([]types.ToolCall, 0)
	for _, block := range out.Content {
		switch block.Type {
		case "text":
			assistant.Content = append(assistant.Content, types.ContentBlock{Type: "text", Text: block.Text})
		case "tool_use":
			assistant.Content = append(assistant.Content, types.ContentBlock{Type: "toolCall", ID: block.ID, Name: block.Name, Arguments: block.Input})
			toolCalls = append(toolCalls, types.ToolCall{ID: block.ID, Name: block.Name, Arguments: block.Input})
		}
	}
	return types.CompletionResponse{Assistant: assistant, ToolCalls: toolCalls}, nil
}

func anthropicTools(tools []types.Tool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.Parameters,
		})
	}
	return out
}

func anthropicMessagesFromContext(ctx types.Context, model types.Model) []map[string]any {
	transformed := transformMessages(ctx.Messages, model, normalizeAnthropicToolCallID)
	messages := make([]map[string]any, 0, len(transformed))
	for i := 0; i < len(transformed); i++ {
		m := transformed[i]
		switch m.Role {
		case types.RoleUser:
			messages = append(messages, map[string]any{
				"role": "user",
				"content": []map[string]any{{
					"type": "text",
					"text": messageText(m),
				}},
			})
		case types.RoleAssistant:
			parts := []map[string]any{}
			for _, c := range m.Content {
				if c.Type == "text" {
					parts = append(parts, map[string]any{"type": "text", "text": c.Text})
				} else if c.Type == "thinking" {
					if strings.TrimSpace(c.Thinking) == "" {
						continue
					}
					if c.ThinkingSig != "" {
						parts = append(parts, map[string]any{
							"type":      "thinking",
							"thinking":  c.Thinking,
							"signature": c.ThinkingSig,
						})
					} else {
						parts = append(parts, map[string]any{"type": "text", "text": c.Thinking})
					}
				} else if c.Type == "toolCall" {
					id := c.ID
					if id == "" {
						id = "tool_" + randomOpenAIID()
					}
					parts = append(parts, map[string]any{"type": "tool_use", "id": id, "name": c.Name, "input": c.Arguments})
				}
			}
			if len(parts) == 0 {
				parts = append(parts, map[string]any{"type": "text", "text": messageText(m)})
			}
			messages = append(messages, map[string]any{"role": "assistant", "content": parts})
		case types.RoleTool:
			parts := []map[string]any{}
			for i < len(transformed) && transformed[i].Role == types.RoleTool {
				tm := transformed[i]
				content := messageText(tm)
				if strings.TrimSpace(content) == "" {
					content = "(no output)"
				}
				parts = append(parts, map[string]any{
					"type":        "tool_result",
					"tool_use_id": tm.ToolCallID,
					"content":     content,
					"is_error":    tm.IsError,
				})
				i++
			}
			i--
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": parts,
			})
		}
	}
	return messages
}

func normalizeAnthropicToolCallID(id string) string {
	if id == "" {
		return id
	}
	id = sanitizeID(id)
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}
