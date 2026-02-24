package session

import (
	"path/filepath"
	"testing"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

func TestManagerCreateAppendOpen(t *testing.T) {
	tmp := t.TempDir()
	sm := NewManager(tmp)
	if err := sm.CreateNew(tmp, ""); err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}

	u := types.TextMessage(types.RoleUser, "hello")
	if _, err := sm.AppendMessage(u); err != nil {
		t.Fatalf("append user failed: %v", err)
	}
	a := types.TextMessage(types.RoleAssistant, "hi")
	if _, err := sm.AppendMessage(a); err != nil {
		t.Fatalf("append assistant failed: %v", err)
	}
	if _, err := sm.AppendModelChange("openai", "gpt-5.1-codex"); err != nil {
		t.Fatalf("append model failed: %v", err)
	}

	path := sm.SessionFile()
	if path == "" {
		t.Fatal("session file should be set")
	}

	sm2 := NewManager(tmp)
	if err := sm2.Open(path); err != nil {
		t.Fatalf("open failed: %v", err)
	}
	if sm2.SessionID() == "" {
		t.Fatal("session id should be present")
	}
	if len(sm2.Entries()) != 3 {
		t.Fatalf("expected 3 entries, got %d", len(sm2.Entries()))
	}
	branch := sm2.Branch("")
	if len(branch) != 3 {
		t.Fatalf("expected 3 branch entries, got %d", len(branch))
	}

	ctx := sm2.BuildContext("", "", nil)
	if len(ctx.Messages) != 2 {
		t.Fatalf("expected 2 context messages, got %d", len(ctx.Messages))
	}
	if ctx.ModelProvider != "openai" || ctx.ModelID != "gpt-5.1-codex" {
		t.Fatalf("unexpected model context: %s/%s", ctx.ModelProvider, ctx.ModelID)
	}

	infos, err := ListSessions(tmp)
	if err != nil {
		t.Fatalf("list sessions failed: %v", err)
	}
	if len(infos) != 1 {
		t.Fatalf("expected 1 session info, got %d", len(infos))
	}
	if infos[0].Path != filepath.Clean(path) {
		t.Fatalf("session path mismatch: %s != %s", infos[0].Path, path)
	}
}

func TestAppendCompactionAndBranchSummary(t *testing.T) {
	tmp := t.TempDir()
	sm := NewManager(tmp)
	if err := sm.CreateNew(tmp, ""); err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}

	userEntry, err := sm.AppendMessage(types.TextMessage(types.RoleUser, "initial request"))
	if err != nil {
		t.Fatalf("append user failed: %v", err)
	}
	if _, err := sm.AppendCompaction("compact summary", userEntry.ID, 1234); err != nil {
		t.Fatalf("append compaction failed: %v", err)
	}
	if _, err := sm.AppendBranchSummary(userEntry.ID, "branch summary"); err != nil {
		t.Fatalf("append branch summary failed: %v", err)
	}

	ctx := sm.BuildContext("", sm.LeafID(), nil)
	if len(ctx.Messages) < 3 {
		t.Fatalf("expected at least 3 context messages, got %d", len(ctx.Messages))
	}

	foundCompaction := false
	foundBranchSummary := false
	for _, msg := range ctx.Messages {
		for _, c := range msg.Content {
			if c.Type != "text" {
				continue
			}
			if c.Text == "<summary>\ncompact summary\n</summary>" {
				foundCompaction = true
			}
			if c.Text == "<branch_summary>\nbranch summary\n</branch_summary>" {
				foundBranchSummary = true
			}
		}
	}
	if !foundCompaction {
		t.Fatal("expected compaction summary in context messages")
	}
	if !foundBranchSummary {
		t.Fatal("expected branch summary in context messages")
	}
}

func TestCustomEntriesLabelsAndSessionName(t *testing.T) {
	tmp := t.TempDir()
	sm := NewManager(tmp)
	if err := sm.CreateNew(tmp, ""); err != nil {
		t.Fatalf("CreateNew failed: %v", err)
	}

	userEntry, err := sm.AppendMessage(types.TextMessage(types.RoleUser, "hello"))
	if err != nil {
		t.Fatalf("append user failed: %v", err)
	}
	if _, err := sm.AppendLabel(userEntry.ID, "bookmark"); err != nil {
		t.Fatalf("append label failed: %v", err)
	}
	if _, err := sm.AppendSessionName("alpha-session"); err != nil {
		t.Fatalf("append session name failed: %v", err)
	}
	if got := sm.SessionName(); got != "alpha-session" {
		t.Fatalf("session name = %q, want alpha-session", got)
	}

	if _, err := sm.AppendCustomEntry("ext.state", map[string]any{"k": "v"}); err != nil {
		t.Fatalf("append custom entry failed: %v", err)
	}
	if _, err := sm.AppendCustomMessage(
		"ext.msg",
		[]types.ContentBlock{{Type: "text", Text: "custom in context"}},
		true,
		map[string]any{"source": "test"},
	); err != nil {
		t.Fatalf("append custom message failed: %v", err)
	}

	foundCustomMessage := false
	ctx := sm.BuildContext("", sm.LeafID(), nil)
	for _, msg := range ctx.Messages {
		for _, c := range msg.Content {
			if c.Type == "text" && c.Text == "custom in context" {
				foundCustomMessage = true
			}
		}
	}
	if !foundCustomMessage {
		t.Fatal("expected custom message content in context")
	}

	var foundLabel, foundCustom bool
	for _, e := range sm.Entries() {
		if e.Type == "label" && e.TargetID == userEntry.ID && e.Label == "bookmark" {
			foundLabel = true
		}
		if e.Type == "custom" && e.CustomType == "ext.state" && e.CustomData["k"] == "v" {
			foundCustom = true
		}
	}
	if !foundLabel {
		t.Fatal("expected label entry to be persisted")
	}
	if !foundCustom {
		t.Fatal("expected custom entry to be persisted")
	}
}
