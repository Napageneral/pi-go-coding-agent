package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/agent"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/extensionsidecar"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/session"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

type rpcRuntime interface {
	SubscribeEvents(func(agent.RuntimeEvent)) func()

	Prompt(text string) (types.Message, error)
	PromptMessage(message types.Message) (types.Message, error)
	Steer(text string) error
	SteerMessage(message types.Message) error
	FollowUp(text string) error
	FollowUpMessage(message types.Message) error
	Abort()

	NewSession(parentSession string, setupEntries []map[string]any) (bool, error)
	SwitchSession(path string) (bool, error)
	ForkSession(entryID string) (string, bool, error)
	Compact(customInstructions string, requestID string) (session.Entry, error)

	SetModel(provider, modelID string) error
	CycleModel() (*types.Model, bool, error)
	AvailableModels() []types.Model
	Model() types.Model

	SetThinkingLevel(level string) error
	CycleThinkingLevel() (string, bool, error)
	ThinkingLevel() string

	SetSteeringMode(mode string) error
	SetFollowUpMode(mode string) error
	SteeringMode() string
	FollowUpMode() string

	IsStreaming() bool
	PendingMessageCount() int

	SessionFile() string
	SessionID() string
	SessionName() string
	SetSessionName(name string) error

	Messages() []types.Message
	ForkMessages() []agent.ForkMessage
	LastAssistantText() (string, bool)
	SessionStats() agent.SessionStats
	Commands() []agent.Command

	ExecuteBash(command string) (types.ToolResult, error)
	AbortBash()

	SetAutoCompactionEnabled(enabled bool)
	AutoCompactionEnabled() bool
	SetAutoRetryEnabled(enabled bool)
	AutoRetryEnabled() bool
	AbortRetry()

	RespondExtensionUI(response extensionsidecar.ExtensionUIResponse) error
	ExportHTML(outputPath string) (string, error)
}

var _ rpcRuntime = (*agent.Runtime)(nil)

type rpcServer struct {
	runtime rpcRuntime
	in      io.Reader
	out     io.Writer

	writeMu sync.Mutex
	stateMu sync.Mutex

	promptRunning bool
	compacting    bool
}

type rpcCommand struct {
	ID                string     `json:"id"`
	Type              string     `json:"type"`
	Message           string     `json:"message"`
	Images            []rpcImage `json:"images"`
	StreamingBehavior string     `json:"streamingBehavior"`

	ParentSession      string `json:"parentSession"`
	Provider           string `json:"provider"`
	ModelID            string `json:"modelId"`
	Level              string `json:"level"`
	Mode               string `json:"mode"`
	CustomInstructions string `json:"customInstructions"`
	Enabled            bool   `json:"enabled"`
	Command            string `json:"command"`
	OutputPath         string `json:"outputPath"`
	SessionPath        string `json:"sessionPath"`
	EntryID            string `json:"entryId"`
	Name               string `json:"name"`
	Value              string `json:"value"`
	Confirmed          *bool  `json:"confirmed"`
	Cancelled          bool   `json:"cancelled"`
}

type rpcImage struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

func runRPCMode(in io.Reader, out io.Writer, rt rpcRuntime) error {
	server := &rpcServer{
		runtime: rt,
		in:      in,
		out:     out,
	}
	return server.run()
}

func (s *rpcServer) run() error {
	unsubscribe := s.runtime.SubscribeEvents(func(event agent.RuntimeEvent) {
		s.emit(runtimeEventToRPC(event))
	})
	defer unsubscribe()

	scanner := bufio.NewScanner(s.in)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var command rpcCommand
		if err := json.Unmarshal([]byte(line), &command); err != nil {
			s.emit(errorResponse("", "parse", fmt.Sprintf("Failed to parse command: %v", err)))
			continue
		}
		if strings.TrimSpace(command.Type) == "" {
			s.emit(errorResponse(command.ID, "parse", "Missing command type"))
			continue
		}
		if strings.EqualFold(strings.TrimSpace(command.Type), "extension_ui_response") {
			_ = s.runtime.RespondExtensionUI(extensionsidecar.ExtensionUIResponse{
				ID:        strings.TrimSpace(command.ID),
				Value:     command.Value,
				Confirmed: command.Confirmed,
				Cancelled: command.Cancelled,
			})
			continue
		}
		response := s.handleCommand(command)
		if response != nil {
			s.emit(response)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func (s *rpcServer) handleCommand(command rpcCommand) map[string]any {
	id := strings.TrimSpace(command.ID)
	commandType := strings.ToLower(strings.TrimSpace(command.Type))

	switch commandType {
	case "prompt":
		message, err := rpcCommandToUserMessage(command.Message, command.Images)
		if err != nil {
			return errorResponse(id, "prompt", err.Error())
		}
		if s.isStreaming() {
			behavior := normalizeStreamingBehavior(command.StreamingBehavior)
			if behavior == "" {
				return errorResponse(id, "prompt", "Agent is streaming; set streamingBehavior to steer or followUp")
			}
			if err := s.queueMessage(behavior, message); err != nil {
				return errorResponse(id, "prompt", err.Error())
			}
			return successResponse(id, "prompt", false, nil)
		}
		if !s.beginPrompt() {
			return errorResponse(id, "prompt", "Agent is already handling a prompt")
		}
		if len(message.Content) == 1 && message.Content[0].Type == "text" {
			prompt := message.Content[0].Text
			go s.runPromptAsync(id, func() (types.Message, error) {
				return s.runtime.Prompt(prompt)
			})
		} else {
			go s.runPromptAsync(id, func() (types.Message, error) {
				return s.runtime.PromptMessage(message)
			})
		}
		return successResponse(id, "prompt", false, nil)

	case "steer":
		message, err := rpcCommandToUserMessage(command.Message, command.Images)
		if err != nil {
			return errorResponse(id, "steer", err.Error())
		}
		if isSlashCommandMessage(message) {
			return errorResponse(id, "steer", "steer does not support extension commands; use prompt")
		}
		if err := s.queueMessage(queueBehaviorSteer, message); err != nil {
			return errorResponse(id, "steer", err.Error())
		}
		return successResponse(id, "steer", false, nil)

	case "follow_up":
		message, err := rpcCommandToUserMessage(command.Message, command.Images)
		if err != nil {
			return errorResponse(id, "follow_up", err.Error())
		}
		if isSlashCommandMessage(message) {
			return errorResponse(id, "follow_up", "follow_up does not support extension commands; use prompt")
		}
		if err := s.queueMessage(queueBehaviorFollowUp, message); err != nil {
			return errorResponse(id, "follow_up", err.Error())
		}
		return successResponse(id, "follow_up", false, nil)

	case "abort":
		s.runtime.Abort()
		return successResponse(id, "abort", false, nil)

	case "new_session":
		if s.isStreaming() {
			return errorResponse(id, "new_session", "cannot create a new session while streaming")
		}
		cancelled, err := s.runtime.NewSession(command.ParentSession, nil)
		if err != nil {
			return errorResponse(id, "new_session", err.Error())
		}
		return successResponse(id, "new_session", true, map[string]any{"cancelled": cancelled})

	case "get_state":
		messageCount := len(s.runtime.Messages())
		sessionName := strings.TrimSpace(s.runtime.SessionName())
		state := map[string]any{
			"model":                 s.runtime.Model(),
			"thinkingLevel":         s.runtime.ThinkingLevel(),
			"isStreaming":           s.isStreaming(),
			"isCompacting":          s.isCompacting(),
			"steeringMode":          s.runtime.SteeringMode(),
			"followUpMode":          s.runtime.FollowUpMode(),
			"sessionFile":           s.runtime.SessionFile(),
			"sessionId":             s.runtime.SessionID(),
			"autoCompactionEnabled": s.runtime.AutoCompactionEnabled(),
			"messageCount":          messageCount,
			"pendingMessageCount":   s.runtime.PendingMessageCount(),
		}
		if sessionName != "" {
			state["sessionName"] = sessionName
		}
		return successResponse(id, "get_state", true, state)

	case "set_model":
		if s.isStreaming() {
			return errorResponse(id, "set_model", "cannot set model while streaming")
		}
		if strings.TrimSpace(command.Provider) == "" || strings.TrimSpace(command.ModelID) == "" {
			return errorResponse(id, "set_model", "provider and modelId are required")
		}
		if err := s.runtime.SetModel(command.Provider, command.ModelID); err != nil {
			return errorResponse(id, "set_model", err.Error())
		}
		return successResponse(id, "set_model", true, s.runtime.Model())

	case "cycle_model":
		if s.isStreaming() {
			return errorResponse(id, "cycle_model", "cannot cycle model while streaming")
		}
		model, isScoped, err := s.runtime.CycleModel()
		if err != nil {
			return errorResponse(id, "cycle_model", err.Error())
		}
		if model == nil {
			return successResponse(id, "cycle_model", true, nil)
		}
		return successResponse(id, "cycle_model", true, map[string]any{
			"model":         *model,
			"thinkingLevel": s.runtime.ThinkingLevel(),
			"isScoped":      isScoped,
		})

	case "get_available_models":
		return successResponse(id, "get_available_models", true, map[string]any{
			"models": s.runtime.AvailableModels(),
		})

	case "set_thinking_level":
		if s.isStreaming() {
			return errorResponse(id, "set_thinking_level", "cannot set thinking level while streaming")
		}
		if err := s.runtime.SetThinkingLevel(command.Level); err != nil {
			return errorResponse(id, "set_thinking_level", err.Error())
		}
		return successResponse(id, "set_thinking_level", false, nil)

	case "cycle_thinking_level":
		if s.isStreaming() {
			return errorResponse(id, "cycle_thinking_level", "cannot cycle thinking level while streaming")
		}
		level, ok, err := s.runtime.CycleThinkingLevel()
		if err != nil {
			return errorResponse(id, "cycle_thinking_level", err.Error())
		}
		if !ok {
			return successResponse(id, "cycle_thinking_level", true, nil)
		}
		return successResponse(id, "cycle_thinking_level", true, map[string]any{"level": level})

	case "set_steering_mode":
		if err := s.runtime.SetSteeringMode(command.Mode); err != nil {
			return errorResponse(id, "set_steering_mode", err.Error())
		}
		return successResponse(id, "set_steering_mode", false, nil)

	case "set_follow_up_mode":
		if err := s.runtime.SetFollowUpMode(command.Mode); err != nil {
			return errorResponse(id, "set_follow_up_mode", err.Error())
		}
		return successResponse(id, "set_follow_up_mode", false, nil)

	case "compact":
		if s.isStreaming() {
			return errorResponse(id, "compact", "cannot compact while streaming")
		}
		s.setCompacting(true)
		entry, err := s.runtime.Compact(command.CustomInstructions, id)
		s.setCompacting(false)
		if err != nil {
			return errorResponse(id, "compact", err.Error())
		}
		return successResponse(id, "compact", true, map[string]any{
			"summary":          entry.Summary,
			"firstKeptEntryId": entry.FirstKeptEntry,
			"tokensBefore":     entry.TokensBefore,
			"details":          entry.Details,
		})

	case "set_auto_compaction":
		s.runtime.SetAutoCompactionEnabled(command.Enabled)
		return successResponse(id, "set_auto_compaction", false, nil)

	case "set_auto_retry":
		s.runtime.SetAutoRetryEnabled(command.Enabled)
		return successResponse(id, "set_auto_retry", false, nil)

	case "abort_retry":
		s.runtime.AbortRetry()
		return successResponse(id, "abort_retry", false, nil)

	case "bash":
		result, err := s.runtime.ExecuteBash(command.Command)
		if err != nil && len(result.Content) == 0 {
			return errorResponse(id, "bash", err.Error())
		}
		output := contentBlocksText(result.Content)
		exitCode := intValue(result.Details["exitCode"], 0)
		cancelled := boolValue(result.Details["cancelled"])
		if cancelled && exitCode == 0 {
			exitCode = 130
		}
		if err != nil && !cancelled && exitCode == 0 {
			exitCode = 1
		}
		return successResponse(id, "bash", true, map[string]any{
			"output":    output,
			"exitCode":  exitCode,
			"cancelled": cancelled,
			"truncated": boolValue(result.Details["truncated"]),
		})

	case "abort_bash":
		s.runtime.AbortBash()
		return successResponse(id, "abort_bash", false, nil)

	case "get_session_stats":
		return successResponse(id, "get_session_stats", true, s.runtime.SessionStats())

	case "export_html":
		path, err := s.runtime.ExportHTML(command.OutputPath)
		if err != nil {
			return errorResponse(id, "export_html", err.Error())
		}
		return successResponse(id, "export_html", true, map[string]any{"path": path})

	case "switch_session":
		if s.isStreaming() {
			return errorResponse(id, "switch_session", "cannot switch sessions while streaming")
		}
		cancelled, err := s.runtime.SwitchSession(command.SessionPath)
		if err != nil {
			return errorResponse(id, "switch_session", err.Error())
		}
		return successResponse(id, "switch_session", true, map[string]any{"cancelled": cancelled})

	case "fork":
		if s.isStreaming() {
			return errorResponse(id, "fork", "cannot fork while streaming")
		}
		text, cancelled, err := s.runtime.ForkSession(command.EntryID)
		if err != nil {
			return errorResponse(id, "fork", err.Error())
		}
		return successResponse(id, "fork", true, map[string]any{
			"text":      text,
			"cancelled": cancelled,
		})

	case "get_fork_messages":
		return successResponse(id, "get_fork_messages", true, map[string]any{
			"messages": s.runtime.ForkMessages(),
		})

	case "get_last_assistant_text":
		text, ok := s.runtime.LastAssistantText()
		if !ok {
			return successResponse(id, "get_last_assistant_text", true, map[string]any{"text": nil})
		}
		return successResponse(id, "get_last_assistant_text", true, map[string]any{"text": text})

	case "set_session_name":
		if err := s.runtime.SetSessionName(command.Name); err != nil {
			return errorResponse(id, "set_session_name", err.Error())
		}
		return successResponse(id, "set_session_name", false, nil)

	case "get_messages":
		return successResponse(id, "get_messages", true, map[string]any{
			"messages": s.runtime.Messages(),
		})

	case "get_commands":
		return successResponse(id, "get_commands", true, map[string]any{"commands": s.runtime.Commands()})
	}

	return errorResponse(id, commandType, fmt.Sprintf("Unknown command: %s", commandType))
}

func (s *rpcServer) runPromptAsync(id string, run func() (types.Message, error)) {
	defer s.endPrompt()
	if _, err := run(); err != nil {
		if errors.Is(err, agent.ErrAborted) {
			return
		}
		s.emit(errorResponse(id, "prompt", err.Error()))
	}
}

func (s *rpcServer) queueMessage(behavior string, message types.Message) error {
	switch behavior {
	case queueBehaviorSteer:
		if len(message.Content) == 1 && message.Content[0].Type == "text" {
			return s.runtime.Steer(message.Content[0].Text)
		}
		return s.runtime.SteerMessage(message)
	case queueBehaviorFollowUp:
		if len(message.Content) == 1 && message.Content[0].Type == "text" {
			return s.runtime.FollowUp(message.Content[0].Text)
		}
		return s.runtime.FollowUpMessage(message)
	default:
		return fmt.Errorf("unsupported queue behavior: %s", behavior)
	}
}

func (s *rpcServer) emit(v any) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	enc := json.NewEncoder(s.out)
	_ = enc.Encode(v)
}

func (s *rpcServer) beginPrompt() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.promptRunning {
		return false
	}
	s.promptRunning = true
	return true
}

func (s *rpcServer) endPrompt() {
	s.stateMu.Lock()
	s.promptRunning = false
	s.stateMu.Unlock()
}

func (s *rpcServer) isStreaming() bool {
	s.stateMu.Lock()
	running := s.promptRunning
	s.stateMu.Unlock()
	if running {
		return true
	}
	return s.runtime.IsStreaming()
}

func (s *rpcServer) setCompacting(value bool) {
	s.stateMu.Lock()
	s.compacting = value
	s.stateMu.Unlock()
}

func (s *rpcServer) isCompacting() bool {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.compacting
}

func successResponse(id, command string, includeData bool, data any) map[string]any {
	response := map[string]any{
		"type":    "response",
		"command": command,
		"success": true,
	}
	if id != "" {
		response["id"] = id
	}
	if includeData {
		response["data"] = data
	}
	return response
}

func errorResponse(id, command, message string) map[string]any {
	response := map[string]any{
		"type":    "response",
		"command": command,
		"success": false,
		"error":   message,
	}
	if id != "" {
		response["id"] = id
	}
	return response
}

func runtimeEventToRPC(event agent.RuntimeEvent) map[string]any {
	payload := map[string]any{
		"type": event.Type,
	}
	for key, value := range event.Payload {
		if key == "type" {
			continue
		}
		payload[key] = value
	}
	return payload
}

func rpcCommandToUserMessage(text string, images []rpcImage) (types.Message, error) {
	content := make([]types.ContentBlock, 0, 1+len(images))
	text = strings.TrimSpace(text)
	if text != "" {
		content = append(content, types.ContentBlock{
			Type: "text",
			Text: text,
		})
	}
	for _, image := range images {
		if strings.TrimSpace(image.Data) == "" {
			continue
		}
		mimeType := strings.TrimSpace(image.MimeType)
		if mimeType == "" {
			mimeType = "image/png"
		}
		content = append(content, types.ContentBlock{
			Type:     "image",
			Data:     image.Data,
			MimeType: mimeType,
		})
	}
	if len(content) == 0 {
		return types.Message{}, errors.New("message is empty")
	}
	return types.Message{
		Role:      types.RoleUser,
		Timestamp: types.NowMillis(),
		Content:   content,
	}, nil
}

func normalizeStreamingBehavior(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case queueBehaviorSteer:
		return queueBehaviorSteer
	case "followup", "follow_up", "follow-up":
		return queueBehaviorFollowUp
	default:
		return ""
	}
}

func isSlashCommandMessage(message types.Message) bool {
	if len(message.Content) != 1 || message.Content[0].Type != "text" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(message.Content[0].Text), "/")
}

func contentBlocksText(content []types.ContentBlock) string {
	parts := make([]string, 0, len(content))
	for _, block := range content {
		if block.Type != "text" {
			continue
		}
		text := strings.TrimSpace(block.Text)
		if text == "" {
			continue
		}
		parts = append(parts, text)
	}
	return strings.Join(parts, "\n")
}

func intValue(raw any, fallback int) int {
	switch value := raw.(type) {
	case int:
		return value
	case int32:
		return int(value)
	case int64:
		return int(value)
	case float32:
		return int(value)
	case float64:
		return int(value)
	default:
		return fallback
	}
}

func boolValue(raw any) bool {
	value, _ := raw.(bool)
	return value
}

const queueBehaviorSteer = "steer"
const queueBehaviorFollowUp = "followUp"
