package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/config"
	"github.com/badlogic/pi-mono/go-coding-agent/pkg/providers"
	"github.com/badlogic/pi-mono/go-coding-agent/pkg/session"
	"github.com/badlogic/pi-mono/go-coding-agent/pkg/tools"
	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

type Runtime struct {
	session       *session.Manager
	tools         *tools.Registry
	modelRegistry *config.ModelRegistry
	auth          *config.AuthStorage
	model         types.Model
	thinkingLevel string
	systemPrompt  string
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

	// SessionManager allows injecting a pre-configured session manager
	// (e.g. an in-memory manager pre-seeded with conversation history).
	// When set, SessionDir/Session/NoSession are ignored.
	SessionManager *session.Manager
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

	var sm *session.Manager
	if opts.SessionManager != nil {
		sm = opts.SessionManager
	} else {
		sessDir := opts.SessionDir
		if sessDir == "" {
			sessDir = config.GetSessionsDirForCWD(absCWD)
		}
		sm = session.NewManager(sessDir)
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
		modelRegistry: registry,
		auth:          auth,
		model:         model,
		thinkingLevel: "medium",
		systemPrompt:  defaultSystemPrompt(opts.SystemPrompt),
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

func (r *Runtime) SetModel(provider, modelID string) error {
	model, err := r.modelRegistry.ResolveModel(provider, modelID)
	if err != nil {
		return err
	}
	r.model = model
	_, err = r.session.AppendModelChange(provider, modelID)
	return err
}

func (r *Runtime) Prompt(text string) (types.Message, error) {
	user := types.TextMessage(types.RoleUser, text)
	if _, err := r.session.AppendMessage(user); err != nil {
		return types.Message{}, err
	}
	return r.runLoop()
}

var ErrAborted = errors.New("run aborted")

func (r *Runtime) Continue() (types.Message, error) {
	ctx := r.session.BuildContext(r.systemPrompt, r.session.LeafID(), r.tools.Definitions())
	if len(ctx.Messages) == 0 {
		return types.Message{}, fmt.Errorf("cannot continue: no messages in session")
	}
	lastRole := ctx.Messages[len(ctx.Messages)-1].Role
	if lastRole != types.RoleUser && lastRole != types.RoleTool {
		return types.Message{}, fmt.Errorf("cannot continue from %s; last message must be user or toolResult", lastRole)
	}
	return r.runLoop()
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
	r.setAbort(cancel)
	defer func() {
		r.clearAbort(cancel)
		cancel()
	}()

	for i := 0; i < 128; i++ {
		if err := runCtx.Err(); err != nil {
			return lastAssistant, ErrAborted
		}
		ctxState := r.session.BuildContext(r.systemPrompt, r.session.LeafID(), r.tools.Definitions())
		ctx := types.Context{
			SystemPrompt: r.systemPrompt,
			Messages:     ctxState.Messages,
			Tools:        r.tools.Definitions(),
		}
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
		if _, err := r.session.AppendMessage(resp.Assistant); err != nil {
			return lastAssistant, err
		}
		if len(resp.ToolCalls) == 0 {
			return lastAssistant, nil
		}

		for _, call := range resp.ToolCalls {
			if err := runCtx.Err(); err != nil {
				return lastAssistant, ErrAborted
			}
			result, err := r.tools.Execute(runCtx, call.Name, call.ID, call.Arguments)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return lastAssistant, ErrAborted
				}
				result.IsError = true
				if len(result.Content) == 0 {
					result.Content = []types.ContentBlock{{Type: "text", Text: err.Error()}}
				}
			}
			toolMsg := types.ToolResultMessage(call.ID, call.Name, result)
			if _, err := r.session.AppendMessage(toolMsg); err != nil {
				return lastAssistant, err
			}
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
