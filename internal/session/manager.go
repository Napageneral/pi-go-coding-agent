package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

const CurrentSessionVersion = 3

type Header struct {
	Type          string `json:"type"`
	Version       int    `json:"version,omitempty"`
	ID            string `json:"id"`
	Timestamp     string `json:"timestamp"`
	CWD           string `json:"cwd"`
	ParentSession string `json:"parentSession,omitempty"`
}

type Entry struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ParentID  string `json:"parentId,omitempty"`
	Timestamp string `json:"timestamp"`

	Message *types.Message `json:"message,omitempty"`

	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	Provider      string `json:"provider,omitempty"`
	ModelID       string `json:"modelId,omitempty"`

	Summary        string               `json:"summary,omitempty"`
	FirstKeptEntry string               `json:"firstKeptEntryId,omitempty"`
	TokensBefore   int                  `json:"tokensBefore,omitempty"`
	FromID         string               `json:"fromId,omitempty"`
	FromHook       bool                 `json:"fromHook,omitempty"`
	CustomType     string               `json:"customType,omitempty"`
	CustomData     map[string]any       `json:"data,omitempty"`
	Display        bool                 `json:"display,omitempty"`
	Content        []types.ContentBlock `json:"content,omitempty"`
	TargetID       string               `json:"targetId,omitempty"`
	Label          string               `json:"label,omitempty"`
	Name           string               `json:"name,omitempty"`
}

type fileLine struct {
	Type string `json:"type"`
}

type Context struct {
	Messages      []types.Message
	ThinkingLevel string
	ModelProvider string
	ModelID       string
}

type Info struct {
	Path         string
	ID           string
	CWD          string
	Name         string
	Created      time.Time
	Modified     time.Time
	MessageCount int
	FirstMessage string
}

type Manager struct {
	sessionDir  string
	sessionFile string
	header      Header
	entries     []Entry
	byID        map[string]int
	leafID      string
}

func EnsureSessionDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

func NewManager(sessionDir string) *Manager {
	return &Manager{sessionDir: sessionDir, byID: map[string]int{}}
}

func (m *Manager) SessionDir() string  { return m.sessionDir }
func (m *Manager) SessionFile() string { return m.sessionFile }
func (m *Manager) SessionID() string   { return m.header.ID }
func (m *Manager) CWD() string         { return m.header.CWD }
func (m *Manager) LeafID() string      { return m.leafID }

func (m *Manager) Header() Header { return m.header }

func (m *Manager) Entries() []Entry {
	out := make([]Entry, len(m.entries))
	copy(out, m.entries)
	return out
}

func (m *Manager) CreateNew(cwd string, parentSession string) error {
	if err := EnsureSessionDir(m.sessionDir); err != nil {
		return err
	}
	sid := shortID()
	m.header = Header{
		Type:          "session",
		Version:       CurrentSessionVersion,
		ID:            sid,
		Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
		CWD:           cwd,
		ParentSession: parentSession,
	}
	m.entries = nil
	m.byID = map[string]int{}
	m.leafID = ""
	m.sessionFile = filepath.Join(m.sessionDir, sid+".jsonl")
	return m.flushAll()
}

func (m *Manager) Open(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	var lines []json.RawMessage
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		lines = append(lines, json.RawMessage(append([]byte(nil), []byte(line)...)))
	}
	if err := s.Err(); err != nil {
		return err
	}
	if len(lines) == 0 {
		return errors.New("session file is empty")
	}

	var header Header
	if err := json.Unmarshal(lines[0], &header); err != nil {
		return fmt.Errorf("failed to parse session header: %w", err)
	}
	if header.Type != "session" {
		return fmt.Errorf("invalid session header type: %s", header.Type)
	}
	if header.Version == 0 {
		header.Version = 1
	}

	entries := make([]Entry, 0, len(lines)-1)
	for _, line := range lines[1:] {
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		if e.Type == "" {
			continue
		}
		entries = append(entries, e)
	}

	m.header = header
	m.entries = entries
	m.sessionFile = path
	m.rebuildIndexAndMigrate()
	return nil
}

func (m *Manager) OpenLatest() (bool, error) {
	infos, err := ListSessions(m.sessionDir)
	if err != nil {
		return false, err
	}
	if len(infos) == 0 {
		return false, nil
	}
	if err := m.Open(infos[0].Path); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) SetLeaf(leafID string) error {
	if leafID == "" {
		m.leafID = ""
		return nil
	}
	if _, ok := m.byID[leafID]; !ok {
		return fmt.Errorf("leaf id not found: %s", leafID)
	}
	m.leafID = leafID
	return nil
}

func (m *Manager) AppendMessage(message types.Message) (Entry, error) {
	e := Entry{
		Type:      "message",
		ID:        m.nextID(),
		ParentID:  m.leafID,
		Timestamp: nowTS(),
		Message:   &message,
	}
	if err := m.appendEntry(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (m *Manager) AppendModelChange(provider, modelID string) (Entry, error) {
	e := Entry{
		Type:      "model_change",
		ID:        m.nextID(),
		ParentID:  m.leafID,
		Timestamp: nowTS(),
		Provider:  provider,
		ModelID:   modelID,
	}
	if err := m.appendEntry(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (m *Manager) AppendThinkingLevel(level string) (Entry, error) {
	e := Entry{
		Type:          "thinking_level_change",
		ID:            m.nextID(),
		ParentID:      m.leafID,
		Timestamp:     nowTS(),
		ThinkingLevel: level,
	}
	if err := m.appendEntry(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (m *Manager) AppendCompaction(summary, firstKeptEntryID string, tokensBefore int) (Entry, error) {
	e := Entry{
		Type:           "compaction",
		ID:             m.nextID(),
		ParentID:       m.leafID,
		Timestamp:      nowTS(),
		Summary:        summary,
		FirstKeptEntry: firstKeptEntryID,
		TokensBefore:   tokensBefore,
	}
	if err := m.appendEntry(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (m *Manager) AppendBranchSummary(fromID, summary string) (Entry, error) {
	e := Entry{
		Type:      "branch_summary",
		ID:        m.nextID(),
		ParentID:  m.leafID,
		Timestamp: nowTS(),
		FromID:    fromID,
		Summary:   summary,
	}
	if err := m.appendEntry(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (m *Manager) AppendLabel(targetID, label string) (Entry, error) {
	e := Entry{
		Type:      "label",
		ID:        m.nextID(),
		ParentID:  m.leafID,
		Timestamp: nowTS(),
		TargetID:  targetID,
		Label:     label,
	}
	if err := m.appendEntry(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (m *Manager) AppendSessionName(name string) (Entry, error) {
	e := Entry{
		Type:      "session_info",
		ID:        m.nextID(),
		ParentID:  m.leafID,
		Timestamp: nowTS(),
		Name:      name,
	}
	if err := m.appendEntry(e); err != nil {
		return Entry{}, err
	}
	return e, nil
}

func (m *Manager) Branch(leafID string) []Entry {
	if leafID == "" {
		leafID = m.leafID
	}
	if leafID == "" && len(m.entries) > 0 {
		leafID = m.entries[len(m.entries)-1].ID
	}
	if leafID == "" {
		return nil
	}

	path := make([]Entry, 0)
	cur := leafID
	seen := map[string]bool{}
	for cur != "" {
		if seen[cur] {
			break
		}
		seen[cur] = true
		i, ok := m.byID[cur]
		if !ok {
			break
		}
		e := m.entries[i]
		path = append(path, e)
		cur = e.ParentID
	}
	for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
		path[i], path[j] = path[j], path[i]
	}
	return path
}

func (m *Manager) BuildContext(systemPrompt string, leafID string, tools []types.Tool) Context {
	branch := m.Branch(leafID)
	messages := make([]types.Message, 0, len(branch))
	thinking := "medium"
	var provider, modelID string

	for _, e := range branch {
		switch e.Type {
		case "message":
			if e.Message != nil {
				messages = append(messages, *e.Message)
			}
		case "custom_message":
			if len(e.Content) > 0 {
				messages = append(messages, types.Message{
					Role:      types.RoleUser,
					Timestamp: types.NowMillis(),
					Content:   e.Content,
				})
			}
		case "thinking_level_change":
			if e.ThinkingLevel != "" {
				thinking = e.ThinkingLevel
			}
		case "model_change":
			provider = e.Provider
			modelID = e.ModelID
		case "compaction":
			if e.Summary != "" {
				messages = append(messages, types.TextMessage(types.RoleUser, "<summary>\n"+e.Summary+"\n</summary>"))
			}
		case "branch_summary":
			if e.Summary != "" {
				messages = append(messages, types.TextMessage(types.RoleUser, "<branch_summary>\n"+e.Summary+"\n</branch_summary>"))
			}
		}
	}

	ctx := Context{
		Messages:      messages,
		ThinkingLevel: thinking,
		ModelProvider: provider,
		ModelID:       modelID,
	}
	_ = systemPrompt
	_ = tools
	return ctx
}

func (m *Manager) appendEntry(entry Entry) error {
	if m.sessionFile == "" {
		return errors.New("session is not initialized")
	}
	f, err := os.OpenFile(m.sessionFile, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	m.entries = append(m.entries, entry)
	m.byID[entry.ID] = len(m.entries) - 1
	m.leafID = entry.ID
	return nil
}

func (m *Manager) flushAll() error {
	if m.sessionFile == "" {
		return errors.New("session file path is empty")
	}
	f, err := os.Create(m.sessionFile)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	if err := enc.Encode(m.header); err != nil {
		return err
	}
	for _, e := range m.entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) rebuildIndexAndMigrate() {
	m.byID = map[string]int{}
	changed := false
	lastID := ""
	for i := range m.entries {
		if m.entries[i].ID == "" {
			m.entries[i].ID = m.nextIDSeen(m.byID)
			changed = true
		}
		if m.entries[i].ParentID == "" && lastID != "" {
			m.entries[i].ParentID = lastID
			changed = true
		}
		m.byID[m.entries[i].ID] = i
		lastID = m.entries[i].ID
	}
	if m.header.Version < CurrentSessionVersion {
		m.header.Version = CurrentSessionVersion
		changed = true
	}
	if len(m.entries) > 0 {
		m.leafID = m.entries[len(m.entries)-1].ID
	}
	if changed {
		_ = m.flushAll()
	}
}

func (m *Manager) nextID() string {
	return m.nextIDSeen(m.byID)
}

func (m *Manager) nextIDSeen(seen map[string]int) string {
	for i := 0; i < 100; i++ {
		id := shortID()
		if _, ok := seen[id]; !ok {
			return id
		}
	}
	return shortID() + shortID()
}

func shortID() string {
	b := make([]byte, 4)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())[:8]
	}
	return hex.EncodeToString(b)
}

func nowTS() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func ListSessions(sessionDir string) ([]Info, error) {
	if err := EnsureSessionDir(sessionDir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(sessionDir)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0)
	for _, de := range entries {
		if de.IsDir() || !strings.HasSuffix(de.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(sessionDir, de.Name())
		info, err := parseSessionInfo(path)
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Modified.After(out[j].Modified)
	})
	return out, nil
}

func parseSessionInfo(path string) (Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return Info{}, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	lineNo := 0
	var h Header
	var firstMsg string
	msgCount := 0
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}
		lineNo++
		if lineNo == 1 {
			if err := json.Unmarshal([]byte(line), &h); err != nil {
				return Info{}, err
			}
			continue
		}
		var l fileLine
		if err := json.Unmarshal([]byte(line), &l); err != nil {
			continue
		}
		if l.Type == "message" {
			msgCount++
			if firstMsg == "" {
				var e Entry
				if err := json.Unmarshal([]byte(line), &e); err == nil && e.Message != nil {
					for _, b := range e.Message.Content {
						if b.Type == "text" && b.Text != "" {
							firstMsg = b.Text
							if len(firstMsg) > 120 {
								firstMsg = firstMsg[:120]
							}
							break
						}
					}
				}
			}
		}
	}
	st, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	created := st.ModTime()
	if h.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, h.Timestamp); err == nil {
			created = t
		}
	}
	return Info{
		Path:         path,
		ID:           h.ID,
		CWD:          h.CWD,
		Created:      created,
		Modified:     st.ModTime(),
		MessageCount: msgCount,
		FirstMessage: firstMsg,
	}, nil
}
