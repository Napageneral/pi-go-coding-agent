package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/extensionsidecar"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

func TestRuntimeQueueModesAllDequeuesAllMessages(t *testing.T) {
	rt := newRPCTestRuntime(t)
	defer func() { _ = rt.Close() }()

	if err := rt.SetSteeringMode("all"); err != nil {
		t.Fatalf("SetSteeringMode: %v", err)
	}
	if err := rt.SetFollowUpMode("all"); err != nil {
		t.Fatalf("SetFollowUpMode: %v", err)
	}
	if err := rt.Steer("one"); err != nil {
		t.Fatalf("Steer one: %v", err)
	}
	if err := rt.Steer("two"); err != nil {
		t.Fatalf("Steer two: %v", err)
	}
	if err := rt.FollowUp("three"); err != nil {
		t.Fatalf("FollowUp three: %v", err)
	}
	if err := rt.FollowUp("four"); err != nil {
		t.Fatalf("FollowUp four: %v", err)
	}

	steering := rt.dequeueSteeringMessages()
	if len(steering) != 2 {
		t.Fatalf("steering message count = %d, want 2", len(steering))
	}
	followUps := rt.dequeueFollowUpMessages()
	if len(followUps) != 2 {
		t.Fatalf("follow-up message count = %d, want 2", len(followUps))
	}
}

func TestRuntimeSubscribeEventsReceivesEventsWithoutSidecar(t *testing.T) {
	rt := newRPCTestRuntime(t)
	defer func() { _ = rt.Close() }()

	received := make(chan RuntimeEvent, 1)
	unsubscribe := rt.SubscribeEvents(func(event RuntimeEvent) {
		if event.Type != "rpc_test_event" {
			return
		}
		select {
		case received <- event:
		default:
		}
	})
	defer unsubscribe()

	_, _ = rt.emitEventBestEffort(context.Background(), extensionsidecar.Event{
		Type: "rpc_test_event",
		Payload: map[string]any{
			"value": "ok",
		},
	})

	select {
	case event := <-received:
		if event.Payload["value"] != "ok" {
			t.Fatalf("payload value = %#v, want ok", event.Payload["value"])
		}
		if event.Payload["ctxModel"] == nil {
			t.Fatalf("expected ctxModel in event payload")
		}
	default:
		t.Fatal("expected subscribed runtime event")
	}
}

func TestRuntimeExecuteBashIncludesExitCodeDetails(t *testing.T) {
	rt := newRPCTestRuntime(t)
	defer func() { _ = rt.Close() }()

	result, err := rt.ExecuteBash("exit 7")
	if err == nil {
		t.Fatal("expected bash command error")
	}
	if got := result.Details["exitCode"]; got != 7 {
		t.Fatalf("exitCode = %#v, want 7", got)
	}
	if cancelled, _ := result.Details["cancelled"].(bool); cancelled {
		t.Fatalf("cancelled = true, want false")
	}
}

func TestRuntimeCommandsIncludesExtensionPromptAndSkillCommands(t *testing.T) {
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
	if err := os.MkdirAll(filepath.Join(agentDir, "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir user prompts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "prompts", "user-cmd.md"), []byte("User prompt command"), 0o644); err != nil {
		t.Fatalf("write user prompt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(agentDir, "skills", "user-skill"), 0o755); err != nil {
		t.Fatalf("mkdir user skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "skills", "user-skill", "SKILL.md"), []byte("---\nname: user-skill\ndescription: User skill\n---\n"), 0o644); err != nil {
		t.Fatalf("write user skill: %v", err)
	}

	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, ".pi", "prompts"), 0o755); err != nil {
		t.Fatalf("mkdir project prompts: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".pi", "prompts", "project-cmd.md"), []byte("Project prompt command"), 0o644); err != nil {
		t.Fatalf("write project prompt: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cwd, ".pi", "skills", "project-skill"), 0o755); err != nil {
		t.Fatalf("mkdir project skill: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cwd, ".pi", "skills", "project-skill", "SKILL.md"), []byte("---\nname: project-skill\ndescription: Project skill\n---\n"), 0o644); err != nil {
		t.Fatalf("write project skill: %v", err)
	}

	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	sidecarPath := filepath.Join(repoRoot, "sidecar", "node-extension-runtime", "main.mjs")
	extensionPath := filepath.Join(filepath.Dir(thisFile), "testdata", "upstream_style_extension.mjs")

	rt, err := NewRuntime(NewRuntimeOptions{
		CWD:                     cwd,
		SessionDir:              t.TempDir(),
		NoSession:               true,
		Provider:                "openai",
		Model:                   "gpt-test",
		ExtensionSidecarCommand: "node",
		ExtensionSidecarArgs:    []string{sidecarPath},
		ExtensionPaths:          []string{extensionPath},
	})
	if err != nil {
		t.Fatalf("NewRuntime with sidecar: %v", err)
	}
	defer func() { _ = rt.Close() }()

	commands := rt.Commands()
	hasCommand := func(name, source, location string) bool {
		for _, command := range commands {
			if command.Name != name || command.Source != source {
				continue
			}
			if location != "" && command.Location != location {
				continue
			}
			return true
		}
		return false
	}
	if !hasCommand("ping", "extension", "") {
		t.Fatalf("expected extension command ping in %#v", commands)
	}
	if !hasCommand("user-cmd", "prompt", "user") {
		t.Fatalf("expected user prompt command in %#v", commands)
	}
	if !hasCommand("project-cmd", "prompt", "project") {
		t.Fatalf("expected project prompt command in %#v", commands)
	}
	if !hasCommand("skill:user-skill", "skill", "user") {
		t.Fatalf("expected user skill command in %#v", commands)
	}
	if !hasCommand("skill:project-skill", "skill", "project") {
		t.Fatalf("expected project skill command in %#v", commands)
	}
}

func TestRuntimeAutoSettingsMethods(t *testing.T) {
	rt := newRPCTestRuntime(t)
	defer func() { _ = rt.Close() }()

	if !rt.AutoCompactionEnabled() {
		t.Fatalf("expected auto compaction default enabled")
	}
	if !rt.AutoRetryEnabled() {
		t.Fatalf("expected auto retry default enabled")
	}
	rt.SetAutoCompactionEnabled(false)
	rt.SetAutoRetryEnabled(false)
	if rt.AutoCompactionEnabled() {
		t.Fatalf("expected auto compaction disabled after setter")
	}
	if rt.AutoRetryEnabled() {
		t.Fatalf("expected auto retry disabled after setter")
	}
}

func TestRuntimeExportHTMLWritesFile(t *testing.T) {
	rt := newRPCTestRuntime(t)
	defer func() { _ = rt.Close() }()

	if _, err := rt.Prompt("export this session"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	outPath := filepath.Join(t.TempDir(), "session-export.html")
	writtenPath, err := rt.ExportHTML(outPath)
	if err != nil {
		t.Fatalf("ExportHTML: %v", err)
	}
	if writtenPath != outPath {
		t.Fatalf("writtenPath = %q, want %q", writtenPath, outPath)
	}
	data, err := os.ReadFile(writtenPath)
	if err != nil {
		t.Fatalf("read export file: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "Session Export") {
		t.Fatalf("expected export HTML title in output")
	}
	if !strings.Contains(body, "export this session") {
		t.Fatalf("expected exported user prompt text in output")
	}
}

func TestRuntimeAutoRetryRetriesTransientProviderError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if call == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"service unavailable"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"ok-after-retry","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	rt := newRPCTestRuntimeWithServerURL(t, server.URL)
	defer func() { _ = rt.Close() }()

	sawRetryStart := false
	sawRetryEndSuccess := false
	unsubscribe := rt.SubscribeEvents(func(event RuntimeEvent) {
		switch event.Type {
		case "auto_retry_start":
			sawRetryStart = true
		case "auto_retry_end":
			success, _ := event.Payload["success"].(bool)
			if success {
				sawRetryEndSuccess = true
			}
		}
	})
	defer unsubscribe()

	assistant, err := rt.Prompt("retry me")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if AssistantText(assistant) != "ok-after-retry" {
		t.Fatalf("assistant text = %q, want ok-after-retry", AssistantText(assistant))
	}
	if got := calls.Load(); got < 2 {
		t.Fatalf("provider call count = %d, want >= 2", got)
	}
	if !sawRetryStart {
		t.Fatalf("expected auto_retry_start event")
	}
	if !sawRetryEndSuccess {
		t.Fatalf("expected successful auto_retry_end event")
	}
}

func TestRuntimeAbortRetryCancelsRetryDelay(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"service unavailable"}}`))
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	rt := newRPCTestRuntimeWithServerURL(t, server.URL)
	defer func() { _ = rt.Close() }()

	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	unsubscribe := rt.SubscribeEvents(func(event RuntimeEvent) {
		switch event.Type {
		case "auto_retry_start":
			select {
			case started <- struct{}{}:
			default:
			}
		case "auto_retry_end":
			if finalError, _ := event.Payload["finalError"].(string); strings.Contains(strings.ToLower(finalError), "cancel") {
				select {
				case cancelled <- struct{}{}:
				default:
				}
			}
		}
	})
	defer unsubscribe()

	done := make(chan error, 1)
	go func() {
		_, err := rt.Prompt("force retry then cancel")
		done <- err
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("expected auto_retry_start event")
	}

	time.Sleep(50 * time.Millisecond)
	rt.AbortRetry()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected prompt to fail after retry cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("prompt did not finish after aborting retry")
	}

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected auto_retry_end cancellation event")
	}
}

func TestRuntimeAutoCompactionRecoversFromContextOverflow(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		switch call {
		case 1:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"context length exceeded"}}`))
		case 2:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"compaction summary","tool_calls":[]},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"choices":[{"message":{"role":"assistant","content":"recovered","tool_calls":[]},"finish_reason":"stop"}],
				"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
			}`))
		}
	}))
	defer server.CloseClientConnections()
	defer server.Close()

	rt := newRPCTestRuntimeWithServerURL(t, server.URL)
	defer func() { _ = rt.Close() }()

	for i := 0; i < 10; i++ {
		if _, err := rt.Session().AppendMessage(types.TextMessage(types.RoleUser, fmt.Sprintf("seed user %d", i))); err != nil {
			t.Fatalf("seed user message %d: %v", i, err)
		}
		if _, err := rt.Session().AppendMessage(types.TextMessage(types.RoleAssistant, fmt.Sprintf("seed assistant %d", i))); err != nil {
			t.Fatalf("seed assistant message %d: %v", i, err)
		}
	}

	assistant, err := rt.Prompt("trigger overflow recovery")
	if err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	if AssistantText(assistant) != "recovered" {
		t.Fatalf("assistant text = %q, want recovered", AssistantText(assistant))
	}
	if got := calls.Load(); got < 3 {
		t.Fatalf("provider call count = %d, want >= 3", got)
	}

	foundCompaction := false
	for _, entry := range rt.Session().Entries() {
		if entry.Type == "compaction" {
			foundCompaction = true
			break
		}
	}
	if !foundCompaction {
		t.Fatalf("expected compaction entry after overflow recovery")
	}
}

func newRPCTestRuntime(t *testing.T) *Runtime {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices":[{"message":{"role":"assistant","content":"ok","tool_calls":[]},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	t.Cleanup(server.CloseClientConnections)
	t.Cleanup(server.Close)

	return newRPCTestRuntimeWithServerURL(t, server.URL)
}

func newRPCTestRuntimeWithServerURL(t *testing.T, baseURL string) *Runtime {
	t.Helper()

	agentDir := t.TempDir()
	writeTestConfig(t, agentDir, baseURL)
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
	return rt
}
