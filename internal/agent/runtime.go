package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
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
	session               *session.Manager
	tools                 *tools.Registry
	activeTools           map[string]struct{}
	modelRegistry         *config.ModelRegistry
	auth                  *config.AuthStorage
	model                 types.Model
	thinkingLevel         string
	systemPrompt          string
	sidecar               *extensionsidecar.Client
	extensionCommands     []extensionsidecar.ExtensionCommandDefinition
	extensionUIEnabled    bool
	steeringQueue         []types.Message
	followUpQueue         []types.Message
	steeringMode          string
	followUpMode          string
	autoCompactionEnabled bool
	autoRetryEnabled      bool
	mu                    sync.Mutex
	abortRun              context.CancelFunc
	abortBash             context.CancelFunc
	abortRetry            context.CancelFunc

	eventSubscribers      map[int]func(RuntimeEvent)
	nextEventSubscriberID int
}

type RuntimeEvent struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

type Command struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`
	Location    string `json:"location,omitempty"`
	Path        string `json:"path,omitempty"`
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
	EnableExtensionUI       bool
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
		session:               sm,
		tools:                 tools.NewCodingRegistry(absCWD),
		activeTools:           map[string]struct{}{},
		modelRegistry:         registry,
		auth:                  auth,
		model:                 model,
		thinkingLevel:         "medium",
		systemPrompt:          defaultSystemPrompt(opts.SystemPrompt),
		extensionUIEnabled:    opts.EnableExtensionUI,
		steeringMode:          queueModeOneAtATime,
		followUpMode:          queueModeOneAtATime,
		autoCompactionEnabled: true,
		autoRetryEnabled:      true,
		eventSubscribers:      map[int]func(RuntimeEvent){},
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
func (r *Runtime) Model() types.Model {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.model
}

func (r *Runtime) Close() error {
	r.mu.Lock()
	sidecar := r.sidecar
	r.sidecar = nil
	r.mu.Unlock()
	if sidecar != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, _ = sidecar.Emit(shutdownCtx, extensionsidecar.Event{Type: "session_shutdown"})
		cancel()
		return sidecar.Close()
	}
	return nil
}

func (r *Runtime) SetModel(provider, modelID string) error {
	model, err := r.modelRegistry.ResolveModel(provider, modelID)
	if err != nil {
		return err
	}
	previous := r.Model()
	if _, err := r.session.AppendModelChange(provider, modelID); err != nil {
		return err
	}
	r.mu.Lock()
	r.model = model
	r.mu.Unlock()
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

func (r *Runtime) AvailableModels() []types.Model {
	return r.modelRegistry.GetAvailable()
}

func (r *Runtime) CycleModel() (*types.Model, bool, error) {
	models := r.AvailableModels()
	if len(models) <= 1 {
		return nil, false, nil
	}
	current := r.Model()
	index := -1
	for i, model := range models {
		if strings.EqualFold(model.Provider, current.Provider) && strings.EqualFold(model.ID, current.ID) {
			index = i
			break
		}
	}
	next := models[0]
	if index >= 0 {
		next = models[(index+1)%len(models)]
	}
	if err := r.SetModel(next.Provider, next.ID); err != nil {
		return nil, false, err
	}
	return &next, false, nil
}

func (r *Runtime) ThinkingLevel() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.thinkingLevel
}

func (r *Runtime) SetThinkingLevel(level string) error {
	level = strings.TrimSpace(level)
	if level == "" {
		return errors.New("thinking level is empty")
	}
	if _, err := r.session.AppendThinkingLevel(level); err != nil {
		return err
	}
	r.mu.Lock()
	r.thinkingLevel = level
	r.mu.Unlock()
	return nil
}

func (r *Runtime) CycleThinkingLevel() (string, bool, error) {
	current := strings.ToLower(strings.TrimSpace(r.ThinkingLevel()))
	if current == "" {
		return "", false, nil
	}
	index := -1
	for i, level := range thinkingLevelCycle {
		if level == current {
			index = i
			break
		}
	}
	if index < 0 {
		return "", false, nil
	}
	next := thinkingLevelCycle[(index+1)%len(thinkingLevelCycle)]
	if err := r.SetThinkingLevel(next); err != nil {
		return "", false, err
	}
	return next, true, nil
}

func (r *Runtime) SetSteeringMode(mode string) error {
	normalized, err := normalizeQueueMode(mode)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.steeringMode = normalized
	r.mu.Unlock()
	return nil
}

func (r *Runtime) SetFollowUpMode(mode string) error {
	normalized, err := normalizeQueueMode(mode)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.followUpMode = normalized
	r.mu.Unlock()
	return nil
}

func (r *Runtime) SteeringMode() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.steeringMode
}

func (r *Runtime) FollowUpMode() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.followUpMode
}

func (r *Runtime) IsStreaming() bool {
	return !r.runtimeIsIdle()
}

func (r *Runtime) PendingMessageCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.steeringQueue) + len(r.followUpQueue)
}

func (r *Runtime) SessionFile() string { return r.session.SessionFile() }
func (r *Runtime) SessionID() string   { return r.session.SessionID() }
func (r *Runtime) SessionName() string { return r.session.SessionName() }

func (r *Runtime) SetAutoCompactionEnabled(enabled bool) {
	r.mu.Lock()
	r.autoCompactionEnabled = enabled
	r.mu.Unlock()
}

func (r *Runtime) AutoCompactionEnabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.autoCompactionEnabled
}

func (r *Runtime) SetAutoRetryEnabled(enabled bool) {
	r.mu.Lock()
	r.autoRetryEnabled = enabled
	r.mu.Unlock()
}

func (r *Runtime) AutoRetryEnabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.autoRetryEnabled
}

func (r *Runtime) AbortRetry() {
	r.mu.Lock()
	cancel := r.abortRetry
	r.abortRetry = nil
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (r *Runtime) RespondExtensionUI(response extensionsidecar.ExtensionUIResponse) error {
	response.ID = strings.TrimSpace(response.ID)
	if response.ID == "" {
		return errors.New("extension ui response id is required")
	}

	r.mu.Lock()
	sidecar := r.sidecar
	r.mu.Unlock()
	if sidecar == nil {
		return errors.New("extension sidecar is not enabled")
	}

	ctx, cancel := context.WithTimeout(context.Background(), extensionUIResponseTimeout)
	defer cancel()
	return sidecar.RespondExtensionUI(ctx, response)
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

func (r *Runtime) PromptMessage(message types.Message) (types.Message, error) {
	if message.Role == "" {
		message.Role = types.RoleUser
	}
	if message.Role != types.RoleUser {
		return types.Message{}, fmt.Errorf("prompt message role must be %s", types.RoleUser)
	}
	if !hasMessageContent(message.Content) {
		return types.Message{}, errors.New("prompt message content is empty")
	}
	if message.Timestamp == 0 {
		message.Timestamp = types.NowMillis()
	}
	if err := r.appendSessionMessageWithEvents(context.Background(), message); err != nil {
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
	return r.SteerMessage(types.TextMessage(types.RoleUser, text))
}

func (r *Runtime) SteerMessage(message types.Message) error {
	if message.Role == "" {
		message.Role = types.RoleUser
	}
	if message.Role != types.RoleUser {
		return fmt.Errorf("steer message role must be %s", types.RoleUser)
	}
	if !hasMessageContent(message.Content) {
		return errors.New("steer message content is empty")
	}
	if message.Timestamp == 0 {
		message.Timestamp = types.NowMillis()
	}
	r.mu.Lock()
	r.steeringQueue = append(r.steeringQueue, message)
	r.mu.Unlock()
	return nil
}

func (r *Runtime) FollowUp(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("follow-up text is empty")
	}
	return r.FollowUpMessage(types.TextMessage(types.RoleUser, text))
}

func (r *Runtime) FollowUpMessage(message types.Message) error {
	if message.Role == "" {
		message.Role = types.RoleUser
	}
	if message.Role != types.RoleUser {
		return fmt.Errorf("follow-up message role must be %s", types.RoleUser)
	}
	if !hasMessageContent(message.Content) {
		return errors.New("follow-up message content is empty")
	}
	if message.Timestamp == 0 {
		message.Timestamp = types.NowMillis()
	}
	r.mu.Lock()
	r.followUpQueue = append(r.followUpQueue, message)
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
	cancelRun := r.abortRun
	cancelRetry := r.abortRetry
	r.abortRetry = nil
	r.mu.Unlock()
	if cancelRun != nil {
		cancelRun()
	}
	if cancelRetry != nil {
		cancelRetry()
	}
}

func (r *Runtime) ExecuteBash(command string) (types.ToolResult, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return types.ToolResult{IsError: true}, errors.New("bash command is empty")
	}
	ctx, cancel := context.WithCancel(context.Background())

	r.mu.Lock()
	if r.abortBash != nil {
		r.mu.Unlock()
		cancel()
		return types.ToolResult{IsError: true}, errors.New("bash command already running")
	}
	r.abortBash = cancel
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.abortBash = nil
		r.mu.Unlock()
		cancel()
	}()

	return r.tools.Execute(ctx, "bash", "rpc_bash", map[string]interface{}{
		"command": command,
	})
}

func (r *Runtime) AbortBash() {
	r.mu.Lock()
	cancel := r.abortBash
	r.mu.Unlock()
	if cancel != nil {
		cancel()
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
	retryAttempt := 0

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
		if r.AutoCompactionEnabled() {
			_, _ = r.maybeAutoCompactThreshold(runCtx)
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
			if r.AutoCompactionEnabled() && isContextOverflowProviderError(err) {
				if compacted, _ := r.runAutoCompaction(runCtx, "overflow", true, err); compacted {
					retryAttempt = 0
					continue
				}
			}
			if r.AutoRetryEnabled() && isRetryableProviderError(err) {
				attempt := retryAttempt + 1
				if attempt > autoRetryMaxAttempts {
					r.emitAutoRetryEnd(retryAttempt, false, err.Error())
					retryAttempt = 0
					return lastAssistant, err
				}
				retryAttempt = attempt
				delay := autoRetryBaseDelay * time.Duration(1<<(attempt-1))
				r.emitAutoRetryStart(attempt, autoRetryMaxAttempts, delay, err.Error())
				if cancelled := r.waitForRetryDelay(runCtx, delay); cancelled {
					r.emitAutoRetryEnd(attempt, false, "Retry cancelled")
					retryAttempt = 0
					if errors.Is(runCtx.Err(), context.Canceled) {
						return lastAssistant, ErrAborted
					}
					return lastAssistant, err
				}
				continue
			}
			if retryAttempt > 0 {
				r.emitAutoRetryEnd(retryAttempt, false, err.Error())
				retryAttempt = 0
			}
			return lastAssistant, err
		}
		if retryAttempt > 0 {
			r.emitAutoRetryEnd(retryAttempt, true, "")
			retryAttempt = 0
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
	client.SetExtensionUIRequestHandler(func(req extensionsidecar.ExtensionUIRequest) {
		r.emitRuntimeEvent(RuntimeEvent{
			Type:    "extension_ui_request",
			Payload: extensionUIRequestPayload(req),
		})
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	initResp, err := client.Initialize(ctx, extensionsidecar.InitializeRequest{
		ProtocolVersion:    extensionsidecar.ProtocolVersion,
		CWD:                cwd,
		SessionID:          r.session.SessionID(),
		SessionFile:        r.session.SessionFile(),
		SessionDir:         r.session.SessionDir(),
		SessionName:        r.session.SessionName(),
		LeafID:             r.session.LeafID(),
		SessionHeader:      sessionHeaderMap(r.session.Header()),
		SessionEntries:     sessionEntriesMaps(r.session.Entries()),
		CurrentModel:       &r.model,
		AllModels:          r.modelRegistry.GetAll(),
		AvailableModels:    r.modelRegistry.GetAvailable(),
		ProviderAPIKeys:    r.sidecarProviderAPIKeys(),
		ProviderAuthTypes:  r.sidecarProviderAuthTypes(),
		ContextUsage:       r.contextUsageSnapshot(),
		SystemPrompt:       r.systemPrompt,
		ThinkingLevel:      r.thinkingLevel,
		IsIdle:             r.runtimeIsIdle(),
		HasPendingMessages: r.hasQueuedMessages(),
		HostTools:          r.tools.Definitions(),
		ActiveTools:        r.currentActiveToolNames(),
		ExtensionPaths:     opts.ExtensionPaths,
		FlagValues:         opts.ExtensionFlagValues,
		HasUI:              opts.EnableExtensionUI,
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
	r.extensionCommands = append([]extensionsidecar.ExtensionCommandDefinition(nil), initResp.Commands...)
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
			"sessionId":          r.session.SessionID(),
			"sessionFile":        r.session.SessionFile(),
			"sessionDir":         r.session.SessionDir(),
			"sessionName":        r.session.SessionName(),
			"leafId":             r.session.LeafID(),
			"cwd":                r.session.CWD(),
			"sessionHeader":      sessionHeaderMap(r.session.Header()),
			"sessionEntries":     sessionEntriesMaps(r.session.Entries()),
			"currentModel":       r.model,
			"allModels":          r.modelRegistry.GetAll(),
			"availableModels":    r.modelRegistry.GetAvailable(),
			"providerApiKeys":    r.sidecarProviderAPIKeys(),
			"providerAuthTypes":  r.sidecarProviderAuthTypes(),
			"contextUsage":       r.contextUsageSnapshot(),
			"systemPrompt":       r.systemPrompt,
			"thinkingLevel":      r.thinkingLevel,
			"isIdle":             r.runtimeIsIdle(),
			"hasPendingMessages": r.hasQueuedMessages(),
			"hostTools":          r.tools.Definitions(),
			"activeTools":        r.currentActiveToolNames(),
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
	if ctx == nil {
		ctx = context.Background()
	}
	if event.Payload == nil {
		event.Payload = map[string]any{}
	}
	event.Payload["ctxModel"] = r.Model()
	event.Payload["ctxSystemPrompt"] = r.systemPrompt
	event.Payload["ctxThinkingLevel"] = r.ThinkingLevel()
	event.Payload["ctxIsIdle"] = r.runtimeIsIdle()
	event.Payload["ctxHasPendingMessages"] = r.hasQueuedMessages()
	event.Payload["ctxProviderAuthTypes"] = r.sidecarProviderAuthTypes()
	event.Payload["ctxContextUsage"] = r.contextUsageSnapshot()

	r.emitRuntimeEvent(RuntimeEvent{
		Type:    event.Type,
		Payload: anyMap(event.Payload),
	})

	r.mu.Lock()
	sidecar := r.sidecar
	r.mu.Unlock()
	if sidecar == nil {
		return extensionsidecar.EmitResponse{}, false
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, extensionEventTimeout)
	defer cancel()
	result, err := sidecar.Emit(timeoutCtx, event)
	if err != nil {
		return extensionsidecar.EmitResponse{}, false
	}
	_, _, _ = r.applySidecarActions(context.Background(), result.Actions, false)
	return result, true
}

func (r *Runtime) runtimeIsIdle() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.abortRun == nil
}

func (r *Runtime) SubscribeEvents(listener func(RuntimeEvent)) func() {
	if listener == nil {
		return func() {}
	}
	r.mu.Lock()
	id := r.nextEventSubscriberID
	r.nextEventSubscriberID++
	if r.eventSubscribers == nil {
		r.eventSubscribers = map[int]func(RuntimeEvent){}
	}
	r.eventSubscribers[id] = listener
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.eventSubscribers, id)
		r.mu.Unlock()
	}
}

func (r *Runtime) emitRuntimeEvent(event RuntimeEvent) {
	r.mu.Lock()
	listeners := make([]func(RuntimeEvent), 0, len(r.eventSubscribers))
	for _, listener := range r.eventSubscribers {
		listeners = append(listeners, listener)
	}
	r.mu.Unlock()

	for _, listener := range listeners {
		func(fn func(RuntimeEvent)) {
			defer func() {
				_ = recover()
			}()
			fn(event)
		}(listener)
	}
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
	entry, err := r.session.AppendMessage(message)
	if err != nil {
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
			"entry":      entry,
			"entryId":    entry.ID,
			"parentId":   entry.ParentID,
			"leafId":     r.session.LeafID(),
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
	result, err := r.sidecar.ExecuteCommandWithRequest(context.Background(), extensionsidecar.ExecuteCommandRequest{
		Name:               commandName,
		Args:               commandArgs,
		CurrentModel:       &r.model,
		AllModels:          r.modelRegistry.GetAll(),
		AvailableModels:    r.modelRegistry.GetAvailable(),
		ProviderAPIKeys:    r.sidecarProviderAPIKeys(),
		ProviderAuthTypes:  r.sidecarProviderAuthTypes(),
		ContextUsage:       r.contextUsageSnapshot(),
		SystemPrompt:       r.systemPrompt,
		ThinkingLevel:      r.thinkingLevel,
		IsIdle:             r.runtimeIsIdle(),
		HasPendingMessages: r.hasQueuedMessages(),
	})
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
	if r.steeringMode == queueModeAll {
		messages := append([]types.Message(nil), r.steeringQueue...)
		r.steeringQueue = nil
		return messages
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
	if r.followUpMode == queueModeAll {
		messages := append([]types.Message(nil), r.followUpQueue...)
		r.followUpQueue = nil
		return messages
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

func (r *Runtime) sidecarProviderAPIKeys() map[string]string {
	providers := map[string]struct{}{}
	for _, model := range r.modelRegistry.GetAll() {
		provider := strings.TrimSpace(model.Provider)
		if provider == "" {
			continue
		}
		providers[provider] = struct{}{}
	}
	if provider := strings.TrimSpace(r.model.Provider); provider != "" {
		providers[provider] = struct{}{}
	}
	if len(providers) == 0 {
		return nil
	}
	keys := map[string]string{}
	for provider := range providers {
		key := strings.TrimSpace(r.auth.GetAPIKey(provider))
		if key == "" {
			continue
		}
		keys[provider] = key
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}

func (r *Runtime) sidecarProviderAuthTypes() map[string]string {
	providers := map[string]struct{}{}
	for _, model := range r.modelRegistry.GetAll() {
		provider := strings.TrimSpace(model.Provider)
		if provider == "" {
			continue
		}
		providers[provider] = struct{}{}
	}
	if provider := strings.TrimSpace(r.model.Provider); provider != "" {
		providers[provider] = struct{}{}
	}
	if len(providers) == 0 {
		return nil
	}
	authTypes := map[string]string{}
	for provider := range providers {
		authType := strings.TrimSpace(r.auth.ProviderAuthType(provider))
		if authType == "" {
			continue
		}
		authTypes[provider] = authType
	}
	if len(authTypes) == 0 {
		return nil
	}
	return authTypes
}

func (r *Runtime) contextUsageSnapshot() map[string]any {
	contextWindow := r.model.ContextWindow
	if contextWindow <= 0 {
		return nil
	}

	branch := r.session.Branch(r.session.LeafID())
	if len(branch) == 0 {
		return map[string]any{
			"tokens":        int64(0),
			"contextWindow": contextWindow,
			"percent":       float64(0),
		}
	}

	latestCompactionIndex := -1
	for i := len(branch) - 1; i >= 0; i-- {
		if branch[i].Type == "compaction" {
			latestCompactionIndex = i
			break
		}
	}
	if latestCompactionIndex >= 0 {
		hasPostCompactionUsage := false
		for i := len(branch) - 1; i > latestCompactionIndex; i-- {
			e := branch[i]
			if e.Type != "message" || e.Message == nil {
				continue
			}
			msg := e.Message
			if msg.Role != types.RoleAssistant {
				continue
			}
			if msg.StopReason == "aborted" || msg.StopReason == "error" {
				continue
			}
			if calculateContextTokens(msg.Usage) > 0 {
				hasPostCompactionUsage = true
			}
			break
		}
		if !hasPostCompactionUsage {
			return map[string]any{
				"tokens":        nil,
				"contextWindow": contextWindow,
				"percent":       nil,
			}
		}
	}

	ctxState := r.session.BuildContext(r.systemPrompt, r.session.LeafID(), r.currentToolsDefinitions())
	tokens := estimateContextTokens(ctxState.Messages)
	if tokens < 0 {
		tokens = 0
	}
	percent := (float64(tokens) / float64(contextWindow)) * 100.0
	return map[string]any{
		"tokens":        tokens,
		"contextWindow": contextWindow,
		"percent":       percent,
	}
}

func (r *Runtime) maybeAutoCompactThreshold(ctx context.Context) (bool, error) {
	usage := r.contextUsageSnapshot()
	if len(usage) == 0 {
		return false, nil
	}
	percent, ok := float64Value(usage["percent"])
	if !ok {
		return false, nil
	}
	if percent < autoCompactionThresholdPercent {
		return false, nil
	}
	return r.runAutoCompaction(ctx, "threshold", false, nil)
}

func (r *Runtime) runAutoCompaction(
	ctx context.Context,
	reason string,
	willRetry bool,
	causeErr error,
) (bool, error) {
	r.emitRuntimeEvent(RuntimeEvent{
		Type: "auto_compaction_start",
		Payload: map[string]any{
			"reason": reason,
		},
	})

	entry, err := r.compactContext(ctx, "", "")
	if err != nil {
		message := strings.TrimSpace(err.Error())
		if causeErr != nil {
			prefix := "Auto-compaction failed: "
			if strings.EqualFold(reason, "overflow") {
				prefix = "Context overflow recovery failed: "
			}
			message = prefix + strings.TrimSpace(causeErr.Error())
		}
		aborted := errors.Is(err, context.Canceled) || strings.Contains(strings.ToLower(err.Error()), "cancel")
		r.emitRuntimeEvent(RuntimeEvent{
			Type: "auto_compaction_end",
			Payload: map[string]any{
				"result":       nil,
				"aborted":      aborted,
				"willRetry":    false,
				"errorMessage": message,
			},
		})
		return false, err
	}

	result := map[string]any{
		"summary":          entry.Summary,
		"firstKeptEntryId": entry.FirstKeptEntry,
		"tokensBefore":     entry.TokensBefore,
		"details":          entry.Details,
	}
	r.emitRuntimeEvent(RuntimeEvent{
		Type: "auto_compaction_end",
		Payload: map[string]any{
			"result":    result,
			"aborted":   false,
			"willRetry": willRetry,
		},
	})
	return true, nil
}

func (r *Runtime) waitForRetryDelay(ctx context.Context, delay time.Duration) bool {
	retryCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.abortRetry = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.abortRetry = nil
		r.mu.Unlock()
		cancel()
	}()

	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return false
	case <-retryCtx.Done():
		return true
	}
}

func (r *Runtime) emitAutoRetryStart(attempt int, maxAttempts int, delay time.Duration, message string) {
	r.emitRuntimeEvent(RuntimeEvent{
		Type: "auto_retry_start",
		Payload: map[string]any{
			"attempt":      attempt,
			"maxAttempts":  maxAttempts,
			"delayMs":      int(delay / time.Millisecond),
			"errorMessage": strings.TrimSpace(message),
		},
	})
}

func (r *Runtime) emitAutoRetryEnd(attempt int, success bool, finalError string) {
	payload := map[string]any{
		"success": success,
		"attempt": attempt,
	}
	if trimmed := strings.TrimSpace(finalError); trimmed != "" {
		payload["finalError"] = trimmed
	}
	r.emitRuntimeEvent(RuntimeEvent{
		Type:    "auto_retry_end",
		Payload: payload,
	})
}

func isRetryableProviderError(err error) bool {
	if err == nil || isContextOverflowProviderError(err) {
		return false
	}
	return retryableProviderErrorPattern.MatchString(strings.ToLower(err.Error()))
}

func isContextOverflowProviderError(err error) bool {
	if err == nil {
		return false
	}
	return contextOverflowErrorPattern.MatchString(strings.ToLower(err.Error()))
}

func float64Value(raw any) (float64, bool) {
	switch value := raw.(type) {
	case float64:
		return value, true
	case float32:
		return float64(value), true
	case int:
		return float64(value), true
	case int32:
		return float64(value), true
	case int64:
		return float64(value), true
	default:
		return 0, false
	}
}

func estimateContextTokens(messages []types.Message) int64 {
	if len(messages) == 0 {
		return 0
	}
	lastUsageIndex := -1
	var usageTokens int64
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Role != types.RoleAssistant {
			continue
		}
		if msg.StopReason == "aborted" || msg.StopReason == "error" {
			continue
		}
		tokens := calculateContextTokens(msg.Usage)
		if tokens <= 0 {
			continue
		}
		lastUsageIndex = i
		usageTokens = tokens
		break
	}
	if lastUsageIndex < 0 {
		var estimated int64
		for _, message := range messages {
			estimated += estimateMessageTokens(message)
		}
		return estimated
	}
	var trailing int64
	for i := lastUsageIndex + 1; i < len(messages); i++ {
		trailing += estimateMessageTokens(messages[i])
	}
	return usageTokens + trailing
}

func calculateContextTokens(usage types.Usage) int64 {
	if usage.Total > 0 {
		return usage.Total
	}
	return usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
}

func estimateMessageTokens(message types.Message) int64 {
	chars := 0
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			chars += len(block.Text)
		case "thinking":
			chars += len(block.Thinking)
		case "toolCall":
			chars += len(block.Name)
			if len(block.Arguments) > 0 {
				if b, err := json.Marshal(block.Arguments); err == nil {
					chars += len(b)
				}
			}
		default:
			chars += len(block.Text)
		}
	}
	if chars == 0 {
		return 0
	}
	// Conservative estimate: ~4 chars/token.
	return int64((chars + 3) / 4)
}

func (r *Runtime) compactContext(ctx context.Context, customInstructions string, requestID string) (session.Entry, error) {
	branch := r.session.Branch(r.session.LeafID())
	if len(branch) == 0 {
		return session.Entry{}, errors.New("nothing to compact: session is empty")
	}

	firstKeptID, ok := selectCompactionFirstKeptEntry(branch, 8)
	if !ok {
		return session.Entry{}, errors.New("nothing to compact (session too small)")
	}

	tokensBefore := int(estimateContextTokens(
		r.session.BuildContext(r.systemPrompt, r.session.LeafID(), r.currentToolsDefinitions()).Messages,
	))

	beforeResult := r.emitSessionBeforeCompact(ctx, firstKeptID, tokensBefore, customInstructions)
	if beforeResult != nil && beforeResult.Cancel {
		return session.Entry{}, errors.New("compaction cancelled")
	}

	summary := ""
	fromExtension := false
	var compactionDetails map[string]any
	if beforeResult != nil && beforeResult.Compaction != nil && strings.TrimSpace(beforeResult.Compaction.Summary) != "" {
		summary = strings.TrimSpace(beforeResult.Compaction.Summary)
		if candidate := strings.TrimSpace(beforeResult.Compaction.FirstKeptEntryID); candidate != "" && branchHasEntryID(branch, candidate) {
			firstKeptID = candidate
		}
		if beforeResult.Compaction.TokensBefore > 0 {
			tokensBefore = beforeResult.Compaction.TokensBefore
		}
		compactionDetails = beforeResult.Compaction.Details
		fromExtension = true
	} else {
		generated, err := r.generateCompactionSummary(ctx, branch, firstKeptID, customInstructions)
		if err != nil {
			return session.Entry{}, err
		}
		summary = generated
	}
	entry, err := r.session.AppendCompactionWithDetails(summary, firstKeptID, tokensBefore, compactionDetails)
	if err != nil {
		return session.Entry{}, err
	}
	_, _ = r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type: "session_compact",
		Payload: map[string]any{
			"compactionEntry": entry,
			"fromExtension":   fromExtension,
			"requestId":       requestID,
		},
	})
	return entry, nil
}

func selectCompactionFirstKeptEntry(branch []session.Entry, keepRecentMessages int) (string, bool) {
	if keepRecentMessages <= 0 {
		keepRecentMessages = 1
	}
	messageLikeIDs := make([]string, 0, len(branch))
	for _, e := range branch {
		if e.Type == "message" || e.Type == "custom_message" || e.Type == "branch_summary" {
			messageLikeIDs = append(messageLikeIDs, e.ID)
		}
	}
	if len(messageLikeIDs) <= keepRecentMessages {
		return "", false
	}
	return messageLikeIDs[len(messageLikeIDs)-keepRecentMessages], true
}

func branchHasEntryID(branch []session.Entry, id string) bool {
	for _, e := range branch {
		if e.ID == id {
			return true
		}
	}
	return false
}

func (r *Runtime) generateCompactionSummary(
	ctx context.Context,
	branch []session.Entry,
	firstKeptID string,
	customInstructions string,
) (string, error) {
	sourceText := formatCompactionSource(branch, firstKeptID)
	if strings.TrimSpace(sourceText) == "" {
		return "", errors.New("nothing to compact: no summarizable messages before cut point")
	}

	provider, err := r.currentProvider()
	if err != nil {
		return "", err
	}
	apiKey := r.auth.GetAPIKey(r.model.Provider)
	if apiKey == "" && r.model.Provider != "amazon-bedrock" && r.model.Provider != "google-vertex" {
		return "", fmt.Errorf("missing api key for provider %s", r.model.Provider)
	}

	prompt := "Summarize the earlier conversation context for a coding-agent session. " +
		"Keep critical decisions, constraints, open tasks, and file/tool details concise and actionable."
	if customInstructions != "" {
		prompt += "\n\nAdditional instructions:\n" + customInstructions
	}
	prompt += "\n\nConversation to summarize:\n" + sourceText

	resp, err := provider.Complete(types.CompletionRequest{
		Model: r.model,
		Context: types.Context{
			SystemPrompt: "You summarize coding-agent conversation history.",
			Messages: []types.Message{
				types.TextMessage(types.RoleUser, prompt),
			},
		},
		Options: types.CompletionOptions{
			APIKey:    apiKey,
			Context:   ctx,
			SessionID: r.session.SessionID(),
		},
	})
	if err != nil {
		return "", err
	}

	summary := strings.TrimSpace(AssistantText(resp.Assistant))
	if summary == "" {
		summary = "Compaction summary unavailable."
	}
	return summary, nil
}

func formatCompactionSource(branch []session.Entry, firstKeptID string) string {
	var lines []string
	for _, entry := range branch {
		if entry.ID == firstKeptID {
			break
		}
		switch entry.Type {
		case "message":
			if entry.Message == nil {
				continue
			}
			text := strings.TrimSpace(AssistantText(*entry.Message))
			if text == "" {
				continue
			}
			lines = append(lines, fmt.Sprintf("%s: %s", entry.Message.Role, text))
		case "custom_message":
			text := strings.TrimSpace(contentBlocksText(entry.Content))
			if text == "" {
				continue
			}
			if entry.CustomType != "" {
				lines = append(lines, fmt.Sprintf("custom[%s]: %s", entry.CustomType, text))
			} else {
				lines = append(lines, "custom: "+text)
			}
		case "branch_summary":
			text := strings.TrimSpace(entry.Summary)
			if text == "" {
				continue
			}
			lines = append(lines, "branch_summary: "+text)
		case "model_change":
			lines = append(lines, fmt.Sprintf("model_change: %s/%s", entry.Provider, entry.ModelID))
		case "thinking_level_change":
			lines = append(lines, "thinking_level_change: "+entry.ThinkingLevel)
		}
	}
	return strings.Join(lines, "\n")
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

func (r *Runtime) applySessionSetupEntries(setupEntries []map[string]any) error {
	refToEntryID := map[string]string{}
	for _, raw := range setupEntries {
		op := strings.ToLower(strings.TrimSpace(anyString(raw["op"])))
		ref := strings.TrimSpace(anyString(raw["ref"]))
		switch op {
		case "append_message":
			var msg types.Message
			if b, err := json.Marshal(raw["message"]); err == nil {
				_ = json.Unmarshal(b, &msg)
			}
			if msg.Role == "" {
				msg.Role = types.RoleUser
			}
			if msg.Timestamp == 0 {
				msg.Timestamp = types.NowMillis()
			}
			entry, err := r.session.AppendMessage(msg)
			if err != nil {
				return err
			}
			if ref != "" {
				refToEntryID[ref] = entry.ID
			}
		case "append_thinking_level_change":
			level := strings.TrimSpace(anyString(raw["thinkingLevel"]))
			if level == "" {
				continue
			}
			entry, err := r.session.AppendThinkingLevel(level)
			if err != nil {
				return err
			}
			if ref != "" {
				refToEntryID[ref] = entry.ID
			}
		case "append_model_change":
			provider := strings.TrimSpace(anyString(raw["provider"]))
			modelID := strings.TrimSpace(anyString(raw["modelId"]))
			if provider == "" || modelID == "" {
				continue
			}
			entry, err := r.session.AppendModelChange(provider, modelID)
			if err != nil {
				return err
			}
			if ref != "" {
				refToEntryID[ref] = entry.ID
			}
		case "append_custom_entry":
			customType := strings.TrimSpace(anyString(raw["customType"]))
			if customType == "" {
				continue
			}
			data := anyMap(raw["data"])
			entry, err := r.session.AppendCustomEntry(customType, data)
			if err != nil {
				return err
			}
			if ref != "" {
				refToEntryID[ref] = entry.ID
			}
		case "append_custom_message":
			customType := strings.TrimSpace(anyString(raw["customType"]))
			if customType == "" {
				continue
			}
			var content []types.ContentBlock
			if b, err := json.Marshal(raw["content"]); err == nil {
				_ = json.Unmarshal(b, &content)
			}
			if len(content) == 0 {
				continue
			}
			display := true
			if v, ok := raw["display"].(bool); ok {
				display = v
			}
			details := anyMap(raw["details"])
			entry, err := r.session.AppendCustomMessage(customType, content, display, details)
			if err != nil {
				return err
			}
			if ref != "" {
				refToEntryID[ref] = entry.ID
			}
		case "append_session_info":
			name := strings.TrimSpace(anyString(raw["name"]))
			if name == "" {
				continue
			}
			entry, err := r.session.AppendSessionName(name)
			if err != nil {
				return err
			}
			if ref != "" {
				refToEntryID[ref] = entry.ID
			}
		case "append_label":
			targetID := strings.TrimSpace(anyString(raw["targetId"]))
			if targetRef := strings.TrimSpace(anyString(raw["targetRef"])); targetRef != "" {
				if resolved, ok := refToEntryID[targetRef]; ok {
					targetID = resolved
				}
			}
			if targetID == "" {
				continue
			}
			label := anyString(raw["label"])
			entry, err := r.session.AppendLabel(targetID, label)
			if err != nil {
				return err
			}
			if ref != "" {
				refToEntryID[ref] = entry.ID
			}
		default:
			continue
		}
	}
	return nil
}

func anyString(value any) string {
	if value == nil {
		return ""
	}
	if s, ok := value.(string); ok {
		return s
	}
	return fmt.Sprint(value)
}

func anyMap(value any) map[string]any {
	v, ok := value.(map[string]any)
	if !ok || len(v) == 0 {
		return nil
	}
	out := make(map[string]any, len(v))
	for k, item := range v {
		out[k] = item
	}
	return out
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
			content := make([]types.ContentBlock, 0, len(action.Content))
			for _, block := range action.Content {
				content = append(content, block)
			}
			text := strings.TrimSpace(action.Text)
			if len(content) == 0 {
				if text == "" {
					continue
				}
				content = []types.ContentBlock{{Type: "text", Text: text}}
			}
			msg := types.Message{
				Role:      types.RoleUser,
				Timestamp: types.NowMillis(),
				Content:   content,
			}
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
			if err := r.SetThinkingLevel(level); err != nil {
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
			cancelled, err := r.startNewSession(
				ctx,
				strings.TrimSpace(action.ParentSession),
				action.SkipBeforeHooks,
				action.SetupEntries,
			)
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
			cancelled, err := r.switchSession(ctx, path, action.SkipBeforeHooks)
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
			cancelled, err := r.forkSession(ctx, entryID, action.SkipBeforeHooks)
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
		case "compact":
			if _, err := r.compactContext(ctx, strings.TrimSpace(action.CustomInstructions), strings.TrimSpace(action.RequestID)); err != nil {
				_, _ = r.emitEventBestEffort(ctx, extensionsidecar.Event{
					Type: "session_compact_error",
					Payload: map[string]any{
						"requestId": action.RequestID,
						"error":     err.Error(),
					},
				})
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

type ForkMessage struct {
	EntryID string `json:"entryId"`
	Text    string `json:"text"`
}

type SessionTokenStats struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Total      int64 `json:"total"`
}

type SessionStats struct {
	SessionFile       string            `json:"sessionFile,omitempty"`
	SessionID         string            `json:"sessionId"`
	UserMessages      int               `json:"userMessages"`
	AssistantMessages int               `json:"assistantMessages"`
	ToolCalls         int               `json:"toolCalls"`
	ToolResults       int               `json:"toolResults"`
	TotalMessages     int               `json:"totalMessages"`
	Tokens            SessionTokenStats `json:"tokens"`
	Cost              float64           `json:"cost"`
}

func (r *Runtime) Compact(customInstructions string, requestID string) (session.Entry, error) {
	return r.compactContext(context.Background(), strings.TrimSpace(customInstructions), strings.TrimSpace(requestID))
}

func (r *Runtime) NewSession(parentSession string, setupEntries []map[string]any) (bool, error) {
	return r.startNewSession(context.Background(), strings.TrimSpace(parentSession), false, setupEntries)
}

func (r *Runtime) SwitchSession(path string) (bool, error) {
	return r.switchSession(context.Background(), strings.TrimSpace(path), false)
}

func (r *Runtime) ForkSession(entryID string) (string, bool, error) {
	entryID = strings.TrimSpace(entryID)
	selected := r.userMessageTextByEntryID(entryID)
	cancelled, err := r.forkSession(context.Background(), entryID, false)
	return selected, cancelled, err
}

func (r *Runtime) SetSessionName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("session name is empty")
	}
	_, err := r.session.AppendSessionName(name)
	return err
}

func (r *Runtime) Messages() []types.Message {
	ctx := r.session.BuildContext(r.systemPrompt, r.session.LeafID(), r.currentToolsDefinitions())
	out := make([]types.Message, len(ctx.Messages))
	copy(out, ctx.Messages)
	return out
}

func (r *Runtime) LastAssistantText() (string, bool) {
	messages := r.Messages()
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != types.RoleAssistant {
			continue
		}
		return strings.TrimSpace(AssistantText(messages[i])), true
	}
	return "", false
}

func (r *Runtime) ForkMessages() []ForkMessage {
	branch := r.session.Branch(r.session.LeafID())
	out := make([]ForkMessage, 0, len(branch))
	for _, entry := range branch {
		if entry.Type != "message" || entry.Message == nil || entry.Message.Role != types.RoleUser {
			continue
		}
		text := strings.TrimSpace(AssistantText(*entry.Message))
		if text == "" {
			continue
		}
		out = append(out, ForkMessage{
			EntryID: entry.ID,
			Text:    text,
		})
	}
	return out
}

func (r *Runtime) SessionStats() SessionStats {
	stats := SessionStats{
		SessionFile: r.session.SessionFile(),
		SessionID:   r.session.SessionID(),
	}
	branch := r.session.Branch(r.session.LeafID())
	for _, entry := range branch {
		if entry.Type != "message" || entry.Message == nil {
			continue
		}
		message := entry.Message
		stats.TotalMessages++
		switch message.Role {
		case types.RoleUser:
			stats.UserMessages++
		case types.RoleAssistant:
			stats.AssistantMessages++
		case types.RoleTool:
			stats.ToolResults++
		}
		for _, block := range message.Content {
			if block.Type == "toolCall" {
				stats.ToolCalls++
			}
		}
		if message.Role != types.RoleAssistant {
			continue
		}
		stats.Tokens.Input += message.Usage.Input
		stats.Tokens.Output += message.Usage.Output
		stats.Tokens.CacheRead += message.Usage.CacheRead
		stats.Tokens.CacheWrite += message.Usage.CacheWrite
		if message.Usage.Total > 0 {
			stats.Tokens.Total += message.Usage.Total
		} else {
			stats.Tokens.Total += message.Usage.Input + message.Usage.Output + message.Usage.CacheRead + message.Usage.CacheWrite
		}
		stats.Cost += message.Usage.CostTotal
	}
	return stats
}

func (r *Runtime) userMessageTextByEntryID(entryID string) string {
	if entryID == "" {
		return ""
	}
	for _, entry := range r.session.Entries() {
		if entry.ID != entryID || entry.Type != "message" || entry.Message == nil {
			continue
		}
		if entry.Message.Role != types.RoleUser {
			return ""
		}
		return strings.TrimSpace(AssistantText(*entry.Message))
	}
	return ""
}

func (r *Runtime) startNewSession(
	ctx context.Context,
	parentSession string,
	skipBeforeHooks bool,
	setupEntries []map[string]any,
) (bool, error) {
	previousSessionFile := r.session.SessionFile()
	if !skipBeforeHooks && r.emitSessionBeforeSwitch(ctx, "new", "") {
		return true, nil
	}
	parentSession = strings.TrimSpace(parentSession)
	if err := r.session.CreateNew(r.session.CWD(), parentSession); err != nil {
		return false, err
	}
	currentModel := r.Model()
	if _, err := r.session.AppendModelChange(currentModel.Provider, currentModel.ID); err != nil {
		return false, err
	}
	if _, err := r.session.AppendThinkingLevel(r.ThinkingLevel()); err != nil {
		return false, err
	}
	if len(setupEntries) > 0 {
		if err := r.applySessionSetupEntries(setupEntries); err != nil {
			return false, err
		}
		if err := r.syncRuntimeFromSessionState(); err != nil {
			return false, err
		}
	}
	r.emitSessionSwitch(ctx, "new", previousSessionFile)
	if err := r.emitSessionStartEvent(r.sidecar); err != nil {
		return false, err
	}
	return false, nil
}

func (r *Runtime) switchSession(ctx context.Context, path string, skipBeforeHooks bool) (bool, error) {
	previousSessionFile := r.session.SessionFile()
	if !skipBeforeHooks && r.emitSessionBeforeSwitch(ctx, "resume", path) {
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

func (r *Runtime) forkSession(ctx context.Context, entryID string, skipBeforeHooks bool) (bool, error) {
	if !skipBeforeHooks && r.emitSessionBeforeFork(ctx, entryID) {
		return true, nil
	}
	branch := r.session.Branch(entryID)
	if len(branch) == 0 {
		return false, fmt.Errorf("cannot fork: entry %s not found", entryID)
	}
	previousSessionFile := r.session.SessionFile()
	parentSession := strings.TrimSpace(previousSessionFile)
	cwd := r.session.CWD()
	prevModel := r.Model()
	prevThinking := r.ThinkingLevel()

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
			if _, err := r.session.AppendCompactionWithDetails(e.Summary, e.FirstKeptEntry, e.TokensBefore, e.Details); err != nil {
				return false, err
			}
		case "branch_summary":
			if _, err := r.session.AppendBranchSummaryWithDetails(e.FromID, e.Summary, e.Details); err != nil {
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
	customInstructions := strings.TrimSpace(action.CustomInstructions)
	replaceInstructions := action.ReplaceInstructions
	label := strings.TrimSpace(action.Label)
	summaryText := strings.TrimSpace(action.Summary)
	summaryDetails := action.SummaryDetails
	if !action.SkipBeforeHooks {
		beforeResult := r.emitSessionBeforeTree(ctx, targetID, oldLeafID, action)
		if beforeResult != nil {
			if beforeResult.Cancel {
				return nil
			}
			if strings.TrimSpace(beforeResult.CustomInstructions) != "" {
				customInstructions = strings.TrimSpace(beforeResult.CustomInstructions)
			}
			if beforeResult.ReplaceInstructions != nil {
				replaceInstructions = *beforeResult.ReplaceInstructions
			}
			if strings.TrimSpace(beforeResult.Label) != "" {
				label = strings.TrimSpace(beforeResult.Label)
			}
			if beforeResult.Summary != nil && strings.TrimSpace(beforeResult.Summary.Summary) != "" {
				summaryText = strings.TrimSpace(beforeResult.Summary.Summary)
				summaryDetails = beforeResult.Summary.Details
			}
		}
	}
	if err := r.session.SetLeaf(targetID); err != nil {
		return err
	}
	var summaryEntryID string
	if summaryText != "" {
		entry, err := r.session.AppendBranchSummaryWithDetails(oldLeafID, summaryText, summaryDetails)
		if err != nil {
			return err
		}
		summaryEntryID = entry.ID
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
	if customInstructions != "" {
		payload["customInstructions"] = customInstructions
	}
	if replaceInstructions {
		payload["replaceInstructions"] = true
	}
	if label != "" {
		payload["label"] = label
	}
	if summaryEntryID != "" {
		payload["summaryEntry"] = map[string]any{
			"id":      summaryEntryID,
			"summary": summaryText,
			"details": summaryDetails,
		}
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

func (r *Runtime) emitSessionBeforeCompact(
	ctx context.Context,
	firstKeptEntryID string,
	tokensBefore int,
	customInstructions string,
) *extensionsidecar.SessionBeforeCompactEventResult {
	preparation := map[string]any{
		"firstKeptEntryId": firstKeptEntryID,
		"tokensBefore":     tokensBefore,
	}
	result, ok := r.emitEventBestEffort(ctx, extensionsidecar.Event{
		Type: "session_before_compact",
		Payload: map[string]any{
			"preparation":        preparation,
			"customInstructions": customInstructions,
			"branchEntries":      sessionEntriesMaps(r.session.Branch(r.session.LeafID())),
		},
	})
	if !ok {
		return nil
	}
	return result.SessionBeforeCompact
}

func (r *Runtime) emitSessionBeforeTree(
	ctx context.Context,
	targetID string,
	oldLeafID string,
	action extensionsidecar.HostAction,
) *extensionsidecar.SessionBeforeTreeEventResult {
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
	if !ok {
		return nil
	}
	return result.SessionBeforeTree
}

func (r *Runtime) syncRuntimeFromSessionState() error {
	ctx := r.session.BuildContext(r.systemPrompt, r.session.LeafID(), r.currentToolsDefinitions())
	if strings.TrimSpace(ctx.ThinkingLevel) != "" {
		r.mu.Lock()
		r.thinkingLevel = strings.TrimSpace(ctx.ThinkingLevel)
		r.mu.Unlock()
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
	r.mu.Lock()
	r.model = model
	r.mu.Unlock()
	return nil
}

func sessionHeaderMap(h session.Header) map[string]any {
	out := map[string]any{
		"type":      h.Type,
		"id":        h.ID,
		"timestamp": h.Timestamp,
		"cwd":       h.CWD,
	}
	if h.Version != 0 {
		out["version"] = h.Version
	}
	if strings.TrimSpace(h.ParentSession) != "" {
		out["parentSession"] = h.ParentSession
	}
	return out
}

func sessionEntriesMaps(entries []session.Entry) []map[string]any {
	if len(entries) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		b, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			continue
		}
		out = append(out, m)
	}
	return out
}

func extensionUIRequestPayload(req extensionsidecar.ExtensionUIRequest) map[string]any {
	payload := map[string]any{
		"id":     req.ID,
		"method": req.Method,
	}
	b, err := json.Marshal(req)
	if err != nil {
		return payload
	}
	var decoded map[string]any
	if err := json.Unmarshal(b, &decoded); err != nil || len(decoded) == 0 {
		return payload
	}
	return decoded
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

func normalizeQueueMode(value string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case queueModeAll:
		return queueModeAll, nil
	case queueModeOneAtATime, "one_at_a_time", "oneatatime":
		return queueModeOneAtATime, nil
	default:
		return "", fmt.Errorf("invalid queue mode: %s", value)
	}
}

func hasMessageContent(content []types.ContentBlock) bool {
	if len(content) == 0 {
		return false
	}
	for _, block := range content {
		switch block.Type {
		case "text":
			if strings.TrimSpace(block.Text) != "" {
				return true
			}
		default:
			return true
		}
	}
	return false
}

const extensionEventTimeout = 2 * time.Second
const extensionUIResponseTimeout = 5 * time.Second
const autoCompactionThresholdPercent = 85.0
const autoRetryMaxAttempts = 3
const autoRetryBaseDelay = 500 * time.Millisecond
const skippedToolCallReason = "Skipped due to queued user message."
const queueModeAll = "all"
const queueModeOneAtATime = "one-at-a-time"

var thinkingLevelCycle = []string{"low", "medium", "high"}
var retryableProviderErrorPattern = regexp.MustCompile(`overloaded|rate.?limit|too many requests|429|500|502|503|504|service.?unavailable|server error|internal error|connection.?error|connection.?refused|fetch failed|terminated|retry`)
var contextOverflowErrorPattern = regexp.MustCompile(`context.?window|context.?length|maximum context|prompt is too long|too many tokens|context overflow|token limit`)
