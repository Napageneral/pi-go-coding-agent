package providers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/config"
	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

type googleProvider struct {
	model       types.Model
	providerCfg config.ProviderConfig
	apiKey      string
	httpClient  *http.Client
}

func NewGoogleProvider(model types.Model, providerCfg config.ProviderConfig, apiKey string) types.Provider {
	return &googleProvider{
		model:       model,
		providerCfg: providerCfg,
		apiKey:      apiKey,
		httpClient:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *googleProvider) API() string { return p.model.API }

type googleReq struct {
	SystemInstruction map[string]any   `json:"system_instruction,omitempty"`
	Contents          []map[string]any `json:"contents"`
	Tools             []map[string]any `json:"tools,omitempty"`
	GenerationConfig  map[string]any   `json:"generationConfig,omitempty"`
}

type googleVertexReq struct {
	SystemInstruction map[string]any   `json:"systemInstruction,omitempty"`
	Contents          []map[string]any `json:"contents"`
	Tools             []map[string]any `json:"tools,omitempty"`
	GenerationConfig  map[string]any   `json:"generationConfig,omitempty"`
}

type googleUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
}

type googleResp struct {
	Candidates []struct {
		Content struct {
			Parts []map[string]any `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	Usage googleUsage `json:"usageMetadata"`
	Error any         `json:"error,omitempty"`
}

type cloudCodeAssistRequest struct {
	Project     string `json:"project"`
	Model       string `json:"model"`
	UserAgent   string `json:"userAgent,omitempty"`
	RequestID   string `json:"requestId,omitempty"`
	RequestType string `json:"requestType,omitempty"`
	Request     struct {
		Contents          []map[string]any `json:"contents"`
		SessionID         string           `json:"sessionId,omitempty"`
		SystemInstruction map[string]any   `json:"systemInstruction,omitempty"`
		GenerationConfig  map[string]any   `json:"generationConfig,omitempty"`
		Tools             []map[string]any `json:"tools,omitempty"`
	} `json:"request"`
}

type cloudCodeAssistChunk struct {
	Response struct {
		Candidates []struct {
			Content struct {
				Parts []map[string]any `json:"parts"`
			} `json:"content"`
			FinishReason string `json:"finishReason"`
		} `json:"candidates"`
		UsageMetadata googleUsage `json:"usageMetadata"`
	} `json:"response"`
}

func (p *googleProvider) Complete(req types.CompletionRequest) (types.CompletionResponse, error) {
	switch strings.ToLower(req.Model.API) {
	case "google-gemini-cli":
		return p.completeCloudCodeAssist(req)
	case "google-vertex":
		return p.completeVertex(req)
	default:
		return p.completeGenerative(req)
	}
}

func (p *googleProvider) completeGenerative(req types.CompletionRequest) (types.CompletionResponse, error) {
	payload := googleReq{
		Contents:         googleMessages(req.Context, req.Model),
		Tools:            googleTools(req.Context.Tools),
		GenerationConfig: map[string]any{"temperature": 0.2},
	}
	if req.Context.SystemPrompt != "" {
		payload.SystemInstruction = map[string]any{
			"parts": []map[string]any{{"text": req.Context.SystemPrompt}},
		}
	}
	if req.Options.Temperature != nil {
		payload.GenerationConfig["temperature"] = *req.Options.Temperature
	}
	if req.Options.MaxTokens != nil {
		payload.GenerationConfig["maxOutputTokens"] = *req.Options.MaxTokens
	}

	base := strings.TrimSuffix(req.Model.BaseURL, "/")
	if base == "" {
		base = strings.TrimSuffix(p.providerCfg.BaseURL, "/")
	}
	if base == "" {
		base = "https://generativelanguage.googleapis.com"
	}

	apiKey := p.resolveAPIKey(req)
	if apiKey == "" {
		return types.CompletionResponse{}, fmt.Errorf("missing API key for google provider")
	}

	modelID := url.PathEscape(req.Model.ID)
	apiURL := base + "/v1beta/models/" + modelID + ":generateContent?key=" + url.QueryEscape(apiKey)

	b, _ := json.Marshal(payload)
	hreq, _ := http.NewRequestWithContext(providerContext(req.Options), http.MethodPost, apiURL, bytes.NewReader(b))
	hreq.Header.Set("Content-Type", "application/json")
	for k, v := range mergeHeaders(req.Model.Headers, req.Options.Headers, p.providerCfg.Headers) {
		hreq.Header.Set(k, v)
	}

	resp, err := p.httpClient.Do(hreq)
	if err != nil {
		return types.CompletionResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return types.CompletionResponse{}, fmt.Errorf("provider error %d: %s", resp.StatusCode, string(body))
	}

	var out googleResp
	if err := json.Unmarshal(body, &out); err != nil {
		return types.CompletionResponse{}, err
	}
	return googleResponseToCompletion(req.Model, out)
}

func (p *googleProvider) completeCloudCodeAssist(req types.CompletionRequest) (types.CompletionResponse, error) {
	token, projectID, err := parseCloudCodeAssistAuth(p.resolveAPIKey(req))
	if err != nil {
		return types.CompletionResponse{}, err
	}

	isAntigravity := req.Model.Provider == "google-antigravity"
	base := strings.TrimSuffix(req.Model.BaseURL, "/")
	if base == "" {
		base = strings.TrimSuffix(p.providerCfg.BaseURL, "/")
	}
	if base == "" {
		if isAntigravity {
			base = "https://daily-cloudcode-pa.sandbox.googleapis.com"
		} else {
			base = "https://cloudcode-pa.googleapis.com"
		}
	}
	endpoints := []string{base}
	if isAntigravity && base != "https://cloudcode-pa.googleapis.com" {
		endpoints = append(endpoints, "https://cloudcode-pa.googleapis.com")
	}

	payload := cloudCodeAssistRequest{
		Project:   projectID,
		Model:     req.Model.ID,
		UserAgent: "pi-coding-agent",
		RequestID: "pi-" + randomID(),
	}
	if isAntigravity {
		payload.UserAgent = "antigravity"
		payload.RequestType = "agent"
	}
	payload.Request.Contents = googleMessages(req.Context, req.Model)
	if req.Options.SessionID != "" {
		payload.Request.SessionID = req.Options.SessionID
	}
	if req.Context.SystemPrompt != "" {
		payload.Request.SystemInstruction = map[string]any{
			"parts": []map[string]any{{"text": req.Context.SystemPrompt}},
		}
	}
	gen := map[string]any{}
	if req.Options.Temperature != nil {
		gen["temperature"] = *req.Options.Temperature
	}
	if req.Options.MaxTokens != nil {
		gen["maxOutputTokens"] = *req.Options.MaxTokens
	}
	if len(gen) > 0 {
		payload.Request.GenerationConfig = gen
	}
	if tools := googleTools(req.Context.Tools); len(tools) > 0 {
		payload.Request.Tools = tools
	}

	b, _ := json.Marshal(payload)
	var resp *http.Response
	var lastErr error
	for _, endpoint := range endpoints {
		apiURL := endpoint + "/v1internal:streamGenerateContent?alt=sse"
		hreq, _ := http.NewRequestWithContext(providerContext(req.Options), http.MethodPost, apiURL, bytes.NewReader(b))
		hreq.Header.Set("Content-Type", "application/json")
		hreq.Header.Set("Accept", "text/event-stream")
		hreq.Header.Set("Authorization", "Bearer "+token)
		if isAntigravity {
			ver := os.Getenv("PI_AI_ANTIGRAVITY_VERSION")
			if ver == "" {
				ver = "1.15.8"
			}
			hreq.Header.Set("User-Agent", "antigravity/"+ver+" darwin/arm64")
		} else {
			hreq.Header.Set("User-Agent", "google-cloud-sdk vscode_cloudshelleditor/0.1")
		}
		hreq.Header.Set("X-Goog-Api-Client", "gl-node/22.17.0")
		hreq.Header.Set("Client-Metadata", `{"ideType":"IDE_UNSPECIFIED","platform":"PLATFORM_UNSPECIFIED","pluginType":"GEMINI"}`)
		if strings.Contains(strings.ToLower(req.Model.ID), "claude") && strings.Contains(strings.ToLower(req.Model.ID), "thinking") {
			hreq.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14")
		}
		for k, v := range mergeHeaders(req.Model.Headers, req.Options.Headers, p.providerCfg.Headers) {
			hreq.Header.Set(k, v)
		}

		resp, err = p.httpClient.Do(hreq)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode < 300 {
			break
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		lastErr = fmt.Errorf("provider error %d: %s", resp.StatusCode, string(body))
		resp = nil
	}
	if resp == nil {
		if lastErr != nil {
			return types.CompletionResponse{}, lastErr
		}
		return types.CompletionResponse{}, fmt.Errorf("cloud code assist request failed")
	}
	defer resp.Body.Close()

	assistant := types.Message{
		Role:      types.RoleAssistant,
		Timestamp: types.NowMillis(),
		API:       req.Model.API,
		Provider:  req.Model.Provider,
		Model:     req.Model.ID,
	}
	toolCalls := make([]types.ToolCall, 0, 4)
	hasToolCalls := false

	s := bufio.NewScanner(resp.Body)
	s.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		jsonStr := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if jsonStr == "" {
			continue
		}

		var chunk cloudCodeAssistChunk
		if err := json.Unmarshal([]byte(jsonStr), &chunk); err != nil {
			continue
		}

		if len(chunk.Response.Candidates) > 0 {
			cand := chunk.Response.Candidates[0]
			calls, hasCalls := appendGoogleParts(&assistant, cand.Content.Parts)
			if len(calls) > 0 {
				toolCalls = append(toolCalls, calls...)
			}
			hasToolCalls = hasToolCalls || hasCalls
			if cand.FinishReason != "" {
				assistant.StopReason = mapGoogleStopReason(cand.FinishReason, hasToolCalls)
			}
		}
		assistant.Usage = googleUsageToUsage(chunk.Response.UsageMetadata, true)
	}
	if err := s.Err(); err != nil {
		return types.CompletionResponse{}, err
	}
	if assistant.StopReason == "" {
		assistant.StopReason = mapGoogleStopReason("STOP", hasToolCalls)
	}
	if len(assistant.Content) == 0 && len(toolCalls) == 0 {
		return types.CompletionResponse{}, fmt.Errorf("empty response")
	}
	return types.CompletionResponse{Assistant: assistant, ToolCalls: toolCalls}, nil
}

func (p *googleProvider) completeVertex(req types.CompletionRequest) (types.CompletionResponse, error) {
	project := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
	if project == "" {
		project = strings.TrimSpace(os.Getenv("GCLOUD_PROJECT"))
	}
	if project == "" {
		return types.CompletionResponse{}, fmt.Errorf("google-vertex requires GOOGLE_CLOUD_PROJECT or GCLOUD_PROJECT")
	}
	location := strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_LOCATION"))
	if location == "" {
		return types.CompletionResponse{}, fmt.Errorf("google-vertex requires GOOGLE_CLOUD_LOCATION")
	}

	accessToken, err := p.resolveVertexToken(req)
	if err != nil {
		return types.CompletionResponse{}, err
	}

	base := strings.TrimSuffix(req.Model.BaseURL, "/")
	if base == "" {
		base = strings.TrimSuffix(p.providerCfg.BaseURL, "/")
	}
	if base == "" {
		base = fmt.Sprintf("https://%s-aiplatform.googleapis.com", location)
	}

	payload := googleVertexReq{
		Contents:         googleMessages(req.Context, req.Model),
		Tools:            googleTools(req.Context.Tools),
		GenerationConfig: map[string]any{},
	}
	if req.Context.SystemPrompt != "" {
		payload.SystemInstruction = map[string]any{"parts": []map[string]any{{"text": req.Context.SystemPrompt}}}
	}
	if req.Options.Temperature != nil {
		payload.GenerationConfig["temperature"] = *req.Options.Temperature
	}
	if req.Options.MaxTokens != nil {
		payload.GenerationConfig["maxOutputTokens"] = *req.Options.MaxTokens
	}
	if len(payload.GenerationConfig) == 0 {
		payload.GenerationConfig = nil
	}

	modelID := url.PathEscape(req.Model.ID)
	apiURL := fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent", base, url.PathEscape(project), url.PathEscape(location), modelID)

	b, _ := json.Marshal(payload)
	hreq, _ := http.NewRequestWithContext(providerContext(req.Options), http.MethodPost, apiURL, bytes.NewReader(b))
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Authorization", "Bearer "+accessToken)
	for k, v := range mergeHeaders(req.Model.Headers, req.Options.Headers, p.providerCfg.Headers) {
		hreq.Header.Set(k, v)
	}

	resp, err := p.httpClient.Do(hreq)
	if err != nil {
		return types.CompletionResponse{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return types.CompletionResponse{}, fmt.Errorf("provider error %d: %s", resp.StatusCode, string(body))
	}

	var out googleResp
	if err := json.Unmarshal(body, &out); err != nil {
		return types.CompletionResponse{}, err
	}
	return googleResponseToCompletion(req.Model, out)
}

func (p *googleProvider) resolveAPIKey(req types.CompletionRequest) string {
	apiKey := req.Options.APIKey
	if apiKey == "" {
		apiKey = p.apiKey
	}
	if apiKey == "" {
		apiKey = p.providerCfg.APIKey
	}
	return apiKey
}

func (p *googleProvider) resolveVertexToken(req types.CompletionRequest) (string, error) {
	token := strings.TrimSpace(p.resolveAPIKey(req))
	if token != "" && token != "<authenticated>" {
		return token, nil
	}
	baseCtx := req.Options.Context
	if baseCtx == nil {
		baseCtx = context.Background()
	}
	ctx, cancel := context.WithTimeout(baseCtx, 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "gcloud", "auth", "application-default", "print-access-token").Output()
	if err != nil {
		out, err = exec.CommandContext(ctx, "gcloud", "auth", "print-access-token").Output()
		if err != nil {
			return "", fmt.Errorf("google-vertex requires GOOGLE_VERTEX_ACCESS_TOKEN or gcloud auth access token")
		}
	}
	token = strings.TrimSpace(string(out))
	if token == "" {
		return "", fmt.Errorf("google-vertex access token is empty")
	}
	return token, nil
}

func parseCloudCodeAssistAuth(raw string) (token string, projectID string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("google-gemini-cli requires OAuth credentials (token + projectId)")
	}
	if strings.HasPrefix(raw, "{") {
		var parsed struct {
			Token     string `json:"token"`
			ProjectID string `json:"projectId"`
		}
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return "", "", fmt.Errorf("invalid google-gemini-cli credential JSON")
		}
		token = strings.TrimSpace(parsed.Token)
		projectID = strings.TrimSpace(parsed.ProjectID)
	} else {
		token = raw
		projectID = strings.TrimSpace(os.Getenv("GOOGLE_CLOUD_PROJECT"))
		if projectID == "" {
			projectID = strings.TrimSpace(os.Getenv("GCLOUD_PROJECT"))
		}
	}
	if token == "" || projectID == "" {
		return "", "", fmt.Errorf("google-gemini-cli credentials missing token or projectId")
	}
	return token, projectID, nil
}

func googleResponseToCompletion(model types.Model, out googleResp) (types.CompletionResponse, error) {
	if len(out.Candidates) == 0 {
		return types.CompletionResponse{}, fmt.Errorf("empty response")
	}
	cand := out.Candidates[0]
	assistant := types.Message{
		Role:      types.RoleAssistant,
		Timestamp: types.NowMillis(),
		API:       model.API,
		Provider:  model.Provider,
		Model:     model.ID,
		Usage:     googleUsageToUsage(out.Usage, false),
	}
	toolCalls, hasToolCalls := appendGoogleParts(&assistant, cand.Content.Parts)
	assistant.StopReason = mapGoogleStopReason(cand.FinishReason, hasToolCalls)
	return types.CompletionResponse{Assistant: assistant, ToolCalls: toolCalls}, nil
}

func appendGoogleParts(assistant *types.Message, parts []map[string]any) ([]types.ToolCall, bool) {
	toolCalls := make([]types.ToolCall, 0)
	hasToolCalls := false
	for _, part := range parts {
		if text, ok := part["text"].(string); ok && text != "" {
			assistant.Content = append(assistant.Content, types.ContentBlock{Type: "text", Text: text})
		}
		if fc, ok := part["functionCall"].(map[string]interface{}); ok {
			name, _ := fc["name"].(string)
			args := map[string]any{}
			if parsed, ok := fc["args"].(map[string]interface{}); ok {
				args = parsed
			}
			id, _ := fc["id"].(string)
			if strings.TrimSpace(id) == "" {
				id = "fc_" + randomID()
			}
			assistant.Content = append(assistant.Content, types.ContentBlock{Type: "toolCall", ID: id, Name: name, Arguments: args})
			toolCalls = append(toolCalls, types.ToolCall{ID: id, Name: name, Arguments: args})
			hasToolCalls = true
		}
	}
	return toolCalls, hasToolCalls
}

func googleUsageToUsage(usage googleUsage, subtractCached bool) types.Usage {
	input := usage.PromptTokenCount
	if subtractCached {
		input -= usage.CachedContentTokenCount
		if input < 0 {
			input = 0
		}
	}
	output := usage.CandidatesTokenCount + usage.ThoughtsTokenCount
	total := usage.TotalTokenCount
	if total == 0 {
		total = input + output + usage.CachedContentTokenCount
	}
	return types.Usage{
		Input:     input,
		Output:    output,
		CacheRead: usage.CachedContentTokenCount,
		Total:     total,
	}
}

func mapGoogleStopReason(reason string, hasToolCalls bool) string {
	if hasToolCalls {
		return "toolUse"
	}
	switch strings.ToUpper(strings.TrimSpace(reason)) {
	case "", "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	default:
		return "error"
	}
}

func googleTools(tools []types.Tool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	decls := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		decls = append(decls, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"parameters":  t.Parameters,
		})
	}
	return []map[string]any{{"functionDeclarations": decls}}
}

func googleMessages(ctx types.Context, model types.Model) []map[string]any {
	transformed := transformMessages(ctx.Messages, model, func(id string) string {
		return normalizeGoogleToolCallID(model, id)
	})
	needCallID := requiresGoogleToolCallID(model.ID)

	out := make([]map[string]any, 0, len(transformed))
	for i := 0; i < len(transformed); i++ {
		m := transformed[i]
		switch m.Role {
		case types.RoleUser:
			out = append(out, map[string]any{"role": "user", "parts": []map[string]any{{"text": messageText(m)}}})
		case types.RoleAssistant:
			parts := []map[string]any{}
			for _, c := range m.Content {
				if c.Type == "text" {
					parts = append(parts, map[string]any{"text": c.Text})
				} else if c.Type == "thinking" {
					if strings.TrimSpace(c.Thinking) != "" {
						parts = append(parts, map[string]any{"text": c.Thinking})
					}
				} else if c.Type == "toolCall" {
					fc := map[string]any{"name": c.Name, "args": c.Arguments}
					if needCallID && c.ID != "" {
						fc["id"] = c.ID
					}
					parts = append(parts, map[string]any{"functionCall": fc})
				}
			}
			if len(parts) > 0 {
				out = append(out, map[string]any{"role": "model", "parts": parts})
			}
		case types.RoleTool:
			parts := []map[string]any{}
			for i < len(transformed) && transformed[i].Role == types.RoleTool {
				tm := transformed[i]
				text := messageText(tm)
				if strings.TrimSpace(text) == "" {
					text = "(no output)"
				}
				resp := map[string]any{"output": text}
				if tm.IsError {
					resp = map[string]any{"error": text}
				}
				fr := map[string]any{
					"name":     tm.ToolName,
					"response": resp,
				}
				if needCallID && tm.ToolCallID != "" {
					fr["id"] = tm.ToolCallID
				}
				parts = append(parts, map[string]any{"functionResponse": fr})
				i++
			}
			i--
			out = append(out, map[string]any{
				"role":  "user",
				"parts": parts,
			})
		}
	}
	if len(out) == 0 {
		out = append(out, map[string]any{"role": "user", "parts": []map[string]any{{"text": ""}}})
	}
	return out
}

func requiresGoogleToolCallID(modelID string) bool {
	return strings.HasPrefix(modelID, "claude-") || strings.HasPrefix(modelID, "gpt-oss-")
}

func normalizeGoogleToolCallID(model types.Model, id string) string {
	if id == "" {
		return id
	}
	if !requiresGoogleToolCallID(model.ID) {
		return id
	}
	id = sanitizeID(id)
	if len(id) > 64 {
		id = id[:64]
	}
	return id
}

func randomID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
