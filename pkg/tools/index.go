package tools

import (
	"context"
	"fmt"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

type Registry struct {
	tools map[string]types.ToolExecutor
}

func NewRegistry(executors ...types.ToolExecutor) *Registry {
	r := &Registry{tools: map[string]types.ToolExecutor{}}
	for _, ex := range executors {
		r.tools[ex.Definition().Name] = ex
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
	defs := make([]types.Tool, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, t.Definition())
	}
	return defs
}

func (r *Registry) Execute(ctx context.Context, name, callID string, args map[string]interface{}) (types.ToolResult, error) {
	ex, ok := r.tools[name]
	if !ok {
		return types.ToolResult{IsError: true}, fmt.Errorf("unknown tool: %s", name)
	}
	return ex.Execute(ctx, callID, args)
}
