package tools

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

type EditTool struct{ cwd string }

func NewEditTool(cwd string) *EditTool { return &EditTool{cwd: cwd} }

func (t *EditTool) Definition() types.Tool {
	return types.Tool{
		Name:        "edit",
		Label:       "edit",
		Description: "Edit a file by replacing text",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"oldText": map[string]any{"type": "string"},
				"newText": map[string]any{"type": "string"},
			},
			"required": []string{"path", "oldText", "newText"},
		},
	}
}

func (t *EditTool) Execute(ctx context.Context, _ string, args map[string]interface{}) (types.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	path, _ := args["path"].(string)
	oldText, _ := args["oldText"].(string)
	newText, _ := args["newText"].(string)
	if path == "" {
		return types.ToolResult{IsError: true}, fmt.Errorf("missing path")
	}
	if oldText == "" {
		return types.ToolResult{IsError: true}, fmt.Errorf("oldText cannot be empty")
	}
	if oldText == newText {
		return types.ToolResult{IsError: true}, fmt.Errorf("replacement produced no changes")
	}
	abs, err := resolvePath(t.cwd, path)
	if err != nil {
		return types.ToolResult{IsError: true}, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return types.ToolResult{IsError: true}, err
	}
	content := string(b)
	count := strings.Count(content, oldText)
	if count == 0 {
		return types.ToolResult{IsError: true}, fmt.Errorf("oldText not found")
	}
	if count > 1 {
		return types.ToolResult{IsError: true}, fmt.Errorf("found %d occurrences; oldText must be unique", count)
	}
	updated := strings.Replace(content, oldText, newText, 1)
	if err := os.WriteFile(abs, []byte(updated), 0o644); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	return types.ToolResult{
		Content: []types.ContentBlock{{Type: "text", Text: "ok"}},
		Details: map[string]any{"path": abs, "replacements": 1},
	}, nil
}
