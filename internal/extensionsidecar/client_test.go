package extensionsidecar

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

func TestClientInitializeEmitExecuteAndClose(t *testing.T) {
	client, err := Start(Options{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestSidecarHelperProcess"},
		Env:     []string{"GO_WANT_SIDECAR_HELPER=1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = client.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	initResp, err := client.Initialize(ctx, InitializeRequest{
		ProtocolVersion: ProtocolVersion,
		CWD:             "/tmp/project",
		SessionID:       "s1",
		SessionFile:     "/tmp/project/session.jsonl",
		FlagValues: map[string]any{
			"helper_mode": "verbose",
		},
	})
	if err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	if initResp.ProtocolVersion != ProtocolVersion {
		t.Fatalf("protocol version = %q, want %q", initResp.ProtocolVersion, ProtocolVersion)
	}
	if len(initResp.Tools) != 1 || initResp.Tools[0].Name != "helper_echo" {
		t.Fatalf("unexpected tools: %#v", initResp.Tools)
	}
	if len(initResp.Flags) != 1 || initResp.Flags[0].Name != "helper_mode" {
		t.Fatalf("unexpected flags: %#v", initResp.Flags)
	}
	if len(initResp.Commands) != 2 || initResp.Commands[0].Name != "ping" {
		t.Fatalf("unexpected commands: %#v", initResp.Commands)
	}
	if len(initResp.Providers) != 1 || initResp.Providers[0].Name != "helper-provider" {
		t.Fatalf("unexpected providers: %#v", initResp.Providers)
	}

	emitResp, err := client.Emit(ctx, Event{
		Type: "input",
		Payload: map[string]any{
			"text": "hello",
		},
	})
	if err != nil {
		t.Fatalf("Emit(input): %v", err)
	}
	if emitResp.Input == nil || emitResp.Input.Action != "transform" || emitResp.Input.Text != "hello [helper]" {
		t.Fatalf("unexpected input emit response: %#v", emitResp.Input)
	}

	contextResp, err := client.Emit(ctx, Event{
		Type: "context",
		Payload: map[string]any{
			"systemPrompt": "base",
			"messages": []types.Message{
				types.TextMessage(types.RoleUser, "hi"),
			},
		},
	})
	if err != nil {
		t.Fatalf("Emit(context): %v", err)
	}
	if contextResp.Context == nil || contextResp.Context.SystemPrompt != "helper-context-prompt" {
		t.Fatalf("unexpected context emit response: %#v", contextResp.Context)
	}

	toolResult, err := client.ExecuteTool(ctx, "helper_echo", "c1", map[string]interface{}{
		"text": "tool input",
	})
	if err != nil {
		t.Fatalf("ExecuteTool(helper_echo): %v", err)
	}
	if len(toolResult.Content) == 0 || toolResult.Content[0].Text != "echo: tool input" {
		t.Fatalf("unexpected tool result: %#v", toolResult)
	}

	_, err = client.ExecuteTool(ctx, "missing_tool", "c2", nil)
	if !errors.Is(err, ErrToolNotFound) {
		t.Fatalf("expected ErrToolNotFound, got %v", err)
	}

	cmdResult, err := client.ExecuteCommand(ctx, "ping", "world")
	if err != nil {
		t.Fatalf("ExecuteCommand(ping): %v", err)
	}
	if !cmdResult.Handled || cmdResult.Output != "pong:world [verbose]" {
		t.Fatalf("unexpected command result: %#v", cmdResult)
	}

	cmdMissing, err := client.ExecuteCommand(ctx, "missing_cmd", "")
	if err != nil {
		t.Fatalf("ExecuteCommand(missing_cmd): %v", err)
	}
	if cmdMissing.Handled {
		t.Fatalf("expected missing command to be unhandled, got: %#v", cmdMissing)
	}
}

func TestClientExtensionUIRequestResponse(t *testing.T) {
	client, err := Start(Options{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestSidecarHelperProcess"},
		Env:     []string{"GO_WANT_SIDECAR_HELPER=1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		_ = client.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := client.Initialize(ctx, InitializeRequest{
		ProtocolVersion: ProtocolVersion,
		HasUI:           true,
	}); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	requests := make(chan ExtensionUIRequest, 1)
	client.SetExtensionUIRequestHandler(func(req ExtensionUIRequest) {
		select {
		case requests <- req:
		default:
		}
		_ = client.RespondExtensionUI(context.Background(), ExtensionUIResponse{
			ID:    req.ID,
			Value: "approved",
		})
	})

	result, err := client.ExecuteCommand(ctx, "ask", "")
	if err != nil {
		t.Fatalf("ExecuteCommand(ask): %v", err)
	}
	if !result.Handled || result.Output != "asked:approved" {
		t.Fatalf("unexpected ask command result: %#v", result)
	}

	select {
	case req := <-requests:
		if req.Method != "input" {
			t.Fatalf("ui request method = %q, want input", req.Method)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("expected extension ui request callback")
	}
}

func TestSidecarHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_SIDECAR_HELPER") != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	helperMode := "default"

	for scanner.Scan() {
		var req struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			var initReq InitializeRequest
			if err := json.Unmarshal(req.Params, &initReq); err != nil {
				writeError(t, encoder, req.ID, "invalid_request", err.Error())
				continue
			}
			mode := "default"
			if v, ok := initReq.FlagValues["helper_mode"].(string); ok && v != "" {
				mode = v
			}
			helperMode = mode
			writeResult(t, encoder, req.ID, InitializeResponse{
				ProtocolVersion: ProtocolVersion,
				SidecarVersion:  "test-helper",
				Tools: []types.Tool{
					{
						Name:        "helper_echo",
						Description: "Echo helper tool",
						Parameters: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"text": map[string]any{
									"type": "string",
								},
							},
						},
					},
				},
				Flags: []ExtensionFlagDefinition{
					{
						Name:        "helper_mode",
						Type:        "string",
						Default:     "default",
						Description: "helper mode",
					},
				},
				Commands: []ExtensionCommandDefinition{
					{
						Name:        "ping",
						Description: "Ping command",
					},
					{
						Name:        "ask",
						Description: "Ask for UI input",
					},
				},
				Providers: []ProviderRegistration{
					{
						Name: "helper-provider",
						Config: map[string]any{
							"api":     "openai-completions",
							"baseUrl": "https://example.test",
						},
					},
				},
			})
		case "emit":
			var emitReq EmitRequest
			if err := json.Unmarshal(req.Params, &emitReq); err != nil {
				writeError(t, encoder, req.ID, "invalid_request", err.Error())
				continue
			}
			switch emitReq.Event.Type {
			case "input":
				text, _ := emitReq.Event.Payload["text"].(string)
				writeResult(t, encoder, req.ID, EmitResponse{
					Input: &InputEventResult{
						Action: "transform",
						Text:   text + " [helper]",
					},
				})
			case "context":
				writeResult(t, encoder, req.ID, EmitResponse{
					Context: &ContextEventResult{
						SystemPrompt: "helper-context-prompt",
					},
				})
			default:
				writeResult(t, encoder, req.ID, EmitResponse{})
			}
		case "tool.execute":
			var toolReq ExecuteToolRequest
			if err := json.Unmarshal(req.Params, &toolReq); err != nil {
				writeError(t, encoder, req.ID, "invalid_request", err.Error())
				continue
			}
			if toolReq.Name != "helper_echo" {
				writeError(t, encoder, req.ID, "tool_not_found", "tool not found")
				continue
			}
			text, _ := toolReq.Arguments["text"].(string)
			writeResult(t, encoder, req.ID, types.ToolResult{
				Content: []types.ContentBlock{{Type: "text", Text: "echo: " + text}},
				IsError: false,
			})
		case "command.execute":
			var cmdReq ExecuteCommandRequest
			if err := json.Unmarshal(req.Params, &cmdReq); err != nil {
				writeError(t, encoder, req.ID, "invalid_request", err.Error())
				continue
			}
			switch cmdReq.Name {
			case "ping":
				writeResult(t, encoder, req.ID, ExecuteCommandResponse{
					Handled: true,
					Output:  "pong:" + cmdReq.Args + " [" + helperMode + "]",
				})
			case "ask":
				uiRequestID := "ui-helper-1"
				if err := encoder.Encode(map[string]any{
					"type":   "extension_ui_request",
					"id":     uiRequestID,
					"method": "input",
					"title":  "helper input",
				}); err != nil {
					t.Fatalf("encode ui request: %v", err)
				}
				if !scanner.Scan() {
					t.Fatalf("expected ui.respond request from host")
				}
				var uiReq struct {
					ID     string          `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := json.Unmarshal(scanner.Bytes(), &uiReq); err != nil {
					t.Fatalf("decode ui.respond request: %v", err)
				}
				if uiReq.Method != "ui.respond" {
					t.Fatalf("method = %q, want ui.respond", uiReq.Method)
				}
				var uiResp ExtensionUIResponse
				if err := json.Unmarshal(uiReq.Params, &uiResp); err != nil {
					t.Fatalf("decode ui.respond params: %v", err)
				}
				writeResult(t, encoder, uiReq.ID, map[string]any{"resolved": true})

				output := "cancelled"
				if !uiResp.Cancelled {
					if strings.TrimSpace(uiResp.Value) != "" {
						output = strings.TrimSpace(uiResp.Value)
					} else if uiResp.Confirmed != nil {
						if *uiResp.Confirmed {
							output = "true"
						} else {
							output = "false"
						}
					}
				}
				writeResult(t, encoder, req.ID, ExecuteCommandResponse{
					Handled: true,
					Output:  "asked:" + output,
				})
			default:
				writeError(t, encoder, req.ID, "command_not_found", "command not found")
			}
		case "shutdown":
			writeResult(t, encoder, req.ID, map[string]any{"ok": true})
			return
		default:
			writeError(t, encoder, req.ID, "method_not_found", "unknown method")
		}
	}
	os.Exit(0)
}

func writeResult(t *testing.T, enc *json.Encoder, id string, result any) {
	t.Helper()
	resp := map[string]any{
		"id":     id,
		"result": result,
	}
	if err := enc.Encode(resp); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func writeError(t *testing.T, enc *json.Encoder, id, code, message string) {
	t.Helper()
	resp := map[string]any{
		"id": id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	if err := enc.Encode(resp); err != nil {
		t.Fatalf("encode error response: %v", err)
	}
}
