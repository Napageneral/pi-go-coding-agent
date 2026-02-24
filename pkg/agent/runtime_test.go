package agent

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/config"
)

func TestRuntimeAbortCancelsProviderRequest(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(5 * time.Second):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[]},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
		}
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:        t.TempDir(),
		SessionDir: t.TempDir(),
		NoSession:  true,
		Provider:   "openai",
		Model:      "gpt-test",
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := rt.Prompt("hello")
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("provider request did not start in time")
	}

	rt.Abort()

	select {
	case err := <-done:
		if !errors.Is(err, ErrAborted) {
			t.Fatalf("expected ErrAborted, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not abort in time")
	}
}

func TestRuntimePromptToolLoopAndResume(t *testing.T) {
	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"note.txt\"}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		default:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"done","tool_calls":[]},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		}
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "note.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write note file: %v", err)
	}
	sessionDir := t.TempDir()

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:        cwd,
		SessionDir: sessionDir,
		Provider:   "openai",
		Model:      "gpt-test",
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}

	assistant, err := rt.Prompt("read the note")
	if err != nil {
		t.Fatalf("Prompt failed: %v", err)
	}
	if AssistantText(assistant) != "done" {
		t.Fatalf("assistant text = %q, want done", AssistantText(assistant))
	}
	if atomic.LoadInt32(&callCount) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", callCount)
	}
	sessionFile := rt.Session().SessionFile()
	if sessionFile == "" {
		t.Fatal("expected session file path")
	}
	if _, err := os.Stat(sessionFile); err != nil {
		t.Fatalf("session file not written: %v", err)
	}

	rt2, err := NewRuntime(NewRuntimeOptions{
		CWD:        cwd,
		SessionDir: sessionDir,
		Provider:   "openai",
		Model:      "gpt-test",
	})
	if err != nil {
		t.Fatalf("NewRuntime resume failed: %v", err)
	}
	if rt2.Session().SessionFile() != sessionFile {
		t.Fatalf("resume opened %q, want %q", rt2.Session().SessionFile(), sessionFile)
	}
	if _, err := rt2.Prompt("one more"); err != nil {
		t.Fatalf("resume Prompt failed: %v", err)
	}
}

func TestRuntimeContinueRequiresUserOrToolResultLeaf(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"done","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:        t.TempDir(),
		SessionDir: t.TempDir(),
		Provider:   "openai",
		Model:      "gpt-test",
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if _, err := rt.Prompt("hello"); err != nil {
		t.Fatalf("Prompt failed: %v", err)
	}
	_, err = rt.Continue()
	if err == nil {
		t.Fatal("expected Continue to fail from assistant leaf")
	}
	if !strings.Contains(err.Error(), "last message must be user or toolResult") {
		t.Fatalf("unexpected continue error: %v", err)
	}
}

func TestRuntimeNormalizesCWDForSessionDir(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	cwd := t.TempDir()
	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("Chdir temp cwd: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:      ".",
		Provider: "openai",
		Model:    "gpt-test",
	})
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	wdCWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd for expected cwd: %v", err)
	}
	expected := config.GetSessionsDirForCWD(wdCWD)
	if rt.Session().SessionDir() != expected {
		t.Fatalf("session dir = %q, want %q", rt.Session().SessionDir(), expected)
	}
}

func writeTestConfig(t *testing.T, agentDir, baseURL string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(agentDir, "auth.json"), []byte(`{
		"openai":{"type":"api_key","key":"test-key"}
	}`), 0o644); err != nil {
		t.Fatalf("write auth.json: %v", err)
	}
	modelsJSON := `{
		"providers": {
			"openai": {
				"api": "openai-completions",
				"models": [{
					"id": "gpt-test",
					"name": "gpt-test",
					"provider": "openai",
					"api": "openai-completions",
					"baseUrl": "` + baseURL + `",
					"reasoning": false,
					"input": ["text"],
					"contextWindow": 8192,
					"maxTokens": 512
				}]
			}
		}
	}`
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(modelsJSON), 0o644); err != nil {
		t.Fatalf("write models.json: %v", err)
	}
}
