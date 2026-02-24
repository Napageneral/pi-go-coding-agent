package main

import (
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/agent"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/extensionsidecar"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/session"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

func TestRPCHandlePromptStartsAsync(t *testing.T) {
	rt := &fakeRPCRuntime{
		model:      types.Model{Provider: "openai", ID: "gpt-test"},
		promptDone: make(chan struct{}, 1),
	}
	server := &rpcServer{
		runtime: rt,
		out:     io.Discard,
	}

	resp := server.handleCommand(rpcCommand{
		ID:      "p1",
		Type:    "prompt",
		Message: "hello",
	})
	if !responseSuccess(resp) || responseCommand(resp) != "prompt" {
		t.Fatalf("unexpected prompt response: %#v", resp)
	}

	select {
	case <-rt.promptDone:
	case <-time.After(1 * time.Second):
		t.Fatal("prompt was not executed asynchronously")
	}
}

func TestRPCHandlePromptWhileStreamingRequiresBehavior(t *testing.T) {
	rt := &fakeRPCRuntime{model: types.Model{Provider: "openai", ID: "gpt-test"}}
	server := &rpcServer{
		runtime:       rt,
		out:           io.Discard,
		promptRunning: true,
	}

	resp := server.handleCommand(rpcCommand{
		Type:    "prompt",
		Message: "follow this up",
	})
	if responseSuccess(resp) {
		t.Fatalf("expected error response, got %#v", resp)
	}
}

func TestRPCHandlePromptWhileStreamingQueuesSteer(t *testing.T) {
	rt := &fakeRPCRuntime{model: types.Model{Provider: "openai", ID: "gpt-test"}}
	server := &rpcServer{
		runtime:       rt,
		out:           io.Discard,
		promptRunning: true,
	}

	resp := server.handleCommand(rpcCommand{
		Type:              "prompt",
		Message:           "switch direction",
		StreamingBehavior: "steer",
	})
	if !responseSuccess(resp) {
		t.Fatalf("expected success response, got %#v", resp)
	}
	if len(rt.steered) != 1 {
		t.Fatalf("expected queued steer message, got %#v", rt.steered)
	}
}

func TestRPCHandleGetState(t *testing.T) {
	rt := &fakeRPCRuntime{
		model:       types.Model{Provider: "openai", ID: "gpt-test"},
		thinking:    "medium",
		steering:    "all",
		followUp:    "one-at-a-time",
		sessionFile: "/tmp/session.jsonl",
		sessionID:   "sid-1",
		sessionName: "rpc-test",
		messages: []types.Message{
			types.TextMessage(types.RoleUser, "hi"),
			types.TextMessage(types.RoleAssistant, "hello"),
		},
		pending: 2,
	}
	server := &rpcServer{
		runtime: rt,
		out:     io.Discard,
	}

	resp := server.handleCommand(rpcCommand{Type: "get_state"})
	if !responseSuccess(resp) {
		t.Fatalf("expected success response, got %#v", resp)
	}
	data := responseData(resp)
	if data["sessionId"] != "sid-1" {
		t.Fatalf("sessionId = %#v, want sid-1", data["sessionId"])
	}
	if data["pendingMessageCount"] != 2 {
		t.Fatalf("pendingMessageCount = %#v, want 2", data["pendingMessageCount"])
	}
}

func TestRPCHandleSetModel(t *testing.T) {
	rt := &fakeRPCRuntime{model: types.Model{Provider: "openai", ID: "gpt-test"}}
	server := &rpcServer{
		runtime: rt,
		out:     io.Discard,
	}

	resp := server.handleCommand(rpcCommand{
		Type:     "set_model",
		Provider: "anthropic",
		ModelID:  "claude-test",
	})
	if !responseSuccess(resp) {
		t.Fatalf("expected success response, got %#v", resp)
	}
	if rt.model.Provider != "anthropic" || rt.model.ID != "claude-test" {
		t.Fatalf("model = %#v, want anthropic/claude-test", rt.model)
	}
}

func TestRPCHandleCompact(t *testing.T) {
	rt := &fakeRPCRuntime{
		model: types.Model{Provider: "openai", ID: "gpt-test"},
		compactEntry: session.Entry{
			Summary:        "summary",
			FirstKeptEntry: "entry-7",
			TokensBefore:   1234,
			Details:        map[string]any{"x": "y"},
		},
	}
	server := &rpcServer{
		runtime: rt,
		out:     io.Discard,
	}

	resp := server.handleCommand(rpcCommand{
		ID:                 "c1",
		Type:               "compact",
		CustomInstructions: "keep code context",
	})
	if !responseSuccess(resp) {
		t.Fatalf("expected success response, got %#v", resp)
	}
	data := responseData(resp)
	if data["summary"] != "summary" {
		t.Fatalf("summary = %#v, want summary", data["summary"])
	}
}

func TestRPCHandleBashReturnsStructuredResult(t *testing.T) {
	rt := &fakeRPCRuntime{
		model: types.Model{Provider: "openai", ID: "gpt-test"},
		bashResult: types.ToolResult{
			Content: []types.ContentBlock{{Type: "text", Text: "ok"}},
			Details: map[string]any{"exitCode": 7, "truncated": false, "cancelled": false},
			IsError: true,
		},
		bashErr: errors.New("exit status 7"),
	}
	server := &rpcServer{
		runtime: rt,
		out:     io.Discard,
	}

	resp := server.handleCommand(rpcCommand{
		Type:    "bash",
		Command: "false",
	})
	if !responseSuccess(resp) {
		t.Fatalf("expected success response, got %#v", resp)
	}
	data := responseData(resp)
	if data["exitCode"] != 7 {
		t.Fatalf("exitCode = %#v, want 7", data["exitCode"])
	}
}

func TestRPCHandleGetLastAssistantTextNull(t *testing.T) {
	rt := &fakeRPCRuntime{model: types.Model{Provider: "openai", ID: "gpt-test"}}
	server := &rpcServer{
		runtime: rt,
		out:     io.Discard,
	}

	resp := server.handleCommand(rpcCommand{Type: "get_last_assistant_text"})
	if !responseSuccess(resp) {
		t.Fatalf("expected success response, got %#v", resp)
	}
	data := responseData(resp)
	if value, ok := data["text"]; !ok || value != nil {
		t.Fatalf("text = %#v, want nil", value)
	}
}

func TestRPCHandleGetCommandsReturnsRuntimeCatalog(t *testing.T) {
	rt := &fakeRPCRuntime{
		model: types.Model{Provider: "openai", ID: "gpt-test"},
		commands: []agent.Command{
			{Name: "ping", Source: "extension"},
			{Name: "template", Source: "prompt", Location: "project"},
			{Name: "skill:lint", Source: "skill", Location: "user"},
		},
	}
	server := &rpcServer{
		runtime: rt,
		out:     io.Discard,
	}

	resp := server.handleCommand(rpcCommand{Type: "get_commands"})
	if !responseSuccess(resp) {
		t.Fatalf("expected success response, got %#v", resp)
	}
	data := responseData(resp)
	commands, ok := data["commands"].([]agent.Command)
	if !ok {
		t.Fatalf("commands type = %T, want []agent.Command", data["commands"])
	}
	if len(commands) != 3 {
		t.Fatalf("commands len = %d, want 3", len(commands))
	}
}

func TestRPCHandleAutoCompactionSettingsUsesRuntime(t *testing.T) {
	rt := &fakeRPCRuntime{model: types.Model{Provider: "openai", ID: "gpt-test"}}
	server := &rpcServer{
		runtime: rt,
		out:     io.Discard,
	}

	resp := server.handleCommand(rpcCommand{Type: "set_auto_compaction", Enabled: true})
	if !responseSuccess(resp) {
		t.Fatalf("set_auto_compaction response = %#v", resp)
	}
	if !rt.autoCompaction {
		t.Fatalf("runtime autoCompaction = false, want true")
	}
	resp = server.handleCommand(rpcCommand{Type: "set_auto_retry", Enabled: true})
	if !responseSuccess(resp) {
		t.Fatalf("set_auto_retry response = %#v", resp)
	}
	if !rt.autoRetry {
		t.Fatalf("runtime autoRetry = false, want true")
	}
}

func TestRPCHandleExportHTML(t *testing.T) {
	rt := &fakeRPCRuntime{
		model:      types.Model{Provider: "openai", ID: "gpt-test"},
		exportPath: "/tmp/pi-session-export.html",
	}
	server := &rpcServer{
		runtime: rt,
		out:     io.Discard,
	}

	resp := server.handleCommand(rpcCommand{Type: "export_html"})
	if !responseSuccess(resp) {
		t.Fatalf("expected success response, got %#v", resp)
	}
	data := responseData(resp)
	if data["path"] != "/tmp/pi-session-export.html" {
		t.Fatalf("path = %#v, want /tmp/pi-session-export.html", data["path"])
	}
}

func TestRPCRunHandlesExtensionUIResponse(t *testing.T) {
	rt := &fakeRPCRuntime{
		model: types.Model{Provider: "openai", ID: "gpt-test"},
	}
	server := &rpcServer{
		runtime: rt,
		in:      strings.NewReader("{\"type\":\"extension_ui_response\",\"id\":\"ui-1\",\"confirmed\":true}\n"),
		out:     io.Discard,
	}
	if err := server.run(); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(rt.extensionUIResponses) != 1 {
		t.Fatalf("expected one extension ui response, got %d", len(rt.extensionUIResponses))
	}
	response := rt.extensionUIResponses[0]
	if response.ID != "ui-1" {
		t.Fatalf("response id = %q, want ui-1", response.ID)
	}
	if response.Confirmed == nil || !*response.Confirmed {
		t.Fatalf("response confirmed = %#v, want true", response.Confirmed)
	}
}

func TestRPCHandleUnknownCommand(t *testing.T) {
	rt := &fakeRPCRuntime{model: types.Model{Provider: "openai", ID: "gpt-test"}}
	server := &rpcServer{
		runtime: rt,
		out:     io.Discard,
	}

	resp := server.handleCommand(rpcCommand{Type: "missing"})
	if responseSuccess(resp) {
		t.Fatalf("expected error response, got %#v", resp)
	}
}

func responseSuccess(response map[string]any) bool {
	success, _ := response["success"].(bool)
	return success
}

func responseCommand(response map[string]any) string {
	command, _ := response["command"].(string)
	return command
}

func responseData(response map[string]any) map[string]any {
	data, _ := response["data"].(map[string]any)
	return data
}

type fakeRPCRuntime struct {
	model           types.Model
	available       []types.Model
	thinking        string
	steering        string
	followUp        string
	streaming       bool
	pending         int
	sessionFile     string
	sessionID       string
	sessionName     string
	messages        []types.Message
	forkMessages    []agent.ForkMessage
	commands        []agent.Command
	lastText        string
	hasLastText     bool
	sessionStats    agent.SessionStats
	compactEntry    session.Entry
	compactErr      error
	bashResult      types.ToolResult
	bashErr         error
	newCancelled    bool
	switchCancelled bool
	forkCancelled   bool
	forkText        string
	autoCompaction  bool
	autoRetry       bool
	exportPath      string
	exportErr       error

	extensionUIResponses []extensionsidecar.ExtensionUIResponse

	steered []types.Message

	promptDone chan struct{}
}

func (f *fakeRPCRuntime) SubscribeEvents(func(agent.RuntimeEvent)) func() { return func() {} }

func (f *fakeRPCRuntime) Prompt(text string) (types.Message, error) {
	if f.promptDone != nil {
		select {
		case f.promptDone <- struct{}{}:
		default:
		}
	}
	msg := types.TextMessage(types.RoleAssistant, "ok")
	return msg, nil
}

func (f *fakeRPCRuntime) PromptMessage(types.Message) (types.Message, error) {
	if f.promptDone != nil {
		select {
		case f.promptDone <- struct{}{}:
		default:
		}
	}
	msg := types.TextMessage(types.RoleAssistant, "ok")
	return msg, nil
}

func (f *fakeRPCRuntime) Steer(text string) error {
	f.steered = append(f.steered, types.TextMessage(types.RoleUser, text))
	return nil
}

func (f *fakeRPCRuntime) SteerMessage(message types.Message) error {
	f.steered = append(f.steered, message)
	return nil
}

func (f *fakeRPCRuntime) FollowUp(text string) error {
	return nil
}

func (f *fakeRPCRuntime) FollowUpMessage(types.Message) error {
	return nil
}

func (f *fakeRPCRuntime) Abort() {}

func (f *fakeRPCRuntime) NewSession(string, []map[string]any) (bool, error) {
	return f.newCancelled, nil
}
func (f *fakeRPCRuntime) SwitchSession(string) (bool, error) { return f.switchCancelled, nil }
func (f *fakeRPCRuntime) ForkSession(string) (string, bool, error) {
	return f.forkText, f.forkCancelled, nil
}

func (f *fakeRPCRuntime) Compact(string, string) (session.Entry, error) {
	if f.compactErr != nil {
		return session.Entry{}, f.compactErr
	}
	return f.compactEntry, nil
}

func (f *fakeRPCRuntime) SetModel(provider, modelID string) error {
	f.model.Provider = provider
	f.model.ID = modelID
	return nil
}

func (f *fakeRPCRuntime) CycleModel() (*types.Model, bool, error) {
	if len(f.available) == 0 {
		return nil, false, nil
	}
	next := f.available[0]
	f.model = next
	return &next, false, nil
}

func (f *fakeRPCRuntime) AvailableModels() []types.Model { return f.available }
func (f *fakeRPCRuntime) Model() types.Model             { return f.model }

func (f *fakeRPCRuntime) SetThinkingLevel(level string) error {
	f.thinking = level
	return nil
}

func (f *fakeRPCRuntime) CycleThinkingLevel() (string, bool, error) {
	return "", false, nil
}

func (f *fakeRPCRuntime) ThinkingLevel() string { return f.thinking }

func (f *fakeRPCRuntime) SetSteeringMode(mode string) error {
	f.steering = mode
	return nil
}

func (f *fakeRPCRuntime) SetFollowUpMode(mode string) error {
	f.followUp = mode
	return nil
}

func (f *fakeRPCRuntime) SteeringMode() string { return f.steering }
func (f *fakeRPCRuntime) FollowUpMode() string { return f.followUp }
func (f *fakeRPCRuntime) IsStreaming() bool    { return f.streaming }

func (f *fakeRPCRuntime) PendingMessageCount() int { return f.pending }

func (f *fakeRPCRuntime) SessionFile() string { return f.sessionFile }
func (f *fakeRPCRuntime) SessionID() string   { return f.sessionID }
func (f *fakeRPCRuntime) SessionName() string { return f.sessionName }
func (f *fakeRPCRuntime) SetSessionName(name string) error {
	f.sessionName = name
	return nil
}

func (f *fakeRPCRuntime) Messages() []types.Message { return f.messages }
func (f *fakeRPCRuntime) ForkMessages() []agent.ForkMessage {
	return f.forkMessages
}
func (f *fakeRPCRuntime) LastAssistantText() (string, bool) { return f.lastText, f.hasLastText }
func (f *fakeRPCRuntime) SessionStats() agent.SessionStats  { return f.sessionStats }
func (f *fakeRPCRuntime) Commands() []agent.Command         { return f.commands }

func (f *fakeRPCRuntime) ExecuteBash(string) (types.ToolResult, error) {
	return f.bashResult, f.bashErr
}
func (f *fakeRPCRuntime) AbortBash() {}

func (f *fakeRPCRuntime) SetAutoCompactionEnabled(enabled bool) { f.autoCompaction = enabled }
func (f *fakeRPCRuntime) AutoCompactionEnabled() bool           { return f.autoCompaction }
func (f *fakeRPCRuntime) SetAutoRetryEnabled(enabled bool)      { f.autoRetry = enabled }
func (f *fakeRPCRuntime) AutoRetryEnabled() bool                { return f.autoRetry }
func (f *fakeRPCRuntime) AbortRetry()                           {}
func (f *fakeRPCRuntime) RespondExtensionUI(response extensionsidecar.ExtensionUIResponse) error {
	f.extensionUIResponses = append(f.extensionUIResponses, response)
	return nil
}
func (f *fakeRPCRuntime) ExportHTML(_ string) (string, error) {
	if f.exportErr != nil {
		return "", f.exportErr
	}
	if f.exportPath == "" {
		return "/tmp/pi-session-export.html", nil
	}
	return f.exportPath, nil
}
