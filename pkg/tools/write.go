package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

type WriteTool struct{ cwd string }

func NewWriteTool(cwd string) *WriteTool { return &WriteTool{cwd: cwd} }

func (t *WriteTool) Definition() types.Tool {
	return types.Tool{
		Name:        "write",
		Label:       "write",
		Description: "Write text content to a file",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			"required": []string{"path", "content"},
		},
	}
}

func (t *WriteTool) Execute(ctx context.Context, _ string, args map[string]interface{}) (types.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	if path == "" {
		return types.ToolResult{IsError: true}, fmt.Errorf("missing path")
	}
	abs, err := resolvePath(t.cwd, path)
	if err != nil {
		return types.ToolResult{IsError: true}, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	return types.ToolResult{
		Content: []types.ContentBlock{{Type: "text", Text: "ok"}},
		Details: map[string]any{"path": abs, "bytes": len(content)},
	}, nil
}
