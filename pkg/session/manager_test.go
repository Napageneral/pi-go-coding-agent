package session

import (
	"path/filepath"
	"testing"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
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
