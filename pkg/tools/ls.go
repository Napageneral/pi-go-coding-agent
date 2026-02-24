package tools

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

type LsTool struct{ cwd string }

func NewLsTool(cwd string) *LsTool { return &LsTool{cwd: cwd} }

func (t *LsTool) Definition() types.Tool {
	return types.Tool{
		Name:        "ls",
		Label:       "ls",
		Description: "List files in a directory",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":      map[string]any{"type": "string", "default": "."},
				"recursive": map[string]any{"type": "boolean", "default": false},
			},
		},
	}
}

func (t *LsTool) Execute(ctx context.Context, _ string, args map[string]interface{}) (types.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	path, _ := args["path"].(string)
	if path == "" {
		path = "."
	}
	recursive := false
	if v, ok := args["recursive"].(bool); ok {
		recursive = v
	}
	abs, err := resolvePath(t.cwd, path)
	if err != nil {
		return types.ToolResult{IsError: true}, err
	}

	items := make([]string, 0)
	if recursive {
		err = filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			rel, err := filepath.Rel(abs, p)
			if err != nil {
				return nil
			}
			if rel == "." {
				return nil
			}
			if d.IsDir() {
				items = append(items, rel+"/")
			} else {
				items = append(items, rel)
			}
			return nil
		})
	} else {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return types.ToolResult{IsError: true}, err
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			items = append(items, name)
		}
	}
	if err != nil {
		return types.ToolResult{IsError: true}, err
	}
	sort.Strings(items)
	text := strings.Join(items, "\n")
	return types.ToolResult{Content: []types.ContentBlock{{Type: "text", Text: text}}, Details: map[string]any{"path": abs, "count": len(items)}}, nil
}
