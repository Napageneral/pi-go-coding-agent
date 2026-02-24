package types

import (
	"context"
	"time"
)

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "toolResult"
)

// ContentBlock is a unified content representation across providers.
// Only fields relevant to Type are populated.
type ContentBlock struct {
	Type             string                 `json:"type"`
	Text             string                 `json:"text,omitempty"`
	Thinking         string                 `json:"thinking,omitempty"`
	ThinkingSig      string                 `json:"thinkingSignature,omitempty"`
	Data             string                 `json:"data,omitempty"`
	MimeType         string                 `json:"mimeType,omitempty"`
	ID               string                 `json:"id,omitempty"`
	Name             string                 `json:"name,omitempty"`
	Arguments        map[string]interface{} `json:"arguments,omitempty"`
	TextSignature    string                 `json:"textSignature,omitempty"`
	ThoughtSignature string                 `json:"thoughtSignature,omitempty"`
}

type Usage struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	Total      int64   `json:"totalTokens"`
	CostTotal  float64 `json:"costTotal,omitempty"`
}

type Message struct {
	Role       string         `json:"role"`
	Content    []ContentBlock `json:"content"`
	Timestamp  int64          `json:"timestamp"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	ToolName   string         `json:"toolName,omitempty"`
	IsError    bool           `json:"isError,omitempty"`
	API        string         `json:"api,omitempty"`
	Provider   string         `json:"provider,omitempty"`
	Model      string         `json:"model,omitempty"`
	StopReason string         `json:"stopReason,omitempty"`
	Error      string         `json:"errorMessage,omitempty"`
	Usage      Usage          `json:"usage,omitempty"`
}

type Context struct {
	SystemPrompt string         `json:"systemPrompt,omitempty"`
	Messages     []Message      `json:"messages"`
	Tools        []Tool         `json:"tools,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type Tool struct {
	Name        string         `json:"name"`
	Label       string         `json:"label,omitempty"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
}

type ToolResult struct {
	Content []ContentBlock `json:"content"`
	Details map[string]any `json:"details,omitempty"`
	IsError bool           `json:"isError"`
}

type ToolExecutor interface {
	Definition() Tool
	Execute(ctx context.Context, toolCallID string, args map[string]interface{}) (ToolResult, error)
}

type CompletionRequest struct {
	Model   Model
	Context Context
	Options CompletionOptions
}

type CompletionOptions struct {
	APIKey      string
	Temperature *float64
	MaxTokens   *int
	Headers     map[string]string
	SessionID   string
	Context     context.Context
}

type CompletionResponse struct {
	Assistant Message
	ToolCalls []ToolCall
}

type Provider interface {
	API() string
	Complete(req CompletionRequest) (CompletionResponse, error)
}

type Model struct {
	ID            string            `json:"id"`
	Name          string            `json:"name"`
	API           string            `json:"api"`
	Provider      string            `json:"provider"`
	BaseURL       string            `json:"baseUrl"`
	Reasoning     bool              `json:"reasoning"`
	Input         []string          `json:"input"`
	ContextWindow int               `json:"contextWindow"`
	MaxTokens     int               `json:"maxTokens"`
	Headers       map[string]string `json:"headers,omitempty"`
	Compat        map[string]any    `json:"compat,omitempty"`
}

func NowMillis() int64 {
	return time.Now().UnixMilli()
}

func TextMessage(role, text string) Message {
	return Message{
		Role:      role,
		Timestamp: NowMillis(),
		Content: []ContentBlock{
			{Type: "text", Text: text},
		},
	}
}

func ToolResultMessage(toolCallID, toolName string, result ToolResult) Message {
	return Message{
		Role:       RoleTool,
		ToolCallID: toolCallID,
		ToolName:   toolName,
		IsError:    result.IsError,
		Timestamp:  NowMillis(),
		Content:    result.Content,
	}
}
