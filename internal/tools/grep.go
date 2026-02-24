package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

type GrepTool struct{ cwd string }

func NewGrepTool(cwd string) *GrepTool { return &GrepTool{cwd: cwd} }

func (t *GrepTool) Definition() types.Tool {
	return types.Tool{
		Name:        "grep",
		Label:       "grep",
		Description: "Search for regex matches across files",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":       map[string]any{"type": "string", "default": "."},
				"pattern":    map[string]any{"type": "string", "description": "regex"},
				"ignoreCase": map[string]any{"type": "boolean", "default": false},
				"maxResults": map[string]any{"type": "integer", "default": 200},
			},
			"required": []string{"pattern"},
		},
	}
}

func (t *GrepTool) Execute(ctx context.Context, _ string, args map[string]interface{}) (types.ToolResult, error) {
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
	ignoreCase, _ := args["ignoreCase"].(bool)
	maxResults := 200
	if v, ok := args["maxResults"].(float64); ok {
		maxResults = int(v)
	}
	maxResults = clampInt(maxResults, 1, 5000)

	if ignoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return types.ToolResult{IsError: true}, err
	}

	abs, err := resolvePath(t.cwd, path)
	if err != nil {
		return types.ToolResult{IsError: true}, err
	}

	results := make([]string, 0, maxResults)
	stop := false
	walkErr := filepath.WalkDir(abs, func(p string, d os.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if stop || walkErr != nil || d.IsDir() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		rel, _ := filepath.Rel(abs, p)
		s := bufio.NewScanner(f)
		lineNo := 0
		for s.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
			lineNo++
			line := s.Text()
			if re.MatchString(line) {
				results = append(results, fmt.Sprintf("%s:%d:%s", rel, lineNo, line))
				if len(results) >= maxResults {
					stop = true
					return nil
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) {
		return types.ToolResult{IsError: true}, walkErr
	}
	if err := ctx.Err(); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	return types.ToolResult{Content: []types.ContentBlock{{Type: "text", Text: strings.Join(results, "\n")}}, Details: map[string]any{"count": len(results), "max": maxResults, "path": abs}}, nil
}
