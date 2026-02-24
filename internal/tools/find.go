package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

type FindTool struct{ cwd string }

func NewFindTool(cwd string) *FindTool { return &FindTool{cwd: cwd} }

func (t *FindTool) Definition() types.Tool {
	return types.Tool{
		Name:        "find",
		Label:       "find",
		Description: "Find files by glob-like pattern",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":    map[string]any{"type": "string", "default": "."},
				"pattern": map[string]any{"type": "string", "description": "substring or glob (*.go)"},
			},
			"required": []string{"pattern"},
		},
	}
}

func (t *FindTool) Execute(ctx context.Context, _ string, args map[string]interface{}) (types.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return types.ToolResult{IsError: true}, fmt.Errorf("missing pattern")
	}
	abs, err := resolvePath(t.cwd, path)
	if err != nil {
		return types.ToolResult{IsError: true}, err
	}

	matches := make([]string, 0)
	walkErr := filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(abs, p)
		if err != nil {
			return nil
		}
		if strings.Contains(pattern, "*") || strings.Contains(pattern, "?") {
			if ok, _ := filepath.Match(pattern, filepath.Base(rel)); ok {
				matches = append(matches, rel)
			}
			return nil
		}
		if strings.Contains(strings.ToLower(rel), strings.ToLower(pattern)) {
			matches = append(matches, rel)
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return types.ToolResult{IsError: true}, walkErr
	}
	if err := ctx.Err(); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	sort.Strings(matches)
	return types.ToolResult{Content: []types.ContentBlock{{Type: "text", Text: strings.Join(matches, "\n")}}, Details: map[string]any{"path": abs, "count": len(matches)}}, nil
}
