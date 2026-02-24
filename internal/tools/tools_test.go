package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

func TestCoreTools(t *testing.T) {
	cwd := t.TempDir()
	reg := NewCodingRegistry(cwd)

	_, err := reg.Execute(context.Background(), "write", "c1", map[string]interface{}{
		"path":    "a/test.txt",
		"content": "hello\nworld\n",
	})
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	res, err := reg.Execute(context.Background(), "read", "c2", map[string]interface{}{
		"path":  "a/test.txt",
		"limit": float64(10),
	})
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if len(res.Content) == 0 || !strings.Contains(res.Content[0].Text, "hello") {
		t.Fatalf("unexpected read content: %#v", res.Content)
	}

	_, err = reg.Execute(context.Background(), "edit", "c3", map[string]interface{}{
		"path":    "a/test.txt",
		"oldText": "world",
		"newText": "go",
	})
	if err != nil {
		t.Fatalf("edit failed: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(cwd, "a", "test.txt"))
	if err != nil {
		t.Fatalf("read file failed: %v", err)
	}
	if !strings.Contains(string(b), "go") {
		t.Fatalf("edit did not apply: %s", string(b))
	}

	grepRes, err := reg.Execute(context.Background(), "grep", "c4", map[string]interface{}{
		"path":       ".",
		"pattern":    "go",
		"maxResults": float64(20),
	})
	if err != nil {
		t.Fatalf("grep failed: %v", err)
	}
	if len(grepRes.Content) == 0 || !strings.Contains(grepRes.Content[0].Text, "test.txt") {
		t.Fatalf("unexpected grep output: %v", grepRes.Content)
	}

	findRes, err := reg.Execute(context.Background(), "find", "c5", map[string]interface{}{
		"path":    ".",
		"pattern": "*.txt",
	})
	if err != nil {
		t.Fatalf("find failed: %v", err)
	}
	if len(findRes.Content) == 0 || !strings.Contains(findRes.Content[0].Text, "test.txt") {
		t.Fatalf("unexpected find output: %v", findRes.Content)
	}

	lsRes, err := reg.Execute(context.Background(), "ls", "c6", map[string]interface{}{"path": ".", "recursive": true})
	if err != nil {
		t.Fatalf("ls failed: %v", err)
	}
	if len(lsRes.Content) == 0 || !strings.Contains(lsRes.Content[0].Text, "a/") {
		t.Fatalf("unexpected ls output: %v", lsRes.Content)
	}
}

func TestEditRequiresUniqueMatch(t *testing.T) {
	cwd := t.TempDir()
	reg := NewCodingRegistry(cwd)

	_, err := reg.Execute(context.Background(), "write", "w1", map[string]interface{}{
		"path":    "dup.txt",
		"content": "one\ntwo\none\n",
	})
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	_, err = reg.Execute(context.Background(), "edit", "e1", map[string]interface{}{
		"path":    "dup.txt",
		"oldText": "one",
		"newText": "ONE",
	})
	if err == nil {
		t.Fatal("expected edit to fail on ambiguous oldText, got nil error")
	}
}

func TestReadOffsetIsOneIndexed(t *testing.T) {
	cwd := t.TempDir()
	reg := NewCodingRegistry(cwd)

	_, err := reg.Execute(context.Background(), "write", "w2", map[string]interface{}{
		"path":    "o.txt",
		"content": "a\nb\nc\n",
	})
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}

	res, err := reg.Execute(context.Background(), "read", "r1", map[string]interface{}{
		"path":   "o.txt",
		"offset": float64(2),
		"limit":  float64(1),
	})
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if len(res.Content) == 0 || !strings.HasPrefix(res.Content[0].Text, "b") {
		t.Fatalf("expected read offset=2 to start at line b, got: %#v", res.Content)
	}
}

func TestExternalDefinitionOverridesBuiltInTool(t *testing.T) {
	cwd := t.TempDir()
	reg := NewCodingRegistry(cwd)

	reg.RegisterDefinitions([]types.Tool{
		{
			Name:        "read",
			Description: "Extension read override",
		},
	})
	reg.SetExternalExecutor(func(ctx context.Context, name, callID string, args map[string]interface{}) (types.ToolResult, bool, error) {
		if name != "read" {
			return types.ToolResult{}, false, nil
		}
		return types.ToolResult{
			Content: []types.ContentBlock{{Type: "text", Text: "external-read"}},
			IsError: false,
		}, true, nil
	})

	res, err := reg.Execute(context.Background(), "read", "ext-read-1", map[string]interface{}{"path": "does-not-matter"})
	if err != nil {
		t.Fatalf("read override failed: %v", err)
	}
	if len(res.Content) == 0 || res.Content[0].Text != "external-read" {
		t.Fatalf("unexpected read override output: %#v", res.Content)
	}
}
