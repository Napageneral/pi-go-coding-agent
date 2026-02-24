package tools

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

type ReadTool struct {
	cwd string
}

func NewReadTool(cwd string) *ReadTool { return &ReadTool{cwd: cwd} }

func (t *ReadTool) Definition() types.Tool {
	return types.Tool{
		Name:        "read",
		Label:       "read",
		Description: "Read a file from disk",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "Path to file"},
				"offset": map[string]any{"type": "integer", "description": "Start line number (1-indexed)"},
				"limit":  map[string]any{"type": "integer", "description": "Max lines", "default": 200},
			},
			"required": []string{"path"},
		},
	}
}

func (t *ReadTool) Execute(ctx context.Context, _ string, args map[string]interface{}) (types.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	path, _ := args["path"].(string)
	if path == "" {
		return types.ToolResult{IsError: true}, fmt.Errorf("missing path")
	}
	offsetProvided := false
	offset := 1
	if v, ok := args["offset"].(float64); ok {
		offsetProvided = true
		offset = int(v)
	}
	if v, ok := args["offset"].(string); ok && strings.TrimSpace(v) != "" {
		offsetProvided = true
		if parsed, err := strconv.Atoi(v); err == nil {
			offset = parsed
		}
	}
	limit := 200
	if v, ok := args["limit"].(float64); ok {
		limit = int(v)
	}
	if v, ok := args["limit"].(string); ok && strings.TrimSpace(v) != "" {
		if parsed, err := strconv.Atoi(v); err == nil {
			limit = parsed
		}
	}
	limit = clampInt(limit, 1, 2000)
	if offset < 1 {
		offset = 1
	}

	abs, err := resolvePath(t.cwd, path)
	if err != nil {
		return types.ToolResult{IsError: true}, err
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return types.ToolResult{IsError: true}, err
	}
	lines := strings.Split(string(b), "\n")
	start := offset - 1
	if offsetProvided && start >= len(lines) {
		return types.ToolResult{IsError: true}, fmt.Errorf("offset %d is beyond end of file (%d lines total)", offset, len(lines))
	}
	if start < 0 {
		start = 0
	}
	if start >= len(lines) {
		return types.ToolResult{Content: []types.ContentBlock{{Type: "text", Text: ""}}}, nil
	}
	end := start + limit
	if end > len(lines) {
		end = len(lines)
	}
	chunk := strings.Join(lines[start:end], "\n")
	if end < len(lines) {
		next := end + 1
		chunk += fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", start+1, end, len(lines), next)
	}
	return types.ToolResult{
		Content: []types.ContentBlock{{Type: "text", Text: chunk}},
		Details: map[string]any{
			"path":        abs,
			"offset":      start + 1,
			"limit":       limit,
			"total_lines": len(lines),
		},
	}, nil
}
