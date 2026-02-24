package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/config"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/extensionsidecar"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/providers"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/session"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/tools"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

type Runtime struct {
	session       *session.Manager
	tools         *tools.Registry
	activeTools   map[string]struct{}
	modelRegistry *config.ModelRegistry
	auth          *config.AuthStorage
	model         types.Model
	thinkingLevel string
	systemPrompt  string
	sidecar       *extensionsidecar.Client
	steeringQueue []types.Message
	followUpQueue []types.Message
	mu            sync.Mutex
	abortRun      context.CancelFunc
}

type NewRuntimeOptions struct {
	CWD          string
	SessionDir   string
	Session      string
	NoSession    bool
	Provider     string
	Model        string
	APIKey       string
	SystemPrompt string

	ExtensionSidecarCommand string
	ExtensionSidecarArgs    []string
	ExtensionSidecarEnv     []string
	ExtensionPaths          []string
	ExtensionFlagValues     map[string]any
}

func NewRuntime(opts NewRuntimeOptions) (*Runtime, error) {
	cwd := opts.CWD
	if strings.TrimSpace(cwd) == "" {
		cwd = "."
	}
	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}

	auth := config.NewAuthStorage(config.GetAuthPath())
	if opts.APIKey != "" && opts.Provider != "" {
		auth.SetRuntimeAPIKey(opts.Provider, opts.APIKey)
	}
	registry := config.NewModelRegistry(auth, config.GetModelsPath())
	model, err := registry.ResolveModel(opts.Provider, opts.Model)
	if err != nil {
		return nil, err
	}

	sessDir := opts.SessionDir
	if sessDir == "" {
		sessDir = config.GetSessionsDirForCWD(absCWD)
	}
	sm := session.NewManager(sessDir)
	if !opts.NoSession {
		if opts.Session != "" {
			if err := sm.Open(opts.Session); err != nil {
				return nil, err
			}
		} else {
			opened, err := sm.OpenLatest()
			if err != nil {
				return nil, err
			}
			if !opened {
				if err := sm.CreateNew(absCWD, ""); err != nil {
					return nil, err
				}
			}
		}
	} else {
		if err := sm.CreateNew(absCWD, ""); err != nil {
			return nil, err
		}
	}

	if _, err := sm.AppendModelChange(model.Provider, model.ID); err != nil {
		return nil, err
	}
	if _, err := sm.AppendThinkingLevel("medium"); err != nil {
		return nil, err
	}

	r := &Runtime{
		session:       sm,
		tools:         tools.NewCodingRegistry(absCWD),
		activeTools:   map[string]struct{}{},
		modelRegistry: registry,
		auth:          auth,
		model:         model,
		thinkingLevel: "medium",
		systemPrompt:  defaultSystemPrompt(opts.SystemPrompt),
	}
	r.resetActiveToolsToAll()
	if err := r.initExtensionSidecar(absCWD, opts); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

func defaultSystemPrompt(override string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return "You are a coding agent. Use tools when needed. Prefer correct and minimal changes."
}

func (r *Runtime) Session() *session.Manager { return r.session }
func (r *Runtime) Model() types.Model        { return r.model }

func (r *Runtime) Close() error {
	r.mu.Lock()
	sidecar := r.sidecar
	r.sidecar = nil
	r.mu.Unlock()
	if sidecar != nil {
		return sidecar.Close()
	}
	return nil
}

func (r *Runtime) SetModel(provider, modelID string) error {
	model, err := r.modelRegistry.ResolveModel(provider, modelID)
	if err != nil {
		return err
	}
	previous := r.model
	r.model = model
	_, err = r.session.AppendModelChange(provider, modelID)
	if err != nil {
		return err
	}
	payload := map[string]any{
		"model":  model,
		"source": "set",
	}
	if strings.TrimSpace(previous.ID) != "" {
		payload["previousModel"] = previous
	}
	_, _ = r.emitEventBestEffort(context.Background(), extensionsidecar.Event{
		Type:    "model_select",
		Payload: payload,
	})
	return nil
}

func (r *Runtime) Prompt(text string) (types.Message, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return types.Message{}, errors.New("prompt text is empty")
	}

	if handled, assistant, err := r.tryExecuteSidecarCommand(text); err != nil {
		return types.Message{}, err
	} else if handled {
		return assistant, nil
	}

	if transformed, handled, err := r.applyInputHooks(text); err != nil {
		return types.Message{}, err
	} else if handled != nil {
		return *handled, nil
	} else {
		text = transformed
	}

	user := types.TextMessage(types.RoleUser, text)
	if err := r.appendSessionMessageWithEvents(context.Background(), user); err != nil {
		return types.Message{}, err
	}
	return r.runLoop()
}

var ErrAborted = errors.New("run aborted")

func (r *Runtime) Continue() (types.Message, error) {
	defs := r.currentToolsDefinitions()
	ctx := r.session.BuildContext(r.systemPrompt, r.session.LeafID(), defs)
	if len(ctx.Messages) == 0 {
		return types.Message{}, fmt.Errorf("cannot continue: no messages in session")
	}
	lastRole := ctx.Messages[len(ctx.Messages)-1].Role
	if lastRole != types.RoleUser && lastRole != types.RoleTool && !r.hasQueuedMessages() {
		return types.Message{}, fmt.Errorf("cannot continue from %s; last message must be user or toolResult", lastRole)
	}
	return r.runLoop()
}

func (r *Runtime) Steer(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("steer text is empty")
	}
	r.mu.Lock()
	r.steeringQueue = append(r.steeringQueue, types.TextMessage(types.RoleUser, text))
	r.mu.Unlock()
	return nil
}

func (r *Runtime) FollowUp(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("follow-up text is empty")
	}
	r.mu.Lock()
	r.followUpQueue = append(r.followUpQueue, types.TextMessage(types.RoleUser, text))
	r.mu.Unlock()
	return nil
}

func (r *Runtime) hasQueuedMessages() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.steeringQueue) > 0 || len(r.followUpQueue) > 0
}

func (r *Runtime) Abort() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.abortRun != nil {
		r.abortRun()
	}
}

func (r *Runtime) setAbort(cancel context.CancelFunc) {
	r.mu.Lock()
	r.abortRun = cancel
	r.mu.Unlock()
}

func (r *Runtime) clearAbort(_ context.CancelFunc) {
	r.mu.Lock()
	r.abortRun = nil
	r.mu.Unlock()
}

func (r *Runtime) runLoop() (types.Message, error) {
	lastAssistant := types.Message{}
	runCtx, cancel := context.WithCancel(context.Background())
	activeSystemPrompt := r.systemPrompt
	pendingMessages := r.dequeueSteeringMessages()
	if len(pendingMessages) == 0 {
		ctxState := r.session.BuildContext(activeSystemPrompt, r.session.LeafID(), r.currentToolsDefinitions())
		if len(ctxState.Messages) > 0 && ctxState.Messages[len(ctxState.Messages)-1].Role == types.RoleAssistant {
			pendingMessages = r.dequeueFollowUpMessages()
		}
	}
	r.setAbort(cancel)
	defer func() {
		r.clearAbort(cancel)
		cancel()
	}()

	promptText := latestUserPromptText(pendingMessages)
	if promptText == "" {
		ctxState := r.session.BuildContext(activeSystemPrompt, r.session.LeafID(), r.currentToolsDefinitions())
		promptText = latestUserPromptText(ctxState.Messages)
	}

	if result, ok := r.emitEventBestEffort(runCtx, extensionsidecar.Event{
		Type: "before_agent_start",
		Payload: map[string]any{
			"prompt":       promptText,
			"images":       []any{},
			"systemPrompt": r.systemPrompt,
			"model":        r.model.ID,
			"provider":     r.model.Provider,
		},
	}); ok && result.BeforeAgentStart != nil {
		if strings.TrimSpace(result.BeforeAgentStart.SystemPrompt) != "" {
			activeSystemPrompt = result.BeforeAgentStart.SystemPrompt
		}
		for _, message := range result.BeforeAgentStart.Messages {
			if err := r.appendSessionMessageWithEvents(runCtx, message); err != nil {
				return lastAssistant, err
			}
		}
	}
	_, _ = r.emitEventBestEffort(runCtx, extensionsidecar.Event{
		Type: "agent_start",
		Payload: map[string]any{
			"model":    r.model.ID,
			"provider": r.model.Provider,
		},
	})
	defer func() {
		_, _ = r.emitEventBestEffort(context.Background(), extensionsidecar.Event{
			Type: "agent_end",
			Payload: map[string]any{
				"model":    r.model.ID,
				"provider": r.model.Provider,
			},
		})
	}()

	for i := 0; i < 128; i++ {
		if err := runCtx.Err(); err != nil {
			return lastAssistant, ErrAborted
		}

		if len(pendingMessages) > 0 {
			for _, message := range pendingMessages {
				if err := r.appendSessionMessageWithEvents(runCtx, message); err != nil {
					return lastAssistant, err
				}
			}
			pendingMessages = nil
		}

		_, _ = r.emitEventBestEffort(runCtx, extensionsidecar.Event{
			Type: "turn_start",
			Payload: map[string]any{
				"turn": i + 1,
			},
		})

		toolDefs := r.currentToolsDefinitions()
		ctxState := r.session.BuildContext(activeSystemPrompt, r.session.LeafID(), toolDefs)
		ctx := types.Context{
			SystemPrompt: activeSystemPrompt,
			Messages:     ctxState.Messages,
			Tools:        toolDefs,
		}
		ctx = r.applyContextHooks(runCtx, i+1, ctx)

		prov, err := r.currentProvider()
		if err != nil {
			return lastAssistant, err
		}
		resp, err := prov.Complete(types.CompletionRequest{
			Model:   r.model,
			Context: ctx,
			Options: types.CompletionOptions{
				APIKey:    r.auth.GetAPIKey(r.model.Provider),
				SessionID: r.session.SessionID(),
				Context:   runCtx,
			},
		})
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(runCtx.Err(), context.Canceled) {
				return lastAssistant, ErrAborted
			}
			return lastAssistant, err
		}
		lastAssistant = resp.Assistant
		if err := r.appendSessionMessageWithEvents(runCtx, resp.Assistant); err != nil {
			return lastAssistant, err
		}
		turnToolMessages := make([]types.Message, 0, len(resp.ToolCalls))
		if len(resp.ToolCalls) == 0 {
			_, _ = r.emitEventBestEffort(runCtx, extensionsidecar.Event{
				Type: "turn_end",
				Payload: map[string]any{
					"turn":        i + 1,
					"toolCalls":   0,
					"message":     resp.Assistant,
					"toolResults": turnToolMessages,
				},
			})
			if followUps := r.dequeueFollowUpMessages(); len(followUps) > 0 {
				pendingMessages = followUps
				continue
			}
			return lastAssistant, nil
		}

		interrupted := false
		for toolIndex, call := range resp.ToolCalls {
			if err := runCtx.Err(); err != nil {
				return lastAssistant, ErrAborted
			}

			_, _ = r.emitEventBestEffort(runCtx, extensionsidecar.Event{
				Type: "tool_execution_start",
				Payload: map[string]any{
					"toolName":   call.Name,
					"toolCallId": call.ID,
					"toolCallID": call.ID,
					"args":       call.Arguments,
					"input":      call.Arguments,
					"arguments":  call.Arguments,
				},
			})

			blocked := false
			blockReason := ""
			if result, ok := r.emitEventBestEffort(runCtx, extensionsidecar.Event{
				Type: "tool_call",
				Payload: map[string]any{
					"toolName":   call.Name,
					"toolCallId": call.ID,
					"toolCallID": call.ID,
					"input":      call.Arguments,
					"arguments":  call.Arguments,
				},
			}); ok && result.ToolCall != nil && result.ToolCall.Block {
				blocked = true
				blockReason = strings.TrimSpace(result.ToolCall.Reason)
			}

			var result types.ToolResult
			if !r.isToolActive(call.Name) {
				result = types.ToolResult{
					IsError: true,
					Content: []types.ContentBlock{{Type: "text", Text: fmt.Sprintf("tool is not active: %s", call.Name)}},
				}
			} else if blocked {
				if blockReason == "" {
					blockReason = "tool execution blocked by extension"
				}
				result = types.ToolResult{
					IsError: true,
					Content: []types.ContentBlock{{Type: "text", Text: blockReason}},
				}
			} else {
				var err error
				result, err = r.tools.Execute(runCtx, call.Name, call.ID, call.Arguments)
				if err != nil {
					if errors.Is(err, context.Canceled) {
						return lastAssistant, ErrAborted
					}
					result.IsError = true
					if len(result.Content) == 0 {
						result.Content = []types.ContentBlock{{Type: "text", Text: err.Error()}}
					}
				}
			}
			toolResultPayload := map[string]any{
				"content": result.Content,
				"details": result.Details,
				"isError": result.IsError,
			}
			_, _ = r.emitEventBestEffort(runCtx, extensionsidecar.Event{
				Type: "tool_execution_update",
				Payload: map[string]any{
					"toolName":      call.Name,
					"toolCallId":    call.ID,
					"toolCallID":    call.ID,
					"args":          call.Arguments,
					"input":         call.Arguments,
					"arguments":     call.Arguments,
					"partialResult": toolResultPayload,
					"content":       result.Content,
					"details":       result.Details,
					"isError":       result.IsError,
				},
			})
			_, _ = r.emitEventBestEffort(runCtx, extensionsidecar.Event{
				Type: "tool_execution_end",
				Payload: map[string]any{
					"toolName":   call.Name,
					"toolCallId": call.ID,
					"toolCallID": call.ID,
					"args":       call.Arguments,
					"input":      call.Arguments,
					"arguments":  call.Arguments,
					"result":     toolResultPayload,
					"content":    result.Content,
					"details":    result.Details,
					"isError":    result.IsError,
				},
			})

			if eventResult, ok := r.emitEventBestEffort(runCtx, extensionsidecar.Event{
				Type: "tool_result",
				Payload: map[string]any{
					"toolName":   call.Name,
					"toolCallId": call.ID,
					"toolCallID": call.ID,
					"input":      call.Arguments,
					"arguments":  call.Arguments,
					"content":    result.Content,
					"details":    result.Details,
					"isError":    result.IsError,
				},
			}); ok && eventResult.ToolResult != nil {
				if len(eventResult.ToolResult.Content) > 0 {
					result.Content = eventResult.ToolResult.Content
				}
				if eventResult.ToolResult.Details != nil {
					result.Details = eventResult.ToolResult.Details
				}
				if eventResult.ToolResult.IsError != nil {
					result.IsError = *eventResult.ToolResult.IsError
				}
			}

			toolMsg := types.ToolResultMessage(call.ID, call.Name, result)
			turnToolMessages = append(turnToolMessages, toolMsg)
			if err := r.appendSessionMessageWithEvents(runCtx, toolMsg); err != nil {
				return lastAssistant, err
			}

			if steering := r.dequeueSteeringMessages(); len(steering) > 0 {
				remaining := resp.ToolCalls[toolIndex+1:]
				for _, skipped := range remaining {
					skipResult := types.ToolResult{
						IsError: true,
						Content: []types.ContentBlock{{Type: "text", Text: skippedToolCallReason}},
					}
					_, _ = r.emitEventBestEffort(runCtx, extensionsidecar.Event{
						Type: "tool_execution_start",
						Payload: map[string]any{
							"toolName":   skipped.Name,
							"toolCallId": skipped.ID,
							"toolCallID": skipped.ID,
							"args":       skipped.Arguments,
							"input":      skipped.Arguments,
							"arguments":  skipped.Arguments,
						},
					})
					_, _ = r.emitEventBestEffort(runCtx, extensionsidecar.Event{
						Type: "tool_execution_end",
						Payload: map[string]any{
							"toolName":   skipped.Name,
							"toolCallId": skipped.ID,
							"toolCallID": skipped.ID,
							"args":       skipped.Arguments,
							"input":      skipped.Arguments,
							"arguments":  skipped.Arguments,
							"result": map[string]any{
								"content": skipResult.Content,
								"details": skipResult.Details,
								"isError": true,
							},
							"content": skipResult.Content,
							"isError": true,
						},
					})
					skipMsg := types.ToolResultMessage(skipped.ID, skipped.Name, skipResult)
					turnToolMessages = append(turnToolMessages, skipMsg)
					if err := r.appendSessionMessageWithEvents(runCtx, skipMsg); err != nil {
						return lastAssistant, err
					}
				}
				pendingMessages = steering
				interrupted = true
				break
			}
		}

		_, _ = r.emitEventBestEffort(runCtx, extensionsidecar.Event{
			Type: "turn_end",
			Payload: map[string]any{
				"turn":        i + 1,
				"toolCalls":   len(resp.ToolCalls),
				"message":     resp.Assistant,
				"toolResults": turnToolMessages,
			},
		})
		if interrupted {
			continue
		}
	}

	return lastAssistant, fmt.Errorf("agent exceeded maximum turn iterations")
}

func (r *Runtime) currentProvider() (types.Provider, error) {
	cfg := r.modelRegistry.GetProviderConfig(r.model.Provider)
	apiKey := r.auth.GetAPIKey(r.model.Provider)
	if apiKey == "" && r.model.Provider != "amazon-bedrock" && r.model.Provider != "google-vertex" {
		return nil, fmt.Errorf("missing api key for provider %s", r.model.Provider)
	}
	return providers.BuildProvider(r.model, cfg, apiKey)
}

func (r *Runtime) initExtensionSidecar(cwd string, opts NewRuntimeOptions) error {
	if strings.TrimSpace(opts.ExtensionSidecarCommand) == "" {
		return nil
	}

	client, err := extensionsidecar.Start(extensionsidecar.Options{
		Command: opts.ExtensionSidecarCommand,
		Args:    opts.ExtensionSidecarArgs,
		CWD:     cwd,
		Env:     opts.ExtensionSidecarEnv,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	initResp, err := client.Initialize(ctx, extensionsidecar.InitializeRequest{
		ProtocolVersion: extensionsidecar.ProtocolVersion,
		CWD:             cwd,
		SessionID:       r.session.SessionID(),
		SessionFile:     r.session.SessionFile(),
		SessionName:     r.session.SessionName(),
		HostTools:       r.tools.Definitions(),
		ActiveTools:     r.currentActiveToolNames(),
		ExtensionPaths:  opts.ExtensionPaths,
		FlagValues:      opts.ExtensionFlagValues,
	})
	if err != nil {
		_ = client.Close()
		return err
	}
	if initResp.ProtocolVersion != "" && initResp.ProtocolVersion != extensionsidecar.ProtocolVersion {
		_ = client.Close()
		return fmt.Errorf(
			"unsupported extension sidecar protocol version %q (expected %q)",
			initResp.ProtocolVersion,
			extensionsidecar.ProtocolVersion,
		)
	}

	r.sidecar = client
	for _, reg := range initResp.Providers {
		if strings.TrimSpace(reg.Name) == "" || reg.Config == nil {
			continue
		}
		b, err := json.Marshal(reg.Config)
		if err != nil {
			continue
		}
		var providerCfg config.ProviderConfig
		if err := json.Unmarshal(b, &providerCfg); err != nil {
			continue
		}
		r.modelRegistry.RegisterProvider(reg.Name, providerCfg)
	}
	r.tools.RegisterDefinitions(initResp.Tools)
	r.resetActiveToolsToAll()
	r.tools.SetExternalExecutor(func(ctx context.Context, name, callID string, args map[string]interface{}) (types.ToolResult, bool, error) {
		if r.sidecar == nil {
			return types.ToolResult{}, false, nil
		}
		result, err := r.sidecar.ExecuteTool(ctx, name, callID, args)
		if errors.Is(err, extensionsidecar.ErrToolNotFound) {
			return types.ToolResult{}, false, nil
		}
		return result, true, err
	})

	if err := r.emitSessionStartEvent(client); err != nil {
		_ = client.Close()
		r.sidecar = nil
		return err
	}
	return nil
}

func (r *Runtime) emitSessionStartEvent(client *extensionsidecar.Client) error {
	if client == nil {
		return nil
	}
	emitCtx, emitCancel := context.WithTimeout(context.Background(), extensionEventTimeout)
	defer emitCancel()
	_, err := client.Emit(emitCtx, extensionsidecar.Event{
		Type: "session_start",
		Payload: map[string]any{
			"sessionId":   r.session.SessionID(),
			"sessionFile": r.session.SessionFile(),
			"sessionName": r.session.SessionName(),
			"cwd":         r.session.CWD(),
			"hostTools":   r.tools.Definitions(),
			"activeTools": r.currentActiveToolNames(),
		},
	})
	return err
}

func (r *Runtime) applyInputHooks(text string) (string, *types.Message, error) {
	result, ok := r.emitEventBestEffort(context.Background(), extensionsidecar.Event{
		Type: "input",
		Payload: map[string]any{
			"text":   text,
			"source": "interactive",
			"images": []any{},
		},
	})
	if !ok || result.Input == nil {
		return text, nil, nil
	}

	switch strings.ToLower(strings.TrimSpace(result.Input.Action)) {
	case "transform":
		if strings.TrimSpace(result.Input.Text) != "" {
			text = result.Input.Text
		}
		return text, nil, nil
	case "handled":
		assistantText := strings.TrimSpace(result.Input.AssistantText)
		if assistantText == "" {
			assistant := types.Message{
				Role:      types.RoleAssistant,
				Timestamp: types.NowMillis(),
			}
			return "", &assistant, nil
		}
		assistant := types.TextMessage(types.RoleAssistant, assistantText)
		if err := r.appendSessionMessageWithEvents(context.Background(), assistant); err != nil {
			return "", nil, err
		}
		return "", &assistant, nil
	default:
		return text, nil, nil
	}
}

func (r *Runtime) emitEventBestEffort(ctx context.Context, event extensionsidecar.Event) (extensionsidecar.EmitResponse, bool) {
	if r.sidecar == nil {
		return extensionsidecar.EmitResponse{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, extensionEventTimeout)
	defer cancel()
	result, err := r.sidecar.Emit(timeoutCtx, event)
	if err != nil {
		return extensionsidecar.EmitResponse{}, false
	}
	_, _, _ = r.applySidecarActions(context.Background(), result.Actions, false)
	return result, true
}

func (r *Runtime) appendSessionMessageWithEvents(ctx context.Context, message types.Message) error {
	_, _ = r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type: "message_start",
		Payload: map[string]any{
			"message":    message,
			"role":       message.Role,
			"toolName":   message.ToolName,
			"toolCallId": message.ToolCallID,
			"toolCallID": message.ToolCallID,
		},
	})
	if _, err := r.session.AppendMessage(message); err != nil {
		return err
	}
	if message.Role == types.RoleAssistant {
		_, _ = r.emitEventBestEffort(ctx, extensionsidecar.Event{
			Type: "message_update",
			Payload: map[string]any{
				"message": message,
				"assistantMessageEvent": map[string]any{
					"type":    "done",
					"partial": message,
				},
				"role":       message.Role,
				"toolName":   message.ToolName,
				"toolCallId": message.ToolCallID,
				"toolCallID": message.ToolCallID,
				"content":    message.Content,
			},
		})
	}
	_, _ = r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type: "message_end",
		Payload: map[string]any{
			"message":    message,
			"role":       message.Role,
			"toolName":   message.ToolName,
			"toolCallId": message.ToolCallID,
			"toolCallID": message.ToolCallID,
		},
	})
	return nil
}

func (r *Runtime) tryExecuteSidecarCommand(text string) (bool, types.Message, error) {
	if r.sidecar == nil || !strings.HasPrefix(text, "/") {
		return false, types.Message{}, nil
	}
	cmdLine := strings.TrimPrefix(text, "/")
	cmdLine = strings.TrimSpace(cmdLine)
	if cmdLine == "" {
		return false, types.Message{}, nil
	}
	commandName := cmdLine
	commandArgs := ""
	if idx := strings.IndexRune(cmdLine, ' '); idx >= 0 {
		commandName = strings.TrimSpace(cmdLine[:idx])
		commandArgs = strings.TrimSpace(cmdLine[idx+1:])
	}
	if commandName == "" {
		return false, types.Message{}, nil
	}
	result, err := r.sidecar.ExecuteCommand(context.Background(), commandName, commandArgs)
	if err != nil {
		return false, types.Message{}, err
	}
	if !result.Handled {
		return false, types.Message{}, nil
	}
	triggerRun, emittedMessage, err := r.applySidecarActions(context.Background(), result.Actions, true)
	if err != nil {
		return true, types.Message{}, err
	}
	output := strings.TrimSpace(result.Output)
	if output == "" && !triggerRun && !emittedMessage {
		output = fmt.Sprintf("Extension command /%s executed.", commandName)
	}
	var assistant types.Message
	if output != "" {
		assistant = types.TextMessage(types.RoleAssistant, output)
		if err := r.appendSessionMessageWithEvents(context.Background(), assistant); err != nil {
			return true, types.Message{}, err
		}
	}
	if triggerRun {
		runAssistant, err := r.runLoop()
		if err != nil {
			return true, types.Message{}, err
		}
		return true, runAssistant, nil
	}
	if output == "" {
		return true, types.Message{Role: types.RoleAssistant, Timestamp: types.NowMillis()}, nil
	}
	return true, assistant, nil
}

func (r *Runtime) applyContextHooks(ctx context.Context, turn int, current types.Context) types.Context {
	result, ok := r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type: "context",
		Payload: map[string]any{
			"turn":         turn,
			"systemPrompt": current.SystemPrompt,
			"messages":     current.Messages,
			"tools":        current.Tools,
			"model":        r.model.ID,
			"provider":     r.model.Provider,
		},
	})
	if !ok || result.Context == nil {
		return current
	}
	if strings.TrimSpace(result.Context.SystemPrompt) != "" {
		current.SystemPrompt = result.Context.SystemPrompt
	}
	if len(result.Context.Messages) > 0 {
		current.Messages = result.Context.Messages
	}
	return current
}

func (r *Runtime) dequeueSteeringMessages() []types.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.steeringQueue) == 0 {
		return nil
	}
	next := r.steeringQueue[0]
	r.steeringQueue = r.steeringQueue[1:]
	return []types.Message{next}
}

func (r *Runtime) dequeueFollowUpMessages() []types.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.followUpQueue) == 0 {
		return nil
	}
	next := r.followUpQueue[0]
	r.followUpQueue = r.followUpQueue[1:]
	return []types.Message{next}
}

func latestUserPromptText(messages []types.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != types.RoleUser {
			continue
		}
		if text := strings.TrimSpace(AssistantText(messages[i])); text != "" {
			return text
		}
	}
	return ""
}

func (r *Runtime) resetActiveToolsToAll() {
	defs := r.tools.Definitions()
	next := make(map[string]struct{}, len(defs))
	for _, def := range defs {
		next[def.Name] = struct{}{}
	}
	r.mu.Lock()
	r.activeTools = next
	r.mu.Unlock()
}

func (r *Runtime) setActiveToolsByName(names []string) error {
	defs := r.tools.Definitions()
	available := make(map[string]struct{}, len(defs))
	for _, def := range defs {
		available[def.Name] = struct{}{}
	}

	next := map[string]struct{}{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := available[name]; !ok {
			return fmt.Errorf("unknown tool in set_active_tools: %s", name)
		}
		next[name] = struct{}{}
	}

	r.mu.Lock()
	r.activeTools = next
	r.mu.Unlock()
	return nil
}

func (r *Runtime) currentActiveToolNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.activeTools))
	for name := range r.activeTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (r *Runtime) currentToolsDefinitions() []types.Tool {
	defs := r.tools.Definitions()
	r.mu.Lock()
	active := make(map[string]struct{}, len(r.activeTools))
	for name := range r.activeTools {
		active[name] = struct{}{}
	}
	r.mu.Unlock()
	filtered := make([]types.Tool, 0, len(defs))
	for _, def := range defs {
		if _, ok := active[def.Name]; ok {
			filtered = append(filtered, def)
		}
	}
	return filtered
}

func (r *Runtime) isToolActive(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.activeTools[name]
	return ok
}

func (r *Runtime) applySidecarActions(ctx context.Context, actions []extensionsidecar.HostAction, allowImmediateRun bool) (bool, bool, error) {
	triggerRun := false
	emittedMessage := false
	for _, action := range actions {
		switch strings.ToLower(strings.TrimSpace(action.Type)) {
		case "send_user_message":
			text := strings.TrimSpace(action.Text)
			if text == "" {
				continue
			}
			msg := types.TextMessage(types.RoleUser, text)
			delivery := normalizeActionDelivery(action.DeliverAs)
			if delivery == "" {
				delivery = "steer"
			}
			switch delivery {
			case "steer":
				r.mu.Lock()
				r.steeringQueue = append(r.steeringQueue, msg)
				r.mu.Unlock()
			default:
				r.mu.Lock()
				r.followUpQueue = append(r.followUpQueue, msg)
				r.mu.Unlock()
			}
			if allowImmediateRun {
				triggerRun = true
			}
		case "send_message":
			role := strings.TrimSpace(action.Role)
			if role == "" {
				role = types.RoleAssistant
			}
			content := make([]types.ContentBlock, 0, len(action.Content))
			for _, block := range action.Content {
				content = append(content, block)
			}
			text := strings.TrimSpace(action.Text)
			if len(content) == 0 && text != "" {
				content = []types.ContentBlock{{Type: "text", Text: text}}
			}
			customType := strings.TrimSpace(action.CustomType)
			if customType != "" {
				if len(content) == 0 {
					continue
				}
				if _, err := r.session.AppendCustomMessage(customType, content, action.Display, action.Data); err != nil {
					return triggerRun, emittedMessage, err
				}
				emittedMessage = true
				continue
			}
			if len(content) == 0 {
				continue
			}
			msg := types.Message{
				Role:      role,
				Timestamp: types.NowMillis(),
				Content:   content,
			}
			if role == types.RoleUser {
				delivery := normalizeActionDelivery(action.DeliverAs)
				if delivery == "" {
					delivery = "steer"
				}
				switch delivery {
				case "steer":
					r.mu.Lock()
					r.steeringQueue = append(r.steeringQueue, msg)
					r.mu.Unlock()
				default:
					r.mu.Lock()
					r.followUpQueue = append(r.followUpQueue, msg)
					r.mu.Unlock()
				}
				if allowImmediateRun {
					triggerRun = true
				}
				continue
			}
			if err := r.appendSessionMessageWithEvents(ctx, msg); err != nil {
				return triggerRun, emittedMessage, err
			}
			emittedMessage = true
		case "set_model":
			modelID := strings.TrimSpace(action.Model)
			if modelID == "" {
				continue
			}
			provider := strings.TrimSpace(action.Provider)
			if provider == "" {
				provider = r.model.Provider
			}
			if err := r.SetModel(provider, modelID); err != nil {
				return triggerRun, emittedMessage, err
			}
		case "set_thinking_level":
			level := strings.TrimSpace(action.ThinkingLevel)
			if level == "" {
				continue
			}
			r.thinkingLevel = level
			if _, err := r.session.AppendThinkingLevel(level); err != nil {
				return triggerRun, emittedMessage, err
			}
		case "append_entry":
			customType := strings.TrimSpace(action.CustomType)
			if customType == "" {
				continue
			}
			if _, err := r.session.AppendCustomEntry(customType, action.Data); err != nil {
				return triggerRun, emittedMessage, err
			}
		case "set_session_name":
			name := strings.TrimSpace(action.Name)
			if name == "" {
				continue
			}
			if _, err := r.session.AppendSessionName(name); err != nil {
				return triggerRun, emittedMessage, err
			}
		case "set_label":
			targetID := strings.TrimSpace(action.TargetID)
			if targetID == "" {
				continue
			}
			if _, err := r.session.AppendLabel(targetID, action.Label); err != nil {
				return triggerRun, emittedMessage, err
			}
		case "set_active_tools":
			if err := r.setActiveToolsByName(action.ToolNames); err != nil {
				return triggerRun, emittedMessage, err
			}
		case "new_session":
			cancelled, err := r.startNewSession(ctx, strings.TrimSpace(action.ParentSession))
			if err != nil {
				return triggerRun, emittedMessage, err
			}
			if cancelled {
				continue
			}
		case "switch_session":
			path := strings.TrimSpace(action.SessionPath)
			if path == "" {
				continue
			}
			cancelled, err := r.switchSession(ctx, path)
			if err != nil {
				return triggerRun, emittedMessage, err
			}
			if cancelled {
				continue
			}
		case "fork":
			entryID := strings.TrimSpace(action.EntryID)
			if entryID == "" {
				continue
			}
			cancelled, err := r.forkSession(ctx, entryID)
			if err != nil {
				return triggerRun, emittedMessage, err
			}
			if cancelled {
				continue
			}
		case "navigate_tree":
			targetID := strings.TrimSpace(action.TargetID)
			if targetID == "" {
				targetID = strings.TrimSpace(action.EntryID)
			}
			if targetID == "" {
				continue
			}
			if err := r.navigateTree(ctx, targetID, action); err != nil {
				return triggerRun, emittedMessage, err
			}
		case "reload", "wait_for_idle":
			// No-op in CLI text mode; accepted for extension compatibility.
			continue
		case "abort":
			r.Abort()
		case "shutdown":
			// Shutdown signaling is acknowledged but process-level exit is owned by caller.
			continue
		}
	}
	return triggerRun, emittedMessage, nil
}

func (r *Runtime) startNewSession(ctx context.Context, parentSession string) (bool, error) {
	previousSessionFile := r.session.SessionFile()
	if r.emitSessionBeforeSwitch(ctx, "new", "") {
		return true, nil
	}
	if strings.TrimSpace(parentSession) == "" {
		parentSession = r.session.SessionID()
	}
	if err := r.session.CreateNew(r.session.CWD(), parentSession); err != nil {
		return false, err
	}
	if _, err := r.session.AppendModelChange(r.model.Provider, r.model.ID); err != nil {
		return false, err
	}
	if _, err := r.session.AppendThinkingLevel(r.thinkingLevel); err != nil {
		return false, err
	}
	r.emitSessionSwitch(ctx, "new", previousSessionFile)
	if err := r.emitSessionStartEvent(r.sidecar); err != nil {
		return false, err
	}
	return false, nil
}

func (r *Runtime) switchSession(ctx context.Context, path string) (bool, error) {
	previousSessionFile := r.session.SessionFile()
	if r.emitSessionBeforeSwitch(ctx, "resume", path) {
		return true, nil
	}
	if err := r.session.Open(path); err != nil {
		return false, err
	}
	if err := r.syncRuntimeFromSessionState(); err != nil {
		return false, err
	}
	r.emitSessionSwitch(ctx, "resume", previousSessionFile)
	if err := r.emitSessionStartEvent(r.sidecar); err != nil {
		return false, err
	}
	return false, nil
}

func (r *Runtime) forkSession(ctx context.Context, entryID string) (bool, error) {
	if r.emitSessionBeforeFork(ctx, entryID) {
		return true, nil
	}
	branch := r.session.Branch(entryID)
	if len(branch) == 0 {
		return false, fmt.Errorf("cannot fork: entry %s not found", entryID)
	}
	parentSession := r.session.SessionID()
	previousSessionFile := r.session.SessionFile()
	cwd := r.session.CWD()
	prevModel := r.model
	prevThinking := r.thinkingLevel

	if err := r.session.CreateNew(cwd, parentSession); err != nil {
		return false, err
	}
	var sawModelChange, sawThinkingLevel bool
	for _, e := range branch {
		switch e.Type {
		case "message":
			if e.Message == nil {
				continue
			}
			if _, err := r.session.AppendMessage(*e.Message); err != nil {
				return false, err
			}
		case "model_change":
			if _, err := r.session.AppendModelChange(e.Provider, e.ModelID); err != nil {
				return false, err
			}
			sawModelChange = true
		case "thinking_level_change":
			if _, err := r.session.AppendThinkingLevel(e.ThinkingLevel); err != nil {
				return false, err
			}
			sawThinkingLevel = true
		case "compaction":
			if _, err := r.session.AppendCompaction(e.Summary, e.FirstKeptEntry, e.TokensBefore); err != nil {
				return false, err
			}
		case "branch_summary":
			if _, err := r.session.AppendBranchSummary(e.FromID, e.Summary); err != nil {
				return false, err
			}
		case "custom":
			if _, err := r.session.AppendCustomEntry(e.CustomType, e.CustomData); err != nil {
				return false, err
			}
		case "custom_message":
			if _, err := r.session.AppendCustomMessage(e.CustomType, e.Content, e.Display, e.CustomData); err != nil {
				return false, err
			}
		case "session_info":
			if strings.TrimSpace(e.Name) == "" {
				continue
			}
			if _, err := r.session.AppendSessionName(e.Name); err != nil {
				return false, err
			}
		}
	}

	if !sawModelChange {
		if _, err := r.session.AppendModelChange(prevModel.Provider, prevModel.ID); err != nil {
			return false, err
		}
	}
	if !sawThinkingLevel {
		if _, err := r.session.AppendThinkingLevel(prevThinking); err != nil {
			return false, err
		}
	}

	if err := r.syncRuntimeFromSessionState(); err != nil {
		return false, err
	}
	r.emitSessionFork(ctx, previousSessionFile)
	if err := r.emitSessionStartEvent(r.sidecar); err != nil {
		return false, err
	}
	return false, nil
}

func (r *Runtime) navigateTree(ctx context.Context, targetID string, action extensionsidecar.HostAction) error {
	oldLeafID := r.session.LeafID()
	if oldLeafID == targetID {
		return nil
	}
	if r.emitSessionBeforeTree(ctx, targetID, oldLeafID, action) {
		return nil
	}
	if err := r.session.SetLeaf(targetID); err != nil {
		return err
	}
	payload := map[string]any{
		"targetId":      targetID,
		"oldLeafId":     oldLeafID,
		"newLeafId":     r.session.LeafID(),
		"fromExtension": true,
	}
	if action.Summarize {
		payload["userWantsSummary"] = true
	}
	if strings.TrimSpace(action.CustomInstructions) != "" {
		payload["customInstructions"] = action.CustomInstructions
	}
	if action.ReplaceInstructions {
		payload["replaceInstructions"] = true
	}
	if strings.TrimSpace(action.Label) != "" {
		payload["label"] = action.Label
	}
	_, _ = r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type:    "session_tree",
		Payload: payload,
	})
	return nil
}

func (r *Runtime) emitSessionBeforeSwitch(ctx context.Context, reason string, targetSessionFile string) bool {
	payload := map[string]any{
		"reason": reason,
	}
	if strings.TrimSpace(targetSessionFile) != "" {
		payload["targetSessionFile"] = targetSessionFile
	}
	result, ok := r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type:    "session_before_switch",
		Payload: payload,
	})
	return ok && result.SessionBeforeSwitch != nil && result.SessionBeforeSwitch.Cancel
}

func (r *Runtime) emitSessionSwitch(ctx context.Context, reason string, previousSessionFile string) {
	_, _ = r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type: "session_switch",
		Payload: map[string]any{
			"reason":              reason,
			"previousSessionFile": previousSessionFile,
		},
	})
}

func (r *Runtime) emitSessionBeforeFork(ctx context.Context, entryID string) bool {
	result, ok := r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type: "session_before_fork",
		Payload: map[string]any{
			"entryId": entryID,
		},
	})
	return ok && result.SessionBeforeFork != nil && result.SessionBeforeFork.Cancel
}

func (r *Runtime) emitSessionFork(ctx context.Context, previousSessionFile string) {
	_, _ = r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type: "session_fork",
		Payload: map[string]any{
			"previousSessionFile": previousSessionFile,
		},
	})
}

func (r *Runtime) emitSessionBeforeTree(
	ctx context.Context,
	targetID string,
	oldLeafID string,
	action extensionsidecar.HostAction,
) bool {
	preparation := map[string]any{
		"targetId":         targetID,
		"oldLeafId":        oldLeafID,
		"userWantsSummary": action.Summarize,
	}
	if strings.TrimSpace(action.CustomInstructions) != "" {
		preparation["customInstructions"] = action.CustomInstructions
	}
	if action.ReplaceInstructions {
		preparation["replaceInstructions"] = true
	}
	if strings.TrimSpace(action.Label) != "" {
		preparation["label"] = action.Label
	}
	result, ok := r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type: "session_before_tree",
		Payload: map[string]any{
			"preparation": preparation,
		},
	})
	return ok && result.SessionBeforeTree != nil && result.SessionBeforeTree.Cancel
}

func (r *Runtime) syncRuntimeFromSessionState() error {
	ctx := r.session.BuildContext(r.systemPrompt, r.session.LeafID(), r.currentToolsDefinitions())
	if strings.TrimSpace(ctx.ThinkingLevel) != "" {
		r.thinkingLevel = strings.TrimSpace(ctx.ThinkingLevel)
	}
	provider := strings.TrimSpace(ctx.ModelProvider)
	modelID := strings.TrimSpace(ctx.ModelID)
	if provider == "" && modelID == "" {
		return nil
	}
	model, err := r.modelRegistry.ResolveModel(provider, modelID)
	if err != nil {
		return err
	}
	r.model = model
	return nil
}

func normalizeActionDelivery(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "steer":
		return "steer"
	case "followup", "follow_up", "follow-up":
		return "followup"
	case "nextturn", "next_turn", "next-turn":
		return "followup"
	default:
		return ""
	}
}

const extensionEventTimeout = 2 * time.Second
const skippedToolCallReason = "Skipped due to queued user message."
