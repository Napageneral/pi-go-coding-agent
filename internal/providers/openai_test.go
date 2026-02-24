package providers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/config"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

func TestOpenAIMessagesFromContextMistralCompat(t *testing.T) {
	model := types.Model{
		ID:       "devstral-medium-latest",
		API:      "openai-completions",
		Provider: "mistral",
		BaseURL:  "https://api.mistral.ai/v1",
	}
	compat := getOpenAICompat(model)

	ctx := types.Context{
		Messages: []types.Message{
			{
				Role:     types.RoleAssistant,
				API:      "openai-completions",
				Provider: "openai",
				Model:    "gpt-5.1-codex",
				Content: []types.ContentBlock{
					{
						Type:      "toolCall",
						ID:        "call|with-illegal+++id",
						Name:      "read",
						Arguments: map[string]any{"path": "a.txt"},
					},
				},
			},
			types.TextMessage(types.RoleUser, "continue"),
		},
	}

	msgs := openAIMessagesFromContext(ctx, model, compat)
	if len(msgs) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(msgs))
	}

	if msgs[1]["role"] != "tool" {
		t.Fatalf("expected synthetic tool message at index 1, got %#v", msgs[1]["role"])
	}
	if _, ok := msgs[1]["name"]; !ok {
		t.Fatal("expected tool name in tool result for mistral compatibility")
	}

	assistantCalls, ok := msgs[0]["tool_calls"].([]map[string]any)
	if !ok {
		t.Fatalf("expected tool_calls array on assistant message, got %#v", msgs[0]["tool_calls"])
	}
	if len(assistantCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(assistantCalls))
	}
	id, _ := assistantCalls[0]["id"].(string)
	if len(id) != 9 {
		t.Fatalf("expected normalized mistral tool id length 9, got %q", id)
	}
}

func TestParseOpenAIMessageText(t *testing.T) {
	rawString := json.RawMessage(`"hello"`)
	if got := parseOpenAIMessageText(rawString); got != "hello" {
		t.Fatalf("parseOpenAIMessageText string = %q, want hello", got)
	}

	rawParts := json.RawMessage(`[{"type":"text","text":"a"},{"type":"text","text":"b"}]`)
	if got := parseOpenAIMessageText(rawParts); got != "a\nb" {
		t.Fatalf("parseOpenAIMessageText parts = %q, want a\\nb", got)
	}
}

func TestResolveCodexResponsesURL(t *testing.T) {
	tests := []struct {
		base string
		want string
	}{
		{"https://chatgpt.com/backend-api", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://chatgpt.com/backend-api/codex", "https://chatgpt.com/backend-api/codex/responses"},
		{"https://chatgpt.com/backend-api/codex/responses", "https://chatgpt.com/backend-api/codex/responses"},
		{"", "https://chatgpt.com/backend-api/codex/responses"},
	}
	for _, tt := range tests {
		if got := resolveCodexResponsesURL(tt.base); got != tt.want {
			t.Fatalf("resolveCodexResponsesURL(%q) = %q, want %q", tt.base, got, tt.want)
		}
	}
}

func TestExtractCodexAccountID(t *testing.T) {
	payload := `{"https://api.openai.com/auth":{"chatgpt_account_id":"acc_123"}}`
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(payload))
	token := header + "." + body + ".sig"

	accountID, err := extractCodexAccountID(token)
	if err != nil {
		t.Fatalf("extractCodexAccountID returned error: %v", err)
	}
	if accountID != "acc_123" {
		t.Fatalf("extractCodexAccountID = %q, want acc_123", accountID)
	}

	if _, err := extractCodexAccountID("not-a-jwt"); err == nil {
		t.Fatal("expected invalid token error")
	}
}

func TestResolveAzureDeploymentName(t *testing.T) {
	mapping := "gpt-5.2=dep-prod,gpt-4o=dep-fast"
	if got := resolveAzureDeploymentName("gpt-5.2", mapping); got != "dep-prod" {
		t.Fatalf("resolveAzureDeploymentName mapped = %q, want dep-prod", got)
	}
	if got := resolveAzureDeploymentName("gpt-4.1", mapping); got != "gpt-4.1" {
		t.Fatalf("resolveAzureDeploymentName fallback = %q, want gpt-4.1", got)
	}
}

func TestResolveAzureResponsesURL(t *testing.T) {
	url := resolveAzureResponsesURL("https://my.openai.azure.com/openai/deployments/mydep", "2024-10-21")
	if !strings.Contains(url, "/openai/deployments/mydep/responses") {
		t.Fatalf("unexpected deployments url: %s", url)
	}
	if !strings.Contains(url, "api-version=2024-10-21") {
		t.Fatalf("missing api-version in deployments url: %s", url)
	}
}

func TestOpenAIResponsesToCompletionToolUse(t *testing.T) {
	var out openAIResponsesResponse
	body := []byte(`{
		"status": "completed",
		"output": [
			{
				"type": "message",
				"content": [{"type": "output_text", "text": "Ready"}]
			},
			{
				"type": "function_call",
				"id": "fc_1",
				"call_id": "call_1",
				"name": "read",
				"arguments": "{\"path\":\"README.md\"}"
			}
		],
		"usage": {
			"input_tokens": 10,
			"output_tokens": 5,
			"total_tokens": 15,
			"input_tokens_details": {"cached_tokens": 2}
		}
	}`)
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	resp, err := openAIResponsesToCompletion(types.Model{
		ID:       "gpt-5.1-codex",
		API:      "openai-responses",
		Provider: "openai",
	}, out)
	if err != nil {
		t.Fatalf("openAIResponsesToCompletion error: %v", err)
	}
	if resp.Assistant.StopReason != "toolUse" {
		t.Fatalf("stop reason = %q, want toolUse", resp.Assistant.StopReason)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].ID != "call_1|fc_1" {
		t.Fatalf("tool call id = %q, want call_1|fc_1", resp.ToolCalls[0].ID)
	}
	if resp.Assistant.Usage.Input != 8 {
		t.Fatalf("usage input = %d, want 8", resp.Assistant.Usage.Input)
	}
}

func TestCompleteCodexResponsesSSE(t *testing.T) {
	payloadSeen := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/codex/responses" {
			t.Fatalf("expected /codex/responses, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("expected SSE accept header, got %q", got)
		}

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request failed: %v", err)
		}
		if stream, ok := body["stream"].(bool); !ok || !stream {
			t.Fatalf("expected stream=true for codex request, got %#v", body["stream"])
		}
		payloadSeen = true

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: response.completed\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"ready\"}]}],\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3,\"input_tokens_details\":{\"cached_tokens\":0}}}}\n\n")
	}))
	defer srv.Close()

	tokenPayload := `{"https://api.openai.com/auth":{"chatgpt_account_id":"acc_test"}}`
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body := base64.RawURLEncoding.EncodeToString([]byte(tokenPayload))
	token := header + "." + body + ".sig"

	model := types.Model{
		ID:       "gpt-5.3-codex",
		API:      "openai-codex-responses",
		Provider: "openai-codex",
		BaseURL:  srv.URL,
	}
	p := NewOpenAICompatibleProvider(model, config.ProviderConfig{}, token)
	prov := p.(*openAICompatibleProvider)
	prov.httpClient = srv.Client()

	req := types.CompletionRequest{
		Model: model,
		Context: types.Context{
			SystemPrompt: "You are concise.",
			Messages: []types.Message{
				types.TextMessage(types.RoleUser, "Say ready"),
			},
		},
	}

	resp, err := prov.Complete(req)
	if err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if !payloadSeen {
		t.Fatal("expected handler to receive request payload")
	}
	if got := strings.TrimSpace(messageText(resp.Assistant)); got != "ready" {
		t.Fatalf("assistant text = %q, want ready", got)
	}
	if resp.Assistant.StopReason != "stop" {
		t.Fatalf("stop reason = %q, want stop", resp.Assistant.StopReason)
	}
}
