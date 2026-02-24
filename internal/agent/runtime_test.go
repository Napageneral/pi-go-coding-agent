package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/config"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/extensionsidecar"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
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

func TestRuntimeWithExtensionSidecarInputTransformAndToolExecution(t *testing.T) {
	var callCount int32
	var firstBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := atomic.AddInt32(&callCount, 1)
		if call == 1 {
			firstBody.Store(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_sidecar_1","type":"function","function":{"name":"helper_echo","arguments":"{\"text\":\"from provider\"}"}}]},"finish_reason":"tool_calls"}],
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

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: os.Args[0],
		ExtensionSidecarArgs:    []string{"-test.run=TestRuntimeSidecarHelperProcess"},
		ExtensionSidecarEnv:     []string{"GO_WANT_RUNTIME_SIDECAR_HELPER=1"},
	})
	if err != nil {
		t.Fatalf("NewRuntime with sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	assistant, err := rt.Prompt("hello runtime")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if AssistantText(assistant) != "done" {
		t.Fatalf("assistant text = %q, want done", AssistantText(assistant))
	}
	if atomic.LoadInt32(&callCount) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", callCount)
	}
	body, _ := firstBody.Load().(string)
	if !strings.Contains(body, "hello runtime [runtime-hook]") {
		t.Fatalf("expected transformed input in provider payload, got: %s", body)
	}
	if !strings.Contains(body, "runtime-context-prompt") {
		t.Fatalf("expected sidecar-updated context system prompt in provider payload, got: %s", body)
	}
}

func TestRuntimeSteerSkipsRemainingTools(t *testing.T) {
	var callCount int32
	var secondBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := atomic.AddInt32(&callCount, 1)
		if call == 2 {
			secondBody.Store(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"","tool_calls":[
					{"id":"call_steer_1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"sleep 0.4; echo one\",\"timeout\":2}"}},
					{"id":"call_steer_2","type":"function","function":{"name":"bash","arguments":"{\"command\":\"echo two\",\"timeout\":2}"}}
				]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		default:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"steer-complete","tool_calls":[]},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
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

	done := make(chan struct{})
	var assistant types.Message
	var promptErr error
	go func() {
		assistant, promptErr = rt.Prompt("start steering test")
		close(done)
	}()

	time.Sleep(120 * time.Millisecond)
	if err := rt.Steer("interrupt now"); err != nil {
		t.Fatalf("Steer: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt did not finish in time")
	}
	if promptErr != nil {
		t.Fatalf("Prompt: %v", promptErr)
	}
	if AssistantText(assistant) != "steer-complete" {
		t.Fatalf("assistant text = %q, want steer-complete", AssistantText(assistant))
	}
	if atomic.LoadInt32(&callCount) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", callCount)
	}

	body, _ := secondBody.Load().(string)
	if !strings.Contains(body, "interrupt now") {
		t.Fatalf("expected steering message in follow-up provider payload, got: %s", body)
	}

	var sawSkipped bool
	for _, e := range rt.Session().Entries() {
		if e.Type != "message" || e.Message == nil {
			continue
		}
		if e.Message.Role != types.RoleTool || e.Message.ToolCallID != "call_steer_2" {
			continue
		}
		for _, block := range e.Message.Content {
			if block.Type == "text" && strings.Contains(block.Text, skippedToolCallReason) {
				sawSkipped = true
			}
		}
	}
	if !sawSkipped {
		t.Fatalf("expected skipped tool result for call_steer_2 in session entries")
	}
}

func TestRuntimeFollowUpRunsAdditionalTurn(t *testing.T) {
	var callCount int32
	var secondBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := atomic.AddInt32(&callCount, 1)
		if call == 2 {
			secondBody.Store(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"first-turn","tool_calls":[]},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		default:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"follow-up-turn","tool_calls":[]},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
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
	if err := rt.FollowUp("queued follow-up message"); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}

	assistant, err := rt.Prompt("initial prompt")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if AssistantText(assistant) != "follow-up-turn" {
		t.Fatalf("assistant text = %q, want follow-up-turn", AssistantText(assistant))
	}
	if atomic.LoadInt32(&callCount) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", callCount)
	}
	body, _ := secondBody.Load().(string)
	if !strings.Contains(body, "queued follow-up message") {
		t.Fatalf("expected queued follow-up message in second provider payload, got: %s", body)
	}
}

func TestRuntimeContinueAllowsQueuedFollowUpFromAssistantLeaf(t *testing.T) {
	var callCount int32
	var secondBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := atomic.AddInt32(&callCount, 1)
		if call == 2 {
			secondBody.Store(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"first","tool_calls":[]},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		default:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"second","tool_calls":[]},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
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
	if _, err := rt.Prompt("first prompt"); err != nil {
		t.Fatalf("Prompt first: %v", err)
	}
	if err := rt.FollowUp("queued via continue"); err != nil {
		t.Fatalf("FollowUp: %v", err)
	}

	assistant, err := rt.Continue()
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if AssistantText(assistant) != "second" {
		t.Fatalf("assistant text = %q, want second", AssistantText(assistant))
	}
	if atomic.LoadInt32(&callCount) < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", callCount)
	}
	body, _ := secondBody.Load().(string)
	if !strings.Contains(body, "queued via continue") {
		t.Fatalf("expected queued follow-up message in continue provider payload, got: %s", body)
	}
}

func TestRuntimePromptExecutesSidecarCommand(t *testing.T) {
	serverCalls := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&serverCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"unexpected","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: os.Args[0],
		ExtensionSidecarArgs:    []string{"-test.run=TestRuntimeSidecarHelperProcess"},
		ExtensionSidecarEnv:     []string{"GO_WANT_RUNTIME_SIDECAR_HELPER=1"},
	})
	if err != nil {
		t.Fatalf("NewRuntime with sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	assistant, err := rt.Prompt("/ping hello")
	if err != nil {
		t.Fatalf("Prompt command: %v", err)
	}
	if AssistantText(assistant) != "pong:hello" {
		t.Fatalf("assistant text = %q, want pong:hello", AssistantText(assistant))
	}
	if got := atomic.LoadInt32(&serverCalls); got != 0 {
		t.Fatalf("expected provider not to be called for extension command, got %d", got)
	}
}

func TestRuntimeRegistersSidecarProviderAndModel(t *testing.T) {
	serverCalls := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&serverCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"provider-ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: os.Args[0],
		ExtensionSidecarArgs:    []string{"-test.run=TestRuntimeSidecarHelperProcess"},
		ExtensionSidecarEnv:     []string{"GO_WANT_RUNTIME_SIDECAR_HELPER=1"},
		ExtensionFlagValues: map[string]any{
			"helper_base_url": server.URL,
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime with sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	if err := rt.SetModel("runtime-helper-provider", "runtime-helper-model"); err != nil {
		t.Fatalf("SetModel runtime-helper-provider: %v", err)
	}
	assistant, err := rt.Prompt("use runtime helper provider")
	if err != nil {
		t.Fatalf("Prompt runtime helper provider: %v", err)
	}
	if AssistantText(assistant) != "provider-ok" {
		t.Fatalf("assistant text = %q, want provider-ok", AssistantText(assistant))
	}
	if got := atomic.LoadInt32(&serverCalls); got == 0 {
		t.Fatalf("expected helper provider server to be called")
	}
}

func TestRuntimeCloseEmitsSessionShutdownEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	markerPath := filepath.Join(t.TempDir(), "session-shutdown.marker")
	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: os.Args[0],
		ExtensionSidecarArgs:    []string{"-test.run=TestRuntimeSidecarHelperProcess"},
		ExtensionSidecarEnv: []string{
			"GO_WANT_RUNTIME_SIDECAR_HELPER=1",
			"GO_RUNTIME_SIDECAR_SHUTDOWN_MARKER=" + markerPath,
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime with sidecar: %v", err)
	}

	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		_, err := os.Stat(markerPath)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat shutdown marker: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected sidecar to receive session_shutdown and write marker %q", markerPath)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRuntimeWithNodeExtensionFixtureEndToEnd(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "upstream_style_extension.mjs")

	var callCount int32
	var firstBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := atomic.AddInt32(&callCount, 1)
		if call == 1 {
			firstBody.Store(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_fixture_1","type":"function","function":{"name":"fixture_tool","arguments":"{\"text\":\"from-provider\"}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		default:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"fixture-done","tool_calls":[]},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		}
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
		ExtensionFlagValues: map[string]any{
			"fixture-mode":     "transform",
			"fixture-base-url": server.URL,
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	if err := rt.SetModel("fixture-provider", "fixture-model"); err != nil {
		t.Fatalf("SetModel fixture-provider: %v", err)
	}

	cmdAssistant, err := rt.Prompt("/ping hello")
	if err != nil {
		t.Fatalf("Prompt /ping: %v", err)
	}
	if AssistantText(cmdAssistant) != "pong:hello:transform" {
		t.Fatalf("command assistant text = %q, want pong:hello:transform", AssistantText(cmdAssistant))
	}

	assistant, err := rt.Prompt("run fixture flow")
	if err != nil {
		t.Fatalf("Prompt fixture flow: %v", err)
	}
	if AssistantText(assistant) != "fixture-done" {
		t.Fatalf("assistant text = %q, want fixture-done", AssistantText(assistant))
	}
	body, _ := firstBody.Load().(string)
	if !strings.Contains(body, "run fixture flow [fixture-input]") {
		t.Fatalf("expected transformed input in provider payload, got: %s", body)
	}
	if !strings.Contains(body, "[fixture-context]") {
		t.Fatalf("expected context-transformed system prompt in provider payload, got: %s", body)
	}

	var sawToolResultOverride bool
	for _, e := range rt.Session().Entries() {
		if e.Type != "message" || e.Message == nil || e.Message.Role != types.RoleTool || e.Message.ToolCallID != "call_fixture_1" {
			continue
		}
		for _, block := range e.Message.Content {
			if block.Type == "text" && strings.Contains(block.Text, "fixture tool result override") {
				sawToolResultOverride = true
			}
		}
	}
	if !sawToolResultOverride {
		t.Fatalf("expected tool_result override content in session entries")
	}

	diagAssistant, err := rt.Prompt("/diag")
	if err != nil {
		t.Fatalf("Prompt /diag: %v", err)
	}
	var diag struct {
		SawModelSelect        bool `json:"sawModelSelect"`
		SawMessageEventShape  bool `json:"sawMessageEventShape"`
		SawToolExecutionShape bool `json:"sawToolExecutionShape"`
		SawToolCallShape      bool `json:"sawToolCallShape"`
	}
	if err := json.Unmarshal([]byte(AssistantText(diagAssistant)), &diag); err != nil {
		t.Fatalf("decode /diag JSON: %v", err)
	}
	if !diag.SawModelSelect {
		t.Fatalf("expected model_select event shape to be observed")
	}
	if !diag.SawMessageEventShape {
		t.Fatalf("expected message_start event shape to be observed")
	}
	if !diag.SawToolExecutionShape {
		t.Fatalf("expected tool_execution_start event shape to be observed")
	}
	if !diag.SawToolCallShape {
		t.Fatalf("expected tool_call event shape to be observed")
	}
}

func TestRuntimeExtensionToolCanOverrideBuiltInTool(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "override_read_extension.mjs")

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_override_1","type":"function","function":{"name":"read","arguments":"{\"path\":\"note.txt\"}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		default:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"override-done","tool_calls":[]},"finish_reason":"stop"}],
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
	if err := os.WriteFile(filepath.Join(cwd, "note.txt"), []byte("builtin-read-source"), 0o644); err != nil {
		t.Fatalf("write note file: %v", err)
	}

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     cwd,
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	assistant, err := rt.Prompt("read note with extension override")
	if err != nil {
		t.Fatalf("Prompt override: %v", err)
	}
	if AssistantText(assistant) != "override-done" {
		t.Fatalf("assistant text = %q, want override-done", AssistantText(assistant))
	}

	var sawOverrideResult bool
	for _, e := range rt.Session().Entries() {
		if e.Type != "message" || e.Message == nil || e.Message.Role != types.RoleTool || e.Message.ToolCallID != "call_override_1" {
			continue
		}
		for _, block := range e.Message.Content {
			if block.Type == "text" && strings.Contains(block.Text, "extension-read:note.txt") {
				sawOverrideResult = true
			}
		}
	}
	if !sawOverrideResult {
		t.Fatalf("expected extension read override content in tool result message")
	}
}

func TestRuntimeExtensionCommandSendUserMessageTriggersTurn(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "send_user_message_extension.mjs")

	var callCount int32
	var firstBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if atomic.AddInt32(&callCount, 1) == 1 {
			firstBody.Store(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"extension-command-done","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	assistant, err := rt.Prompt("/ask summarize extension action bridge")
	if err != nil {
		t.Fatalf("Prompt /ask: %v", err)
	}
	if AssistantText(assistant) != "extension-command-done" {
		t.Fatalf("assistant text = %q, want extension-command-done", AssistantText(assistant))
	}
	if got := atomic.LoadInt32(&callCount); got != 1 {
		t.Fatalf("expected provider to be called once, got %d", got)
	}
	body, _ := firstBody.Load().(string)
	if !strings.Contains(body, "summarize extension action bridge") {
		t.Fatalf("expected extension command message in provider payload, got: %s", body)
	}
}

func TestRuntimeExtensionCommandSendUserMessageSupportsImageContent(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "send_user_message_extension.mjs")

	var callCount int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"extension-image-done","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	assistant, err := rt.Prompt("/askwithimage summarize screenshot")
	if err != nil {
		t.Fatalf("Prompt /askwithimage: %v", err)
	}
	if AssistantText(assistant) != "extension-image-done" {
		t.Fatalf("assistant text = %q, want extension-image-done", AssistantText(assistant))
	}
	if got := atomic.LoadInt32(&callCount); got != 1 {
		t.Fatalf("expected provider to be called once, got %d", got)
	}

	var sawUserImage bool
	for _, e := range rt.Session().Entries() {
		if e.Type != "message" || e.Message == nil || e.Message.Role != types.RoleUser {
			continue
		}
		var sawText bool
		var sawImage bool
		for _, block := range e.Message.Content {
			if block.Type == "text" && strings.Contains(block.Text, "summarize screenshot") {
				sawText = true
			}
			if block.Type == "image" && block.Data == "aGVsbG8=" && block.MimeType == "image/png" {
				sawImage = true
			}
		}
		if sawText && sawImage {
			sawUserImage = true
			break
		}
	}
	if !sawUserImage {
		t.Fatalf("expected user message with text+image content from pi.sendUserMessage(...)")
	}
}

func TestRuntimeExtensionCommandPersistsMetadataActions(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "action_bridge_extension.mjs")

	serverCalls := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&serverCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"unexpected","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	assistant, err := rt.Prompt("/meta")
	if err != nil {
		t.Fatalf("Prompt /meta: %v", err)
	}
	if AssistantText(assistant) != "meta-ok" {
		t.Fatalf("assistant text = %q, want meta-ok", AssistantText(assistant))
	}
	if got := atomic.LoadInt32(&serverCalls); got != 0 {
		t.Fatalf("expected provider not to be called for /meta, got %d", got)
	}
	if got := rt.Session().SessionName(); got != "bridge-session" {
		t.Fatalf("session name = %q, want bridge-session", got)
	}

	var foundCustom bool
	for _, e := range rt.Session().Entries() {
		if e.Type != "custom" || e.CustomType != "ext.state" {
			continue
		}
		if e.CustomData["foo"] == "bar" {
			foundCustom = true
			break
		}
	}
	if !foundCustom {
		t.Fatalf("expected custom action entry ext.state{foo:bar} in session")
	}
}

func TestRuntimeExtensionCommandSetActiveToolsRestrictsExecution(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "action_bridge_extension.mjs")

	var callCount int32
	var firstBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		call := atomic.AddInt32(&callCount, 1)
		if call == 1 {
			firstBody.Store(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 1:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_restrict_1","type":"function","function":{"name":"write","arguments":"{\"path\":\"x.txt\",\"content\":\"data\"}"}}]},"finish_reason":"tool_calls"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		default:
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"restricted-done","tool_calls":[]},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		}
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	restrictAssistant, err := rt.Prompt("/restrict read")
	if err != nil {
		t.Fatalf("Prompt /restrict: %v", err)
	}
	if AssistantText(restrictAssistant) != "restrict-ok:read" {
		t.Fatalf("assistant text = %q, want restrict-ok:read", AssistantText(restrictAssistant))
	}

	assistant, err := rt.Prompt("trigger tool call")
	if err != nil {
		t.Fatalf("Prompt after /restrict: %v", err)
	}
	if AssistantText(assistant) != "restricted-done" {
		t.Fatalf("assistant text = %q, want restricted-done", AssistantText(assistant))
	}
	if got := atomic.LoadInt32(&callCount); got < 2 {
		t.Fatalf("expected at least 2 provider calls, got %d", got)
	}
	body, _ := firstBody.Load().(string)
	if !strings.Contains(body, `"name":"read"`) {
		t.Fatalf("expected read to remain active in provider tool list, got: %s", body)
	}
	if strings.Contains(body, `"name":"write"`) {
		t.Fatalf("expected write to be excluded from provider tool list, got: %s", body)
	}

	var sawInactiveError bool
	for _, e := range rt.Session().Entries() {
		if e.Type != "message" || e.Message == nil || e.Message.Role != types.RoleTool || e.Message.ToolCallID != "call_restrict_1" {
			continue
		}
		for _, block := range e.Message.Content {
			if block.Type == "text" && strings.Contains(block.Text, "tool is not active: write") {
				sawInactiveError = true
			}
		}
	}
	if !sawInactiveError {
		t.Fatalf("expected inactive tool error result for write")
	}
}

func TestRuntimeExtensionCommandSessionControlActions(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "session_control_extension.mjs")

	var providerCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	sessionDir := t.TempDir()
	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              sessionDir,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	if _, err := rt.Prompt("prime old session"); err != nil {
		t.Fatalf("Prompt prime old session: %v", err)
	}
	oldSessionPath := rt.Session().SessionFile()
	if oldSessionPath == "" {
		t.Fatal("expected old session path")
	}
	var oldUserEntryID string
	for _, e := range rt.Session().Entries() {
		if e.Type != "message" || e.Message == nil || e.Message.Role != types.RoleUser {
			continue
		}
		if strings.Contains(AssistantText(*e.Message), "prime old session") {
			oldUserEntryID = e.ID
			break
		}
	}
	if oldUserEntryID == "" {
		t.Fatal("failed to locate old user entry id")
	}

	newSessionAssistant, err := rt.Prompt("/newsession")
	if err != nil {
		t.Fatalf("Prompt /newsession: %v", err)
	}
	if AssistantText(newSessionAssistant) != "new-session-ok" {
		t.Fatalf("assistant text = %q, want new-session-ok", AssistantText(newSessionAssistant))
	}
	if rt.Session().SessionFile() == oldSessionPath {
		t.Fatalf("expected new session path after /newsession")
	}

	if _, err := rt.Prompt("in new session"); err != nil {
		t.Fatalf("Prompt in new session: %v", err)
	}

	switchAssistant, err := rt.Prompt("/switchsession " + oldSessionPath)
	if err != nil {
		t.Fatalf("Prompt /switchsession: %v", err)
	}
	if AssistantText(switchAssistant) != "switch-session-ok" {
		t.Fatalf("assistant text = %q, want switch-session-ok", AssistantText(switchAssistant))
	}
	if rt.Session().SessionFile() != oldSessionPath {
		t.Fatalf("expected switched session path %q, got %q", oldSessionPath, rt.Session().SessionFile())
	}

	navAssistant, err := rt.Prompt("/navigate " + oldUserEntryID)
	if err != nil {
		t.Fatalf("Prompt /navigate: %v", err)
	}
	if AssistantText(navAssistant) != "navigate-ok" {
		t.Fatalf("assistant text = %q, want navigate-ok", AssistantText(navAssistant))
	}
	branch := rt.Session().Branch(rt.Session().LeafID())
	var foundNavigateTarget bool
	for _, entry := range branch {
		if entry.ID == oldUserEntryID {
			foundNavigateTarget = true
			break
		}
	}
	if !foundNavigateTarget {
		t.Fatalf("expected branch to include navigate target entry %q", oldUserEntryID)
	}

	forkAssistant, err := rt.Prompt("/forkat " + oldUserEntryID)
	if err != nil {
		t.Fatalf("Prompt /forkat: %v", err)
	}
	if AssistantText(forkAssistant) != "fork-ok" {
		t.Fatalf("assistant text = %q, want fork-ok", AssistantText(forkAssistant))
	}
	if rt.Session().SessionFile() == oldSessionPath {
		t.Fatalf("expected new forked session path, got same as old session")
	}
	ctx := rt.Session().BuildContext("", rt.Session().LeafID(), nil)
	var sawPrimeMessage bool
	for _, msg := range ctx.Messages {
		if msg.Role != types.RoleUser {
			continue
		}
		if strings.Contains(AssistantText(msg), "prime old session") {
			sawPrimeMessage = true
			break
		}
	}
	if !sawPrimeMessage {
		t.Fatalf("expected forked session to include prime old session message")
	}

	reloadAssistant, err := rt.Prompt("/reloadcmd")
	if err != nil {
		t.Fatalf("Prompt /reloadcmd: %v", err)
	}
	if AssistantText(reloadAssistant) != "reload-ok" {
		t.Fatalf("assistant text = %q, want reload-ok", AssistantText(reloadAssistant))
	}

	waitAssistant, err := rt.Prompt("/waitcmd")
	if err != nil {
		t.Fatalf("Prompt /waitcmd: %v", err)
	}
	if AssistantText(waitAssistant) != "wait-ok" {
		t.Fatalf("assistant text = %q, want wait-ok", AssistantText(waitAssistant))
	}

	if got := atomic.LoadInt32(&providerCalls); got != 2 {
		t.Fatalf("expected 2 provider calls from non-command prompts, got %d", got)
	}
}

func TestRuntimeExtensionSessionParityHooksAndStateSync(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "session_parity_extension.mjs")

	var providerCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	sessionDir := t.TempDir()
	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              sessionDir,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	parseSessionInfo := func(text string) (string, string, string) {
		parts := strings.SplitN(strings.TrimSpace(text), "|", 3)
		for len(parts) < 3 {
			parts = append(parts, "")
		}
		return parts[0], parts[1], parts[2]
	}
	findLatestAssistantParent := func(contains string) string {
		entries := rt.Session().Entries()
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if e.Type != "message" || e.Message == nil || e.Message.Role != types.RoleAssistant {
				continue
			}
			if strings.Contains(AssistantText(*e.Message), contains) {
				return e.ParentID
			}
		}
		return ""
	}

	infoStart, err := rt.Prompt("/sessioninfo")
	if err != nil {
		t.Fatalf("Prompt /sessioninfo start: %v", err)
	}
	startSessionID, startSessionPath, _ := parseSessionInfo(AssistantText(infoStart))
	if startSessionID != rt.Session().SessionID() {
		t.Fatalf("start session id = %q, want %q", startSessionID, rt.Session().SessionID())
	}
	if startSessionPath != rt.Session().SessionFile() {
		t.Fatalf("start session path = %q, want %q", startSessionPath, rt.Session().SessionFile())
	}

	if _, err := rt.Prompt("seed parity session"); err != nil {
		t.Fatalf("Prompt seed parity session: %v", err)
	}
	oldSessionPath := rt.Session().SessionFile()
	if oldSessionPath == "" {
		t.Fatal("expected old session path")
	}

	var seedUserEntryID string
	for _, e := range rt.Session().Entries() {
		if e.Type != "message" || e.Message == nil || e.Message.Role != types.RoleUser {
			continue
		}
		if strings.Contains(AssistantText(*e.Message), "seed parity session") {
			seedUserEntryID = e.ID
			break
		}
	}
	if seedUserEntryID == "" {
		t.Fatal("failed to locate seed user entry id")
	}

	if _, err := rt.Prompt("/armcancelswitch"); err != nil {
		t.Fatalf("Prompt /armcancelswitch: %v", err)
	}
	cancelledNew, err := rt.Prompt("/newsession")
	if err != nil {
		t.Fatalf("Prompt /newsession (cancelled): %v", err)
	}
	if AssistantText(cancelledNew) != "new-session-cancelled" {
		t.Fatalf("assistant text = %q, want new-session-cancelled", AssistantText(cancelledNew))
	}
	if rt.Session().SessionFile() != oldSessionPath {
		t.Fatalf("expected cancelled /newsession to keep session path %q, got %q", oldSessionPath, rt.Session().SessionFile())
	}

	newSessionAssistant, err := rt.Prompt("/newsession")
	if err != nil {
		t.Fatalf("Prompt /newsession: %v", err)
	}
	if AssistantText(newSessionAssistant) != "new-session-ok" {
		t.Fatalf("assistant text = %q, want new-session-ok", AssistantText(newSessionAssistant))
	}
	newSessionPath := rt.Session().SessionFile()
	if newSessionPath == oldSessionPath {
		t.Fatalf("expected /newsession to switch session path, got unchanged %q", newSessionPath)
	}
	infoAfterNew, err := rt.Prompt("/sessioninfo")
	if err != nil {
		t.Fatalf("Prompt /sessioninfo after /newsession: %v", err)
	}
	afterNewSessionID, afterNewSessionPath, _ := parseSessionInfo(AssistantText(infoAfterNew))
	if afterNewSessionID != rt.Session().SessionID() {
		t.Fatalf("sessioninfo id after /newsession = %q, want %q", afterNewSessionID, rt.Session().SessionID())
	}
	if afterNewSessionPath != newSessionPath {
		t.Fatalf("sessioninfo path after /newsession = %q, want %q", afterNewSessionPath, newSessionPath)
	}
	if got := strings.TrimSpace(rt.Session().Header().ParentSession); got != "" {
		t.Fatalf("expected /newsession default parentSession to be empty, got %q", got)
	}

	newSessionParentAssistant, err := rt.Prompt("/newsessionparent " + oldSessionPath)
	if err != nil {
		t.Fatalf("Prompt /newsessionparent: %v", err)
	}
	if AssistantText(newSessionParentAssistant) != "new-session-parent-ok" {
		t.Fatalf("assistant text = %q, want new-session-parent-ok", AssistantText(newSessionParentAssistant))
	}
	if got := strings.TrimSpace(rt.Session().Header().ParentSession); got != oldSessionPath {
		t.Fatalf("expected /newsessionparent parentSession %q, got %q", oldSessionPath, got)
	}
	parentSessionPath := rt.Session().SessionFile()
	if parentSessionPath == "" {
		t.Fatalf("expected non-empty session path after /newsessionparent")
	}

	if _, err := rt.Prompt("/armcancelswitch"); err != nil {
		t.Fatalf("Prompt /armcancelswitch (for switch): %v", err)
	}
	cancelledSwitch, err := rt.Prompt("/switchsession " + oldSessionPath)
	if err != nil {
		t.Fatalf("Prompt /switchsession (cancelled): %v", err)
	}
	if AssistantText(cancelledSwitch) != "switch-session-cancelled" {
		t.Fatalf("assistant text = %q, want switch-session-cancelled", AssistantText(cancelledSwitch))
	}
	if rt.Session().SessionFile() != parentSessionPath {
		t.Fatalf("expected cancelled /switchsession to keep session path %q, got %q", parentSessionPath, rt.Session().SessionFile())
	}

	switchAssistant, err := rt.Prompt("/switchsession " + oldSessionPath)
	if err != nil {
		t.Fatalf("Prompt /switchsession: %v", err)
	}
	if AssistantText(switchAssistant) != "switch-session-ok" {
		t.Fatalf("assistant text = %q, want switch-session-ok", AssistantText(switchAssistant))
	}
	if rt.Session().SessionFile() != oldSessionPath {
		t.Fatalf("expected switched session path %q, got %q", oldSessionPath, rt.Session().SessionFile())
	}
	infoAfterSwitch, err := rt.Prompt("/sessioninfo")
	if err != nil {
		t.Fatalf("Prompt /sessioninfo after /switchsession: %v", err)
	}
	_, afterSwitchPath, _ := parseSessionInfo(AssistantText(infoAfterSwitch))
	if afterSwitchPath != oldSessionPath {
		t.Fatalf("sessioninfo path after /switchsession = %q, want %q", afterSwitchPath, oldSessionPath)
	}

	if _, err := rt.Prompt("/armcancelfork"); err != nil {
		t.Fatalf("Prompt /armcancelfork: %v", err)
	}
	cancelledFork, err := rt.Prompt("/forkat " + seedUserEntryID)
	if err != nil {
		t.Fatalf("Prompt /forkat (cancelled): %v", err)
	}
	if AssistantText(cancelledFork) != "fork-cancelled" {
		t.Fatalf("assistant text = %q, want fork-cancelled", AssistantText(cancelledFork))
	}
	if rt.Session().SessionFile() != oldSessionPath {
		t.Fatalf("expected cancelled /forkat to keep session path %q, got %q", oldSessionPath, rt.Session().SessionFile())
	}

	forkAssistant, err := rt.Prompt("/forkat " + seedUserEntryID)
	if err != nil {
		t.Fatalf("Prompt /forkat: %v", err)
	}
	if AssistantText(forkAssistant) != "fork-ok" {
		t.Fatalf("assistant text = %q, want fork-ok", AssistantText(forkAssistant))
	}
	forkedSessionPath := rt.Session().SessionFile()
	if forkedSessionPath == oldSessionPath {
		t.Fatalf("expected /forkat to switch to new forked session, got unchanged %q", forkedSessionPath)
	}
	if got := strings.TrimSpace(rt.Session().Header().ParentSession); got != oldSessionPath {
		t.Fatalf("expected forked session parentSession %q, got %q", oldSessionPath, got)
	}
	navigateTargetID := ""
	for _, e := range rt.Session().Entries() {
		if e.Type != "message" || e.Message == nil || e.Message.Role != types.RoleUser {
			continue
		}
		if strings.Contains(AssistantText(*e.Message), "seed parity session") {
			navigateTargetID = e.ID
			break
		}
	}
	if navigateTargetID == "" {
		t.Fatal("failed to locate navigate target entry id in forked session")
	}

	if _, err := rt.Prompt("/armcanceltree"); err != nil {
		t.Fatalf("Prompt /armcanceltree: %v", err)
	}
	leafBeforeCancelledNavigate := rt.Session().LeafID()
	cancelledNavigate, err := rt.Prompt("/navigateopts " + navigateTargetID)
	if err != nil {
		t.Fatalf("Prompt /navigateopts (cancelled): %v", err)
	}
	if AssistantText(cancelledNavigate) != "navigate-opts-cancelled" {
		t.Fatalf("assistant text = %q, want navigate-opts-cancelled", AssistantText(cancelledNavigate))
	}
	if got := findLatestAssistantParent("navigate-opts-cancelled"); got != leafBeforeCancelledNavigate {
		t.Fatalf("expected cancelled /navigateopts output parent %q, got %q", leafBeforeCancelledNavigate, got)
	}

	navigateAssistant, err := rt.Prompt("/navigateopts " + navigateTargetID)
	if err != nil {
		t.Fatalf("Prompt /navigateopts: %v", err)
	}
	if AssistantText(navigateAssistant) != "navigate-opts-ok" {
		t.Fatalf("assistant text = %q, want navigate-opts-ok", AssistantText(navigateAssistant))
	}
	if got := findLatestAssistantParent("navigate-opts-ok"); got != navigateTargetID {
		t.Fatalf("expected /navigateopts output parent %q, got %q", navigateTargetID, got)
	}

	if _, err := rt.Prompt("/armtreesummary"); err != nil {
		t.Fatalf("Prompt /armtreesummary: %v", err)
	}
	summaryNavigateAssistant, err := rt.Prompt("/navigateopts " + leafBeforeCancelledNavigate)
	if err != nil {
		t.Fatalf("Prompt /navigateopts (summary): %v", err)
	}
	if AssistantText(summaryNavigateAssistant) != "navigate-opts-ok" {
		t.Fatalf("assistant text = %q, want navigate-opts-ok", AssistantText(summaryNavigateAssistant))
	}
	var sawInjectedBranchSummary bool
	var sawInjectedBranchSummaryDetails bool
	for _, e := range rt.Session().Entries() {
		if e.Type != "branch_summary" {
			continue
		}
		if strings.Contains(e.Summary, "parity-tree-summary") {
			sawInjectedBranchSummary = true
			if e.Details != nil && e.Details["source"] == "session_parity_extension" {
				sawInjectedBranchSummaryDetails = true
			}
			break
		}
	}
	if !sawInjectedBranchSummary {
		t.Fatalf("expected injected branch_summary entry from session_before_tree summary override")
	}
	if !sawInjectedBranchSummaryDetails {
		t.Fatalf("expected injected branch_summary details to be persisted")
	}

	dumpAssistant, err := rt.Prompt("/eventsdump")
	if err != nil {
		t.Fatalf("Prompt /eventsdump: %v", err)
	}
	var dump struct {
		BeforeSwitch []struct {
			Reason            string `json:"reason"`
			TargetSessionFile string `json:"targetSessionFile"`
		} `json:"beforeSwitch"`
		SwitchEvents []struct {
			Reason              string `json:"reason"`
			PreviousSessionFile string `json:"previousSessionFile"`
		} `json:"switchEvents"`
		BeforeFork []struct {
			EntryID string `json:"entryId"`
		} `json:"beforeFork"`
		ForkEvents []struct {
			PreviousSessionFile string `json:"previousSessionFile"`
		} `json:"forkEvents"`
		BeforeTree []struct {
			TargetID            string `json:"targetId"`
			OldLeafID           string `json:"oldLeafId"`
			UserWantsSummary    bool   `json:"userWantsSummary"`
			CustomInstructions  string `json:"customInstructions"`
			ReplaceInstructions bool   `json:"replaceInstructions"`
			Label               string `json:"label"`
		} `json:"beforeTree"`
		TreeEvents []struct {
			TargetID string `json:"targetId"`
		} `json:"treeEvents"`
	}
	if err := json.Unmarshal([]byte(AssistantText(dumpAssistant)), &dump); err != nil {
		t.Fatalf("decode /eventsdump JSON: %v", err)
	}

	var sawBeforeSwitchNew, sawBeforeSwitchResume bool
	for _, e := range dump.BeforeSwitch {
		if e.Reason == "new" {
			sawBeforeSwitchNew = true
		}
		if e.Reason == "resume" {
			sawBeforeSwitchResume = true
		}
	}
	if !sawBeforeSwitchNew {
		t.Fatalf("expected session_before_switch reason=new event")
	}
	if !sawBeforeSwitchResume {
		t.Fatalf("expected session_before_switch reason=resume event")
	}

	var sawSwitchNew, sawSwitchResume bool
	for _, e := range dump.SwitchEvents {
		if e.Reason == "new" {
			sawSwitchNew = true
		}
		if e.Reason == "resume" {
			sawSwitchResume = true
		}
	}
	if !sawSwitchNew {
		t.Fatalf("expected session_switch reason=new event")
	}
	if !sawSwitchResume {
		t.Fatalf("expected session_switch reason=resume event")
	}

	var sawBeforeFork, sawFork bool
	for _, e := range dump.BeforeFork {
		if e.EntryID == seedUserEntryID {
			sawBeforeFork = true
			break
		}
	}
	for _, e := range dump.ForkEvents {
		if strings.TrimSpace(e.PreviousSessionFile) != "" {
			sawFork = true
			break
		}
	}
	if !sawBeforeFork {
		t.Fatalf("expected session_before_fork for entry %q", seedUserEntryID)
	}
	if !sawFork {
		t.Fatalf("expected session_fork event with previousSessionFile")
	}

	var sawBeforeTreeOptions, sawTreeEvent bool
	for _, e := range dump.BeforeTree {
		if e.TargetID != navigateTargetID {
			continue
		}
		if e.UserWantsSummary &&
			e.CustomInstructions == "parity-tree-custom" &&
			e.ReplaceInstructions &&
			e.Label == "parity-tree-label" {
			sawBeforeTreeOptions = true
			break
		}
	}
	for _, e := range dump.TreeEvents {
		if e.TargetID == navigateTargetID {
			sawTreeEvent = true
			break
		}
	}
	if !sawBeforeTreeOptions {
		t.Fatalf("expected session_before_tree to include navigate options for target %q", navigateTargetID)
	}
	if !sawTreeEvent {
		t.Fatalf("expected session_tree event for target %q", navigateTargetID)
	}

	if got := atomic.LoadInt32(&providerCalls); got != 1 {
		t.Fatalf("expected exactly one provider call from seed prompt, got %d", got)
	}
}

func TestRuntimeExtensionEventBusOnAndEmit(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "event_bus_extension.mjs")

	serverCalls := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&serverCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"unexpected","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	emitAssistant, err := rt.Prompt("/emitbus hello")
	if err != nil {
		t.Fatalf("Prompt /emitbus: %v", err)
	}
	if AssistantText(emitAssistant) != "emitbus-ok" {
		t.Fatalf("assistant text = %q, want emitbus-ok", AssistantText(emitAssistant))
	}

	stateAssistant, err := rt.Prompt("/busstate")
	if err != nil {
		t.Fatalf("Prompt /busstate: %v", err)
	}
	if AssistantText(stateAssistant) != "busstate:hello" {
		t.Fatalf("assistant text = %q, want busstate:hello", AssistantText(stateAssistant))
	}
	if got := atomic.LoadInt32(&serverCalls); got != 0 {
		t.Fatalf("expected provider not to be called for event bus commands, got %d", got)
	}
}

func TestRuntimeExtensionExecAndMessageRendererShim(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "exec_api_extension.mjs")

	serverCalls := int32(0)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&serverCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"unexpected","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	assistant, err := rt.Prompt("/execdiag")
	if err != nil {
		t.Fatalf("Prompt /execdiag: %v", err)
	}
	var diag struct {
		Code      int  `json:"code"`
		Killed    bool `json:"killed"`
		HasStdout bool `json:"hasStdout"`
	}
	if err := json.Unmarshal([]byte(AssistantText(assistant)), &diag); err != nil {
		t.Fatalf("decode /execdiag JSON: %v", err)
	}
	if diag.Code != 0 {
		t.Fatalf("exec code = %d, want 0", diag.Code)
	}
	if diag.Killed {
		t.Fatalf("exec command unexpectedly reported killed=true")
	}
	if !diag.HasStdout {
		t.Fatalf("expected exec command to return non-empty stdout")
	}
	if got := atomic.LoadInt32(&serverCalls); got != 0 {
		t.Fatalf("expected provider not to be called for /execdiag, got %d", got)
	}
}

func TestRuntimeExtensionReadonlySessionManagerSurface(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "session_manager_extension.mjs")

	var providerCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"seed-ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	if _, err := rt.Prompt("seed readonly session manager"); err != nil {
		t.Fatalf("Prompt seed readonly session manager: %v", err)
	}

	diagAssistant, err := rt.Prompt("/smdiag")
	if err != nil {
		t.Fatalf("Prompt /smdiag: %v", err)
	}
	var diag struct {
		HasShape     bool   `json:"hasShape"`
		CWD          string `json:"cwd"`
		SessionDir   string `json:"sessionDir"`
		SessionID    string `json:"sessionId"`
		SessionFile  string `json:"sessionFile"`
		SessionName  string `json:"sessionName"`
		LeafID       string `json:"leafId"`
		LeafEntryID  string `json:"leafEntryId"`
		HeaderID     string `json:"headerId"`
		EntryByLeaf  string `json:"entryByLeafId"`
		EntriesCount int    `json:"entriesCount"`
		BranchCount  int    `json:"branchCount"`
		TreeRoots    int    `json:"treeRoots"`
	}
	if err := json.Unmarshal([]byte(AssistantText(diagAssistant)), &diag); err != nil {
		t.Fatalf("decode /smdiag JSON: %v", err)
	}
	if !diag.HasShape {
		t.Fatalf("expected readonly session manager shape to be available")
	}
	if diag.SessionID != rt.Session().SessionID() {
		t.Fatalf("sessionId = %q, want %q", diag.SessionID, rt.Session().SessionID())
	}
	if diag.SessionFile != rt.Session().SessionFile() {
		t.Fatalf("sessionFile = %q, want %q", diag.SessionFile, rt.Session().SessionFile())
	}
	if diag.SessionDir != rt.Session().SessionDir() {
		t.Fatalf("sessionDir = %q, want %q", diag.SessionDir, rt.Session().SessionDir())
	}
	if diag.HeaderID != rt.Session().SessionID() {
		t.Fatalf("headerId = %q, want %q", diag.HeaderID, rt.Session().SessionID())
	}
	if diag.LeafID == "" {
		t.Fatalf("expected non-empty leafId")
	}
	if diag.LeafEntryID != diag.LeafID {
		t.Fatalf("leafEntryId = %q, want leafId %q", diag.LeafEntryID, diag.LeafID)
	}
	if diag.EntryByLeaf != diag.LeafID {
		t.Fatalf("entryByLeafId = %q, want leafId %q", diag.EntryByLeaf, diag.LeafID)
	}
	if diag.EntriesCount == 0 {
		t.Fatalf("expected entriesCount > 0")
	}
	if diag.BranchCount == 0 {
		t.Fatalf("expected branchCount > 0")
	}
	if diag.TreeRoots == 0 {
		t.Fatalf("expected treeRoots > 0")
	}

	nameAssistant, err := rt.Prompt("/smname")
	if err != nil {
		t.Fatalf("Prompt /smname: %v", err)
	}
	if AssistantText(nameAssistant) != "smname-ok" {
		t.Fatalf("assistant text = %q, want smname-ok", AssistantText(nameAssistant))
	}
	diagAfterName, err := rt.Prompt("/smdiag")
	if err != nil {
		t.Fatalf("Prompt /smdiag after /smname: %v", err)
	}
	if err := json.Unmarshal([]byte(AssistantText(diagAfterName)), &diag); err != nil {
		t.Fatalf("decode /smdiag after /smname JSON: %v", err)
	}
	if diag.SessionName != "smdiag-session" {
		t.Fatalf("sessionName = %q, want smdiag-session", diag.SessionName)
	}

	labelAssistant, err := rt.Prompt("/smlabel")
	if err != nil {
		t.Fatalf("Prompt /smlabel: %v", err)
	}
	labelText := AssistantText(labelAssistant)
	if !strings.HasPrefix(labelText, "smlabel-ok:") {
		t.Fatalf("assistant text = %q, want prefix smlabel-ok:", labelText)
	}
	targetID := strings.TrimSpace(strings.TrimPrefix(labelText, "smlabel-ok:"))
	if targetID == "" {
		t.Fatalf("expected non-empty target id from /smlabel")
	}
	labelCheckAssistant, err := rt.Prompt("/smgetlabel " + targetID)
	if err != nil {
		t.Fatalf("Prompt /smgetlabel: %v", err)
	}
	if AssistantText(labelCheckAssistant) != "smdiag-label" {
		t.Fatalf("assistant text = %q, want smdiag-label", AssistantText(labelCheckAssistant))
	}

	if got := atomic.LoadInt32(&providerCalls); got != 1 {
		t.Fatalf("expected provider to be called once for seed prompt, got %d", got)
	}
}

func TestRuntimeExtensionContextParitySurface(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "context_parity_extension.mjs")

	var providerCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"seed-ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		Provider:                "openai",
		Model:                   "gpt-test",
		SystemPrompt:            "ctxdiag-system",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	if _, err := rt.Prompt("seed context parity"); err != nil {
		t.Fatalf("Prompt seed context parity: %v", err)
	}

	thinkingAssistant, err := rt.Prompt("/ctxsetthinking high")
	if err != nil {
		t.Fatalf("Prompt /ctxsetthinking: %v", err)
	}
	if AssistantText(thinkingAssistant) != "ctxsetthinking:high" {
		t.Fatalf("assistant text = %q, want ctxsetthinking:high", AssistantText(thinkingAssistant))
	}

	diagAssistant, err := rt.Prompt("/ctxdiag")
	if err != nil {
		t.Fatalf("Prompt /ctxdiag: %v", err)
	}
	var diag struct {
		HasContextShape          bool   `json:"hasContextShape"`
		HasModelRegistryShape    bool   `json:"hasModelRegistryShape"`
		ModelProvider            string `json:"modelProvider"`
		ModelID                  string `json:"modelId"`
		FoundCurrentModel        bool   `json:"foundCurrentModel"`
		AvailableCount           int    `json:"availableCount"`
		AvailableHasCurrent      bool   `json:"availableHasCurrent"`
		SystemPrompt             string `json:"systemPrompt"`
		IsIdle                   bool   `json:"isIdle"`
		HasPendingMessages       bool   `json:"hasPendingMessages"`
		ContextUsageVisible      bool   `json:"contextUsageVisible"`
		ThinkingLevel            string `json:"thinkingLevel"`
		APIKeyForModelPresent    bool   `json:"apiKeyForModelPresent"`
		APIKeyForProviderPresent bool   `json:"apiKeyForProviderPresent"`
		APIKeyByProviderPresent  bool   `json:"apiKeyByProviderPresent"`
		UsingOAuth               bool   `json:"usingOAuth"`
	}
	if err := json.Unmarshal([]byte(AssistantText(diagAssistant)), &diag); err != nil {
		t.Fatalf("decode /ctxdiag JSON: %v", err)
	}
	if !diag.HasContextShape {
		t.Fatalf("expected extension context shape to be available")
	}
	if !diag.HasModelRegistryShape {
		t.Fatalf("expected model registry shape to be available")
	}
	if diag.ModelProvider != rt.Model().Provider {
		t.Fatalf("modelProvider = %q, want %q", diag.ModelProvider, rt.Model().Provider)
	}
	if diag.ModelID != rt.Model().ID {
		t.Fatalf("modelId = %q, want %q", diag.ModelID, rt.Model().ID)
	}
	if !diag.FoundCurrentModel {
		t.Fatalf("expected modelRegistry.find() to resolve current model")
	}
	if diag.AvailableCount == 0 {
		t.Fatalf("expected available model count > 0")
	}
	if !diag.AvailableHasCurrent {
		t.Fatalf("expected available model set to include current model")
	}
	if diag.SystemPrompt != "ctxdiag-system" {
		t.Fatalf("systemPrompt = %q, want ctxdiag-system", diag.SystemPrompt)
	}
	if !diag.IsIdle {
		t.Fatalf("expected ctx.isIdle() == true in command context")
	}
	if diag.HasPendingMessages {
		t.Fatalf("expected ctx.hasPendingMessages() == false in command context")
	}
	if !diag.ContextUsageVisible {
		t.Fatalf("expected ctx.getContextUsage() to be visible")
	}
	if diag.ThinkingLevel != "high" {
		t.Fatalf("thinkingLevel = %q, want high", diag.ThinkingLevel)
	}
	if !diag.APIKeyForModelPresent {
		t.Fatalf("expected modelRegistry.getApiKey(ctx.model) to return configured key")
	}
	if !diag.APIKeyForProviderPresent {
		t.Fatalf("expected modelRegistry.getApiKeyForProvider() to return configured key")
	}
	if !diag.APIKeyByProviderPresent {
		t.Fatalf("expected modelRegistry.getApiKeyByProvider() to return configured key")
	}
	if diag.UsingOAuth {
		t.Fatalf("expected ctx.modelRegistry.isUsingOAuth(ctx.model) == false for api_key auth")
	}

	if got := atomic.LoadInt32(&providerCalls); got != 1 {
		t.Fatalf("expected provider to be called once for seed prompt, got %d", got)
	}
}

func TestRuntimeExtensionModelRegistryIsUsingOAuthParity(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "context_parity_extension.mjs")

	var providerCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"oauth-seed-ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeOAuthTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	if _, err := rt.Prompt("seed oauth context parity"); err != nil {
		t.Fatalf("Prompt seed oauth context parity: %v", err)
	}

	diagAssistant, err := rt.Prompt("/ctxdiag")
	if err != nil {
		t.Fatalf("Prompt /ctxdiag oauth: %v", err)
	}
	var diag struct {
		UsingOAuth bool `json:"usingOAuth"`
	}
	if err := json.Unmarshal([]byte(AssistantText(diagAssistant)), &diag); err != nil {
		t.Fatalf("decode /ctxdiag oauth JSON: %v", err)
	}
	if !diag.UsingOAuth {
		t.Fatalf("expected ctx.modelRegistry.isUsingOAuth(ctx.model) == true for oauth auth")
	}

	if got := atomic.LoadInt32(&providerCalls); got != 1 {
		t.Fatalf("expected provider to be called once for seed prompt, got %d", got)
	}
}

func TestRuntimeExtensionContextUsageAndCompactAction(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "context_parity_extension.mjs")

	var providerCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"summary-ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":30,"completion_tokens":10,"total_tokens":40}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		Provider:                "openai",
		Model:                   "gpt-test",
		SystemPrompt:            "ctxcompact-system",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	for i := 0; i < 5; i++ {
		if _, err := rt.Prompt(fmt.Sprintf("seed compaction parity %d", i)); err != nil {
			t.Fatalf("Prompt seed compaction parity %d: %v", i, err)
		}
	}

	usageBeforeAssistant, err := rt.Prompt("/ctxusage")
	if err != nil {
		t.Fatalf("Prompt /ctxusage before compact: %v", err)
	}
	var usageBefore struct {
		HasUsage      bool     `json:"hasUsage"`
		Tokens        *int64   `json:"tokens"`
		ContextWindow int64    `json:"contextWindow"`
		Percent       *float64 `json:"percent"`
	}
	if err := json.Unmarshal([]byte(AssistantText(usageBeforeAssistant)), &usageBefore); err != nil {
		t.Fatalf("decode /ctxusage before JSON: %v", err)
	}
	if !usageBefore.HasUsage {
		t.Fatalf("expected context usage snapshot before compact")
	}
	if usageBefore.ContextWindow <= 0 {
		t.Fatalf("expected contextWindow > 0, got %d", usageBefore.ContextWindow)
	}
	if usageBefore.Tokens == nil {
		t.Fatalf("expected tokens before compact to be known")
	}

	compactAssistant, err := rt.Prompt("/ctxcompact include key TODOs")
	if err != nil {
		t.Fatalf("Prompt /ctxcompact: %v", err)
	}
	if AssistantText(compactAssistant) != "ctxcompact-ok" {
		t.Fatalf("assistant text = %q, want ctxcompact-ok", AssistantText(compactAssistant))
	}

	entries := rt.Session().Entries()
	var foundCompaction bool
	for _, entry := range entries {
		if entry.Type != "compaction" {
			continue
		}
		foundCompaction = true
		if strings.TrimSpace(entry.Summary) == "" {
			t.Fatalf("expected non-empty compaction summary")
		}
		if strings.TrimSpace(entry.FirstKeptEntry) == "" {
			t.Fatalf("expected non-empty compaction firstKeptEntryId")
		}
		break
	}
	if !foundCompaction {
		t.Fatalf("expected compaction entry after /ctxcompact")
	}

	usageAfterAssistant, err := rt.Prompt("/ctxusage")
	if err != nil {
		t.Fatalf("Prompt /ctxusage after compact: %v", err)
	}
	var usageAfter struct {
		HasUsage      bool     `json:"hasUsage"`
		Tokens        *int64   `json:"tokens"`
		ContextWindow int64    `json:"contextWindow"`
		Percent       *float64 `json:"percent"`
	}
	if err := json.Unmarshal([]byte(AssistantText(usageAfterAssistant)), &usageAfter); err != nil {
		t.Fatalf("decode /ctxusage after JSON: %v", err)
	}
	if !usageAfter.HasUsage {
		t.Fatalf("expected context usage snapshot after compact")
	}
	if usageAfter.ContextWindow <= 0 {
		t.Fatalf("expected contextWindow > 0 after compact")
	}
	if usageAfter.Tokens != nil {
		t.Fatalf("expected tokens to be unknown immediately after compaction")
	}
	if usageAfter.Percent != nil {
		t.Fatalf("expected percent to be unknown immediately after compaction")
	}

	if got := atomic.LoadInt32(&providerCalls); got < 2 {
		t.Fatalf("expected provider to be called for seed prompt and compaction summary, got %d", got)
	}
}

func TestRuntimeExtensionCompactionHooksParity(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "compaction_extension.mjs")

	var providerCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"seed-ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		Provider:                "openai",
		Model:                   "gpt-test",
		SystemPrompt:            "compaction-hook-system",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	for i := 0; i < 5; i++ {
		if _, err := rt.Prompt(fmt.Sprintf("seed compaction hook parity %d", i)); err != nil {
			t.Fatalf("Prompt seed compaction hook parity %d: %v", i, err)
		}
	}

	armCustom, err := rt.Prompt("/armcustomcompact")
	if err != nil {
		t.Fatalf("Prompt /armcustomcompact: %v", err)
	}
	if AssistantText(armCustom) != "custom-compact-armed" {
		t.Fatalf("assistant text = %q, want custom-compact-armed", AssistantText(armCustom))
	}

	runCompact, err := rt.Prompt("/runcompact include unresolved TODOs")
	if err != nil {
		t.Fatalf("Prompt /runcompact custom: %v", err)
	}
	if AssistantText(runCompact) != "runcompact-ok" {
		t.Fatalf("assistant text = %q, want runcompact-ok", AssistantText(runCompact))
	}

	entries := rt.Session().Entries()
	compactionCount := 0
	foundCustomSummary := false
	foundCustomDetails := false
	for _, entry := range entries {
		if entry.Type != "compaction" {
			continue
		}
		compactionCount++
		if entry.Summary == "extension-compaction-summary" {
			foundCustomSummary = true
			if entry.Details != nil && entry.Details["source"] == "compaction_extension" {
				foundCustomDetails = true
			}
		}
	}
	if !foundCustomSummary {
		t.Fatalf("expected custom compaction summary from session_before_compact override")
	}
	if !foundCustomDetails {
		t.Fatalf("expected custom compaction details to be persisted")
	}
	if compactionCount == 0 {
		t.Fatalf("expected at least one compaction entry")
	}

	eventsAssistant, err := rt.Prompt("/compactevents")
	if err != nil {
		t.Fatalf("Prompt /compactevents: %v", err)
	}
	var events struct {
		BeforeCompact []struct {
			FirstKeptEntryID   string `json:"firstKeptEntryId"`
			TokensBefore       int    `json:"tokensBefore"`
			CustomInstructions string `json:"customInstructions"`
		} `json:"beforeCompact"`
		CompactEvents []struct {
			FromExtension bool   `json:"fromExtension"`
			Summary       string `json:"summary"`
		} `json:"compactEvents"`
	}
	if err := json.Unmarshal([]byte(AssistantText(eventsAssistant)), &events); err != nil {
		t.Fatalf("decode /compactevents JSON: %v", err)
	}
	if len(events.BeforeCompact) == 0 {
		t.Fatalf("expected session_before_compact hook to run")
	}
	if len(events.CompactEvents) == 0 {
		t.Fatalf("expected session_compact event to run")
	}
	lastCompact := events.CompactEvents[len(events.CompactEvents)-1]
	if !lastCompact.FromExtension {
		t.Fatalf("expected session_compact.fromExtension=true for custom override")
	}
	if lastCompact.Summary != "extension-compaction-summary" {
		t.Fatalf("session_compact summary = %q, want extension-compaction-summary", lastCompact.Summary)
	}

	mirrorAssistant, err := rt.Prompt("/compactmirror")
	if err != nil {
		t.Fatalf("Prompt /compactmirror: %v", err)
	}
	var mirror struct {
		Count         int    `json:"count"`
		LatestSummary string `json:"latestSummary"`
	}
	if err := json.Unmarshal([]byte(AssistantText(mirrorAssistant)), &mirror); err != nil {
		t.Fatalf("decode /compactmirror JSON: %v", err)
	}
	if mirror.Count != compactionCount {
		t.Fatalf("session mirror compaction count = %d, want %d", mirror.Count, compactionCount)
	}
	if mirror.LatestSummary != "extension-compaction-summary" {
		t.Fatalf("session mirror latestSummary = %q, want extension-compaction-summary", mirror.LatestSummary)
	}

	armCancel, err := rt.Prompt("/armcancelcompact")
	if err != nil {
		t.Fatalf("Prompt /armcancelcompact: %v", err)
	}
	if AssistantText(armCancel) != "cancel-compact-armed" {
		t.Fatalf("assistant text = %q, want cancel-compact-armed", AssistantText(armCancel))
	}

	runCancelled, err := rt.Prompt("/runcompact should-cancel")
	if err != nil {
		t.Fatalf("Prompt /runcompact cancel path: %v", err)
	}
	if AssistantText(runCancelled) != "runcompact-ok" {
		t.Fatalf("assistant text = %q, want runcompact-ok on cancel path", AssistantText(runCancelled))
	}

	newCompactionCount := 0
	for _, entry := range rt.Session().Entries() {
		if entry.Type == "compaction" {
			newCompactionCount++
		}
	}
	if newCompactionCount != compactionCount {
		t.Fatalf("expected cancelled compaction to keep count at %d, got %d", compactionCount, newCompactionCount)
	}

	mirrorAfterCancelAssistant, err := rt.Prompt("/compactmirror")
	if err != nil {
		t.Fatalf("Prompt /compactmirror after cancel: %v", err)
	}
	var mirrorAfterCancel struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal([]byte(AssistantText(mirrorAfterCancelAssistant)), &mirrorAfterCancel); err != nil {
		t.Fatalf("decode /compactmirror after cancel JSON: %v", err)
	}
	if mirrorAfterCancel.Count != compactionCount {
		t.Fatalf("expected mirrored compaction count to stay at %d after cancel, got %d", compactionCount, mirrorAfterCancel.Count)
	}

	if got := atomic.LoadInt32(&providerCalls); got != 5 {
		t.Fatalf("expected provider calls to stay at seed prompts only (5), got %d", got)
	}
}

func TestRuntimeExtensionNewSessionSetupCallbackParity(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "new_session_setup_extension.mjs")

	var providerCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"seed-ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":5,"total_tokens":10}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	if _, err := rt.Prompt("seed setup callback parity"); err != nil {
		t.Fatalf("Prompt seed setup callback parity: %v", err)
	}
	oldSessionFile := rt.Session().SessionFile()

	setupAssistant, err := rt.Prompt("/newwithsetup")
	if err != nil {
		t.Fatalf("Prompt /newwithsetup: %v", err)
	}
	if AssistantText(setupAssistant) != "newwithsetup-ok" {
		t.Fatalf("assistant text = %q, want newwithsetup-ok", AssistantText(setupAssistant))
	}
	if rt.Session().SessionFile() == oldSessionFile {
		t.Fatalf("expected new session file after /newwithsetup")
	}

	diagAssistant, err := rt.Prompt("/setupdiag")
	if err != nil {
		t.Fatalf("Prompt /setupdiag: %v", err)
	}
	var diag struct {
		SessionName      string `json:"sessionName"`
		SetupMessageSeen bool   `json:"setupMessageSeen"`
		SetupCustomSeen  bool   `json:"setupCustomSeen"`
		SetupLabel       string `json:"setupLabel"`
	}
	if err := json.Unmarshal([]byte(AssistantText(diagAssistant)), &diag); err != nil {
		t.Fatalf("decode /setupdiag JSON: %v", err)
	}
	if diag.SessionName != "setup-session-name" {
		t.Fatalf("sessionName = %q, want setup-session-name", diag.SessionName)
	}
	if !diag.SetupMessageSeen {
		t.Fatalf("expected setup message from setup callback")
	}
	if !diag.SetupCustomSeen {
		t.Fatalf("expected setup custom message from setup callback")
	}
	if diag.SetupLabel != "setup-label" {
		t.Fatalf("setupLabel = %q, want setup-label", diag.SetupLabel)
	}

	if got := atomic.LoadInt32(&providerCalls); got != 1 {
		t.Fatalf("expected provider calls to remain at seed prompt only (1), got %d", got)
	}
}

func TestRuntimeExtensionCompactCallbacksParity(t *testing.T) {
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("failed to resolve caller")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "compact_callbacks_extension.mjs")

	var providerCalls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&providerCalls, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"summary-ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":20,"completion_tokens":10,"total_tokens":30}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, server.URL)
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     t.TempDir(),
		SessionDir:              t.TempDir(),
		Provider:                "openai",
		Model:                   "gpt-test",
		SystemPrompt:            "compact-callback-system",
		ExtensionSidecarCommand: nodePath,
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with node sidecar: %v", err)
	}
	defer func() {
		_ = rt.Close()
	}()

	failArm, err := rt.Prompt("/compactcbfail")
	if err != nil {
		t.Fatalf("Prompt /compactcbfail: %v", err)
	}
	if AssistantText(failArm) != "compactcbfail-armed" {
		t.Fatalf("assistant text = %q, want compactcbfail-armed", AssistantText(failArm))
	}

	failStateAssistant, err := rt.Prompt("/compactcbstate")
	if err != nil {
		t.Fatalf("Prompt /compactcbstate after fail: %v", err)
	}
	var failState struct {
		Completed bool   `json:"completed"`
		Summary   string `json:"summary"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(AssistantText(failStateAssistant)), &failState); err != nil {
		t.Fatalf("decode fail compactcbstate JSON: %v", err)
	}
	if failState.Completed {
		t.Fatalf("expected completed=false for fail path")
	}
	if strings.TrimSpace(failState.Error) == "" {
		t.Fatalf("expected non-empty error message for fail path")
	}
	if got := atomic.LoadInt32(&providerCalls); got != 0 {
		t.Fatalf("expected no provider calls for fail path, got %d", got)
	}

	for i := 0; i < 5; i++ {
		if _, err := rt.Prompt(fmt.Sprintf("seed compact callbacks %d", i)); err != nil {
			t.Fatalf("Prompt seed compact callbacks %d: %v", i, err)
		}
	}

	okArm, err := rt.Prompt("/compactcbok keep critical TODOs")
	if err != nil {
		t.Fatalf("Prompt /compactcbok: %v", err)
	}
	if AssistantText(okArm) != "compactcbok-armed" {
		t.Fatalf("assistant text = %q, want compactcbok-armed", AssistantText(okArm))
	}

	okStateAssistant, err := rt.Prompt("/compactcbstate")
	if err != nil {
		t.Fatalf("Prompt /compactcbstate after success: %v", err)
	}
	var okState struct {
		Completed bool   `json:"completed"`
		Summary   string `json:"summary"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal([]byte(AssistantText(okStateAssistant)), &okState); err != nil {
		t.Fatalf("decode success compactcbstate JSON: %v", err)
	}
	if !okState.Completed {
		t.Fatalf("expected completed=true for success path")
	}
	if strings.TrimSpace(okState.Summary) == "" {
		t.Fatalf("expected non-empty summary for success path")
	}
	if okState.Error != "" {
		t.Fatalf("expected empty error for success path, got %q", okState.Error)
	}
	if got := atomic.LoadInt32(&providerCalls); got < 6 {
		t.Fatalf("expected provider calls for seed prompts + compaction summary, got %d", got)
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

func writeOAuthTestConfig(t *testing.T, agentDir, baseURL string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(agentDir, "auth.json"), []byte(`{
		"openai":{"type":"oauth","access":"oauth-access-token"}
	}`), 0o644); err != nil {
		t.Fatalf("write auth.json (oauth): %v", err)
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
		t.Fatalf("write models.json (oauth): %v", err)
	}
}

func TestRuntimeSidecarHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIME_SIDECAR_HELPER") != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	enc := json.NewEncoder(os.Stdout)

	for scanner.Scan() {
		var req struct {
			ID     string          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}
		switch req.Method {
		case "initialize":
			var initReq extensionsidecar.InitializeRequest
			if err := json.Unmarshal(req.Params, &initReq); err != nil {
				writeSidecarHelperError(t, enc, req.ID, "invalid_request", err.Error())
				continue
			}
			helperBaseURL, _ := initReq.FlagValues["helper_base_url"].(string)
			writeSidecarHelperResult(t, enc, req.ID, extensionsidecar.InitializeResponse{
				ProtocolVersion: extensionsidecar.ProtocolVersion,
				Tools: []types.Tool{
					{
						Name:        "helper_echo",
						Description: "Runtime test sidecar echo tool",
						Parameters: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"text": map[string]any{"type": "string"},
							},
						},
					},
				},
				Flags: []extensionsidecar.ExtensionFlagDefinition{
					{
						Name:    "helper_base_url",
						Type:    "string",
						Default: "",
					},
				},
				Commands: []extensionsidecar.ExtensionCommandDefinition{
					{
						Name: "ping",
					},
				},
				Providers: []extensionsidecar.ProviderRegistration{
					{
						Name: "runtime-helper-provider",
						Config: map[string]any{
							"api":     "openai-completions",
							"baseUrl": helperBaseURL,
							"apiKey":  "test-key",
							"models": []map[string]any{
								{
									"id":            "runtime-helper-model",
									"name":          "runtime-helper-model",
									"provider":      "runtime-helper-provider",
									"api":           "openai-completions",
									"baseUrl":       helperBaseURL,
									"reasoning":     false,
									"input":         []string{"text"},
									"contextWindow": 8192,
									"maxTokens":     512,
								},
							},
						},
					},
				},
			})
		case "emit":
			var emitReq extensionsidecar.EmitRequest
			if err := json.Unmarshal(req.Params, &emitReq); err != nil {
				writeSidecarHelperError(t, enc, req.ID, "invalid_request", err.Error())
				continue
			}
			switch emitReq.Event.Type {
			case "input":
				text, _ := emitReq.Event.Payload["text"].(string)
				writeSidecarHelperResult(t, enc, req.ID, extensionsidecar.EmitResponse{
					Input: &extensionsidecar.InputEventResult{
						Action: "transform",
						Text:   text + " [runtime-hook]",
					},
				})
			case "before_agent_start":
				writeSidecarHelperResult(t, enc, req.ID, extensionsidecar.EmitResponse{
					BeforeAgentStart: &extensionsidecar.BeforeAgentStartEventResult{
						SystemPrompt: "runtime-sidecar-prompt",
					},
				})
			case "context":
				writeSidecarHelperResult(t, enc, req.ID, extensionsidecar.EmitResponse{
					Context: &extensionsidecar.ContextEventResult{
						SystemPrompt: "runtime-context-prompt",
					},
				})
			case "session_shutdown":
				markerPath := strings.TrimSpace(os.Getenv("GO_RUNTIME_SIDECAR_SHUTDOWN_MARKER"))
				if markerPath != "" {
					_ = os.WriteFile(markerPath, []byte("session_shutdown"), 0o644)
				}
				writeSidecarHelperResult(t, enc, req.ID, extensionsidecar.EmitResponse{})
			default:
				writeSidecarHelperResult(t, enc, req.ID, extensionsidecar.EmitResponse{})
			}
		case "tool.execute":
			var toolReq extensionsidecar.ExecuteToolRequest
			if err := json.Unmarshal(req.Params, &toolReq); err != nil {
				writeSidecarHelperError(t, enc, req.ID, "invalid_request", err.Error())
				continue
			}
			if toolReq.Name != "helper_echo" {
				writeSidecarHelperError(t, enc, req.ID, "tool_not_found", "tool not found")
				continue
			}
			text, _ := toolReq.Arguments["text"].(string)
			writeSidecarHelperResult(t, enc, req.ID, types.ToolResult{
				Content: []types.ContentBlock{{Type: "text", Text: "runtime-sidecar: " + text}},
				IsError: false,
			})
		case "command.execute":
			var cmdReq extensionsidecar.ExecuteCommandRequest
			if err := json.Unmarshal(req.Params, &cmdReq); err != nil {
				writeSidecarHelperError(t, enc, req.ID, "invalid_request", err.Error())
				continue
			}
			if cmdReq.Name != "ping" {
				writeSidecarHelperResult(t, enc, req.ID, extensionsidecar.ExecuteCommandResponse{Handled: false})
				continue
			}
			writeSidecarHelperResult(t, enc, req.ID, extensionsidecar.ExecuteCommandResponse{
				Handled: true,
				Output:  "pong:" + cmdReq.Args,
			})
		case "shutdown":
			writeSidecarHelperResult(t, enc, req.ID, map[string]any{"ok": true})
			return
		default:
			writeSidecarHelperError(t, enc, req.ID, "method_not_found", "unknown method")
		}
	}
	os.Exit(0)
}

func writeSidecarHelperResult(t *testing.T, enc *json.Encoder, id string, result any) {
	t.Helper()
	resp := map[string]any{
		"id":     id,
		"result": result,
	}
	if err := enc.Encode(resp); err != nil {
		t.Fatalf("encode helper response: %v", err)
	}
}

func writeSidecarHelperError(t *testing.T, enc *json.Encoder, id, code, message string) {
	t.Helper()
	resp := map[string]any{
		"id": id,
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}
	if err := enc.Encode(resp); err != nil {
		t.Fatalf("encode helper error response: %v", err)
	}
}
