package extensionsidecar

import "github.com/badlogic/pi-mono/go-coding-agent/internal/types"

const ProtocolVersion = "2026-02-24"

type InitializeRequest struct {
	ProtocolVersion string         `json:"protocolVersion"`
	CWD             string         `json:"cwd"`
	SessionID       string         `json:"sessionId"`
	SessionFile     string         `json:"sessionFile"`
	SessionName     string         `json:"sessionName,omitempty"`
	HostTools       []types.Tool   `json:"hostTools,omitempty"`
	ActiveTools     []string       `json:"activeTools,omitempty"`
	ExtensionPaths  []string       `json:"extensionPaths,omitempty"`
	FlagValues      map[string]any `json:"flagValues,omitempty"`
}

type ExtensionFlagDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type"`
	Default     any    `json:"default,omitempty"`
}

type ExtensionCommandDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type ProviderRegistration struct {
	Name   string         `json:"name"`
	Config map[string]any `json:"config,omitempty"`
}

type InitializeResponse struct {
	ProtocolVersion string                       `json:"protocolVersion"`
	SidecarVersion  string                       `json:"sidecarVersion,omitempty"`
	Capabilities    []string                     `json:"capabilities,omitempty"`
	Tools           []types.Tool                 `json:"tools,omitempty"`
	Flags           []ExtensionFlagDefinition    `json:"flags,omitempty"`
	Commands        []ExtensionCommandDefinition `json:"commands,omitempty"`
	Providers       []ProviderRegistration       `json:"providers,omitempty"`
}

type Event struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

type EmitRequest struct {
	Event Event `json:"event"`
}

type InputEventResult struct {
	Action        string `json:"action,omitempty"`
	Text          string `json:"text,omitempty"`
	AssistantText string `json:"assistantText,omitempty"`
}

type BeforeAgentStartEventResult struct {
	SystemPrompt string          `json:"systemPrompt,omitempty"`
	Messages     []types.Message `json:"messages,omitempty"`
}

type ContextEventResult struct {
	SystemPrompt string          `json:"systemPrompt,omitempty"`
	Messages     []types.Message `json:"messages,omitempty"`
}

type ToolCallEventResult struct {
	Block  bool   `json:"block,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type ToolResultEventResult struct {
	Content []types.ContentBlock `json:"content,omitempty"`
	Details map[string]any       `json:"details,omitempty"`
	IsError *bool                `json:"isError,omitempty"`
}

type SessionBeforeSwitchEventResult struct {
	Cancel bool `json:"cancel,omitempty"`
}

type SessionBeforeForkEventResult struct {
	Cancel                  bool `json:"cancel,omitempty"`
	SkipConversationRestore bool `json:"skipConversationRestore,omitempty"`
}

type SessionBeforeTreeSummary struct {
	Summary string         `json:"summary,omitempty"`
	Details map[string]any `json:"details,omitempty"`
}

type SessionBeforeTreeEventResult struct {
	Cancel              bool                      `json:"cancel,omitempty"`
	Summary             *SessionBeforeTreeSummary `json:"summary,omitempty"`
	CustomInstructions  string                    `json:"customInstructions,omitempty"`
	ReplaceInstructions *bool                     `json:"replaceInstructions,omitempty"`
	Label               string                    `json:"label,omitempty"`
}

type EmitResponse struct {
	Input               *InputEventResult               `json:"input,omitempty"`
	BeforeAgentStart    *BeforeAgentStartEventResult    `json:"beforeAgentStart,omitempty"`
	Context             *ContextEventResult             `json:"context,omitempty"`
	ToolCall            *ToolCallEventResult            `json:"toolCall,omitempty"`
	ToolResult          *ToolResultEventResult          `json:"toolResult,omitempty"`
	SessionBeforeSwitch *SessionBeforeSwitchEventResult `json:"sessionBeforeSwitch,omitempty"`
	SessionBeforeFork   *SessionBeforeForkEventResult   `json:"sessionBeforeFork,omitempty"`
	SessionBeforeTree   *SessionBeforeTreeEventResult   `json:"sessionBeforeTree,omitempty"`
	Actions             []HostAction                    `json:"actions,omitempty"`
}

type ExecuteToolRequest struct {
	Name       string                 `json:"name"`
	ToolCallID string                 `json:"toolCallID"`
	Arguments  map[string]interface{} `json:"arguments"`
}

type ExecuteCommandRequest struct {
	Name string `json:"name"`
	Args string `json:"args,omitempty"`
}

type ExecuteCommandResponse struct {
	Handled bool         `json:"handled"`
	Output  string       `json:"output,omitempty"`
	Actions []HostAction `json:"actions,omitempty"`
}

type HostAction struct {
	Type                string               `json:"type"`
	Text                string               `json:"text,omitempty"`
	Role                string               `json:"role,omitempty"`
	DeliverAs           string               `json:"deliverAs,omitempty"`
	TriggerTurn         bool                 `json:"triggerTurn,omitempty"`
	Provider            string               `json:"provider,omitempty"`
	Model               string               `json:"model,omitempty"`
	ThinkingLevel       string               `json:"thinkingLevel,omitempty"`
	Name                string               `json:"name,omitempty"`
	TargetID            string               `json:"targetId,omitempty"`
	Label               string               `json:"label,omitempty"`
	CustomType          string               `json:"customType,omitempty"`
	Data                map[string]any       `json:"data,omitempty"`
	Content             []types.ContentBlock `json:"content,omitempty"`
	Display             bool                 `json:"display,omitempty"`
	ToolNames           []string             `json:"toolNames,omitempty"`
	SessionPath         string               `json:"sessionPath,omitempty"`
	EntryID             string               `json:"entryId,omitempty"`
	ParentSession       string               `json:"parentSession,omitempty"`
	Summarize           bool                 `json:"summarize,omitempty"`
	CustomInstructions  string               `json:"customInstructions,omitempty"`
	ReplaceInstructions bool                 `json:"replaceInstructions,omitempty"`
}
