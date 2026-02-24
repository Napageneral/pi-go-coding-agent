package tools

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

const (
	defaultMaxBytes = 32 * 1024
)

type BashTool struct{ cwd string }

func NewBashTool(cwd string) *BashTool { return &BashTool{cwd: cwd} }

func (t *BashTool) Definition() types.Tool {
	return types.Tool{
		Name:        "bash",
		Label:       "bash",
		Description: "Execute a shell command in cwd",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
				"timeout": map[string]any{"type": "number", "description": "seconds", "default": 60},
			},
			"required": []string{"command"},
		},
	}
}

func (t *BashTool) Execute(ctx context.Context, _ string, args map[string]interface{}) (types.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return types.ToolResult{IsError: true}, err
	}
	command, _ := args["command"].(string)
	if command == "" {
		return types.ToolResult{IsError: true}, fmt.Errorf("missing command")
	}
	timeoutSec := 0.0
	if v, ok := args["timeout"].(float64); ok && v > 0 {
		timeoutSec = v
	}
	var cancel context.CancelFunc
	if timeoutSec > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	var shell string
	var shellArgs []string
	if runtime.GOOS == "windows" {
		shell = "cmd"
		shellArgs = []string{"/C", command}
	} else {
		shell = "bash"
		shellArgs = []string{"-lc", command}
	}
	cmd := exec.CommandContext(ctx, shell, shellArgs...)
	cmd.Dir = t.cwd

	var out bytes.Buffer
	var errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	err := cmd.Run()
	combined := out.String() + errOut.String()
	truncated := false
	if len(combined) > defaultMaxBytes {
		combined = combined[len(combined)-defaultMaxBytes:]
		truncated = true
	}

	details := map[string]any{
		"cwd":       t.cwd,
		"truncated": truncated,
	}
	if ctx.Err() == context.DeadlineExceeded {
		return types.ToolResult{IsError: true, Content: []types.ContentBlock{{Type: "text", Text: combined + "\n\nCommand timed out"}}, Details: details}, fmt.Errorf("command timed out")
	}
	if err != nil {
		return types.ToolResult{IsError: true, Content: []types.ContentBlock{{Type: "text", Text: combined}}, Details: details}, err
	}
	return types.ToolResult{Content: []types.ContentBlock{{Type: "text", Text: combined}}, Details: details}, nil
}
