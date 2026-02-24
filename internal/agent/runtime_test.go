package agent

import (
	"bufio"
	"encoding/json"
	"errors"
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
	if _, err := rt.Prompt("/newsession"); err != nil {
		t.Fatalf("Prompt /newsession (cancelled): %v", err)
	}
	if rt.Session().SessionFile() != oldSessionPath {
		t.Fatalf("expected cancelled /newsession to keep session path %q, got %q", oldSessionPath, rt.Session().SessionFile())
	}

	if _, err := rt.Prompt("/newsession"); err != nil {
		t.Fatalf("Prompt /newsession: %v", err)
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

	if _, err := rt.Prompt("/switchsession " + oldSessionPath); err != nil {
		t.Fatalf("Prompt /switchsession: %v", err)
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
	if _, err := rt.Prompt("/forkat " + seedUserEntryID); err != nil {
		t.Fatalf("Prompt /forkat (cancelled): %v", err)
	}
	if rt.Session().SessionFile() != oldSessionPath {
		t.Fatalf("expected cancelled /forkat to keep session path %q, got %q", oldSessionPath, rt.Session().SessionFile())
	}

	if _, err := rt.Prompt("/forkat " + seedUserEntryID); err != nil {
		t.Fatalf("Prompt /forkat: %v", err)
	}
	forkedSessionPath := rt.Session().SessionFile()
	if forkedSessionPath == oldSessionPath {
		t.Fatalf("expected /forkat to switch to new forked session, got unchanged %q", forkedSessionPath)
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
	if _, err := rt.Prompt("/navigateopts " + navigateTargetID); err != nil {
		t.Fatalf("Prompt /navigateopts (cancelled): %v", err)
	}
	if got := findLatestAssistantParent("navigate-opts-ok"); got != leafBeforeCancelledNavigate {
		t.Fatalf("expected cancelled /navigateopts output parent %q, got %q", leafBeforeCancelledNavigate, got)
	}

	if _, err := rt.Prompt("/navigateopts " + navigateTargetID); err != nil {
		t.Fatalf("Prompt /navigateopts: %v", err)
	}
	if got := findLatestAssistantParent("navigate-opts-ok"); got != navigateTargetID {
		t.Fatalf("expected /navigateopts output parent %q, got %q", navigateTargetID, got)
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
