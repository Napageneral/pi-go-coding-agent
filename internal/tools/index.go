package tools

import (
	"context"
	"fmt"
	"sort"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

type ExternalExecutor func(ctx context.Context, name, callID string, args map[string]interface{}) (types.ToolResult, bool, error)

type Registry struct {
	tools       map[string]types.ToolExecutor
	definitions map[string]types.Tool
	externalDef map[string]struct{}
	external    ExternalExecutor
}

func NewRegistry(executors ...types.ToolExecutor) *Registry {
	r := &Registry{
		tools:       map[string]types.ToolExecutor{},
		definitions: map[string]types.Tool{},
		externalDef: map[string]struct{}{},
	}
	for _, ex := range executors {
		def := ex.Definition()
		r.tools[def.Name] = ex
		r.definitions[def.Name] = def
	}
	return r
}

func NewCodingRegistry(cwd string) *Registry {
	return NewRegistry(
		NewReadTool(cwd),
		NewWriteTool(cwd),
		NewEditTool(cwd),
		NewBashTool(cwd),
		NewLsTool(cwd),
		NewFindTool(cwd),
		NewGrepTool(cwd),
	)
}

func (r *Registry) Definitions() []types.Tool {
	names := make([]string, 0, len(r.definitions))
	for name := range r.definitions {
		names = append(names, name)
	}
	sort.Strings(names)
	defs := make([]types.Tool, 0, len(names))
	for _, name := range names {
		defs = append(defs, r.definitions[name])
	}
	return defs
}

func (r *Registry) RegisterDefinitions(defs []types.Tool) {
	for _, def := range defs {
		if def.Name == "" {
			continue
		}
		r.definitions[def.Name] = def
		r.externalDef[def.Name] = struct{}{}
	}
}

func (r *Registry) SetExternalExecutor(executor ExternalExecutor) {
	r.external = executor
}

func (r *Registry) Execute(ctx context.Context, name, callID string, args map[string]interface{}) (types.ToolResult, error) {
	if _, preferExternal := r.externalDef[name]; preferExternal && r.external != nil {
		result, handled, err := r.external(ctx, name, callID, args)
		if handled {
			return result, err
		}
		return types.ToolResult{IsError: true}, fmt.Errorf("external tool registered but not handled: %s", name)
	}

	if ex, ok := r.tools[name]; ok {
		return ex.Execute(ctx, callID, args)
	}
	if r.external != nil {
		result, handled, err := r.external(ctx, name, callID, args)
		if handled {
			return result, err
		}
	}
	return types.ToolResult{IsError: true}, fmt.Errorf("unknown tool: %s", name)
}
