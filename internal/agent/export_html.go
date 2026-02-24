package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"path/filepath"
	"strings"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/config"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/session"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

func (r *Runtime) ExportHTML(outputPath string) (string, error) {
	sessionFile := strings.TrimSpace(r.session.SessionFile())
	if sessionFile == "" {
		return "", errors.New("cannot export in-memory session to HTML")
	}

	entries := r.session.Entries()
	if len(entries) == 0 {
		return "", errors.New("nothing to export yet - start a conversation first")
	}

	path := strings.TrimSpace(outputPath)
	if path == "" {
		base := strings.TrimSuffix(filepath.Base(sessionFile), filepath.Ext(sessionFile))
		if base == "" {
			base = "session"
		}
		path = fmt.Sprintf("%s-session-%s.html", config.AppName, base)
	}

	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs
	}

	content := renderSessionHTML(r.session.Header(), sessionFile, entries)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func renderSessionHTML(header session.Header, sessionFile string, entries []session.Entry) string {
	var sb strings.Builder
	sb.WriteString("<!doctype html><html><head><meta charset=\"utf-8\">")
	sb.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	sb.WriteString("<title>pi session export</title>")
	sb.WriteString("<style>")
	sb.WriteString("body{font-family:ui-sans-serif,system-ui,-apple-system,Segoe UI,Roboto,Arial,sans-serif;margin:0;background:#111;color:#eee;}")
	sb.WriteString("main{max-width:980px;margin:0 auto;padding:24px;}h1{margin:0 0 12px 0;}h2{margin:0 0 8px 0;font-size:14px;color:#bbb;text-transform:uppercase;letter-spacing:.06em;}")
	sb.WriteString(".meta{background:#1b1b1f;border:1px solid #2a2a30;border-radius:10px;padding:12px;margin-bottom:16px;}")
	sb.WriteString(".entry{background:#18181c;border:1px solid #26262b;border-radius:10px;padding:12px;margin-bottom:10px;}")
	sb.WriteString(".entry-header{font-size:12px;color:#9ca3af;margin-bottom:8px;}pre{white-space:pre-wrap;word-break:break-word;background:#0f0f12;border:1px solid #26262b;padding:10px;border-radius:8px;margin:6px 0;}")
	sb.WriteString(".badge{display:inline-block;background:#2b2b31;color:#d4d4d8;border-radius:999px;padding:2px 8px;font-size:11px;margin-right:8px;}")
	sb.WriteString("</style></head><body><main>")

	sb.WriteString("<h1>Session Export</h1>")
	sb.WriteString("<div class=\"meta\">")
	sb.WriteString("<h2>Session</h2>")
	sb.WriteString("<div><strong>ID:</strong> " + html.EscapeString(header.ID) + "</div>")
	sb.WriteString("<div><strong>File:</strong> " + html.EscapeString(sessionFile) + "</div>")
	sb.WriteString("<div><strong>CWD:</strong> " + html.EscapeString(header.CWD) + "</div>")
	sb.WriteString("<div><strong>Created:</strong> " + html.EscapeString(header.Timestamp) + "</div>")
	sb.WriteString("</div>")

	for _, entry := range entries {
		sb.WriteString("<article class=\"entry\">")
		sb.WriteString("<div class=\"entry-header\"><span class=\"badge\">" + html.EscapeString(entry.Type) + "</span>")
		sb.WriteString("id=" + html.EscapeString(entry.ID))
		if strings.TrimSpace(entry.ParentID) != "" {
			sb.WriteString(" parent=" + html.EscapeString(entry.ParentID))
		}
		if strings.TrimSpace(entry.Timestamp) != "" {
			sb.WriteString(" time=" + html.EscapeString(entry.Timestamp))
		}
		sb.WriteString("</div>")
		renderSessionEntryBody(&sb, entry)
		sb.WriteString("</article>")
	}

	sb.WriteString("</main></body></html>")
	return sb.String()
}

func renderSessionEntryBody(sb *strings.Builder, entry session.Entry) {
	switch entry.Type {
	case "message":
		if entry.Message == nil {
			return
		}
		renderMessage(sb, *entry.Message)
	case "compaction":
		if strings.TrimSpace(entry.Summary) != "" {
			sb.WriteString("<pre>" + html.EscapeString(entry.Summary) + "</pre>")
		}
		if strings.TrimSpace(entry.FirstKeptEntry) != "" {
			sb.WriteString("<div><strong>firstKeptEntryId:</strong> " + html.EscapeString(entry.FirstKeptEntry) + "</div>")
		}
		if entry.TokensBefore > 0 {
			sb.WriteString(fmt.Sprintf("<div><strong>tokensBefore:</strong> %d</div>", entry.TokensBefore))
		}
		if len(entry.Details) > 0 {
			appendJSONPre(sb, entry.Details)
		}
	case "branch_summary":
		if strings.TrimSpace(entry.Summary) != "" {
			sb.WriteString("<pre>" + html.EscapeString(entry.Summary) + "</pre>")
		}
		if len(entry.Details) > 0 {
			appendJSONPre(sb, entry.Details)
		}
	case "custom_message":
		for _, block := range entry.Content {
			renderContentBlock(sb, block)
		}
		if len(entry.CustomData) > 0 {
			appendJSONPre(sb, entry.CustomData)
		}
	case "custom":
		if strings.TrimSpace(entry.CustomType) != "" {
			sb.WriteString("<div><strong>customType:</strong> " + html.EscapeString(entry.CustomType) + "</div>")
		}
		if len(entry.CustomData) > 0 {
			appendJSONPre(sb, entry.CustomData)
		}
	case "session_info":
		sb.WriteString("<div><strong>name:</strong> " + html.EscapeString(entry.Name) + "</div>")
	case "label":
		sb.WriteString("<div><strong>target:</strong> " + html.EscapeString(entry.TargetID) + "</div>")
		sb.WriteString("<div><strong>label:</strong> " + html.EscapeString(entry.Label) + "</div>")
	case "model_change":
		sb.WriteString("<div><strong>model:</strong> " + html.EscapeString(entry.Provider) + "/" + html.EscapeString(entry.ModelID) + "</div>")
	case "thinking_level_change":
		sb.WriteString("<div><strong>thinkingLevel:</strong> " + html.EscapeString(entry.ThinkingLevel) + "</div>")
	default:
		appendJSONPre(sb, entry)
	}
}

func renderMessage(sb *strings.Builder, message types.Message) {
	sb.WriteString("<div><strong>role:</strong> " + html.EscapeString(message.Role) + "</div>")
	if strings.TrimSpace(message.ToolName) != "" {
		sb.WriteString("<div><strong>toolName:</strong> " + html.EscapeString(message.ToolName) + "</div>")
	}
	if strings.TrimSpace(message.ToolCallID) != "" {
		sb.WriteString("<div><strong>toolCallId:</strong> " + html.EscapeString(message.ToolCallID) + "</div>")
	}
	if strings.TrimSpace(message.StopReason) != "" {
		sb.WriteString("<div><strong>stopReason:</strong> " + html.EscapeString(message.StopReason) + "</div>")
	}
	if strings.TrimSpace(message.Error) != "" {
		sb.WriteString("<pre>" + html.EscapeString(message.Error) + "</pre>")
	}
	for _, block := range message.Content {
		renderContentBlock(sb, block)
	}
	if usage := message.Usage; usage.Total > 0 || usage.Input > 0 || usage.Output > 0 || usage.CacheRead > 0 || usage.CacheWrite > 0 {
		appendJSONPre(sb, usage)
	}
}

func renderContentBlock(sb *strings.Builder, block types.ContentBlock) {
	switch block.Type {
	case "text":
		if strings.TrimSpace(block.Text) == "" {
			return
		}
		sb.WriteString("<pre>" + html.EscapeString(block.Text) + "</pre>")
	case "thinking":
		text := strings.TrimSpace(block.Thinking)
		if text == "" {
			return
		}
		sb.WriteString("<pre>" + html.EscapeString(text) + "</pre>")
	case "toolCall":
		sb.WriteString("<div><strong>toolCall:</strong> " + html.EscapeString(block.Name) + "</div>")
		if len(block.Arguments) > 0 {
			appendJSONPre(sb, block.Arguments)
		}
	case "image":
		label := block.MimeType
		if strings.TrimSpace(label) == "" {
			label = "image"
		}
		sb.WriteString("<div><strong>attachment:</strong> " + html.EscapeString(label) + "</div>")
	default:
		appendJSONPre(sb, block)
	}
}

func appendJSONPre(sb *strings.Builder, value any) {
	blob, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	sb.WriteString("<pre>" + html.EscapeString(string(blob)) + "</pre>")
}
