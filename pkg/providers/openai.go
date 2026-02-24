package providers

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/config"
	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

const (
	defaultOpenAIBaseURL   = "https://api.openai.com/v1"
	defaultCodexBaseURL    = "https://chatgpt.com/backend-api"
	defaultAzureAPIVersion = "v1"
)

type openAICompatibleProvider struct {
	model       types.Model
	providerCfg config.ProviderConfig
	apiKey      string
	httpClient  *http.Client
}

func NewOpenAICompatibleProvider(model types.Model, providerCfg config.ProviderConfig, apiKey string) types.Provider {
	return &openAICompatibleProvider{
		model:       model,
		providerCfg: providerCfg,
		apiKey:      apiKey,
		httpClient:  &http.Client{Timeout: 120 * time.Second},
	}
}

func (p *openAICompatibleProvider) API() string { return p.model.API }

type openAICompat struct {
	SupportsStore                 bool
	SupportsDeveloperRole         bool
	SupportsReasoningEffort       bool
	MaxTokensField                string
	RequiresToolResultName        bool
	RequiresAssistantAfterToolRes bool
	RequiresThinkingAsText        bool
	RequiresMistralToolIDs        bool
	SupportsStrictMode            bool
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Role      string          `json:"role"`
			Content   json.RawMessage `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
	Error any `json:"error,omitempty"`
}

type openAIResponsesResponse struct {
	Status string `json:"status"`
	Output []struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Summary   []struct {
			Text string `json:"text"`
		} `json:"summary"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
		InputDetails struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

type openAIResponsesMode int

const (
	openAIResponsesModeOpenAI openAIResponsesMode = iota
	openAIResponsesModeCodex
	openAIResponsesModeAzure
)

func (p *openAICompatibleProvider) Complete(req types.CompletionRequest) (types.CompletionResponse, error) {
	switch strings.ToLower(req.Model.API) {
	case "openai-responses":
		return p.completeResponses(req, openAIResponsesModeOpenAI)
	case "openai-codex-responses":
		return p.completeResponses(req, openAIResponsesModeCodex)
	case "azure-openai-responses":
		return p.completeResponses(req, openAIResponsesModeAzure)
	default:
		return p.completeChatCompletions(req)
	}
}

func (p *openAICompatibleProvider) completeChatCompletions(req types.CompletionRequest) (types.CompletionResponse, error) {
	compat := getOpenAICompat(req.Model)

	payload := map[string]any{
		"model":    req.Model.ID,
		"messages": openAIMessagesFromContext(req.Context, req.Model, compat),
		"stream":   false,
	}
	tools := openAITools(req.Context.Tools, compat)
	if len(tools) > 0 {
		payload["tools"] = tools
	} else if hasToolHistory(req.Context.Messages) {
		// Some compatible endpoints require tools=[] when tool history exists.
		payload["tools"] = []map[string]any{}
	}
	if req.Options.Temperature != nil {
		payload["temperature"] = *req.Options.Temperature
	}
	if req.Options.MaxTokens != nil {
		if compat.MaxTokensField == "max_tokens" {
			payload["max_tokens"] = *req.Options.MaxTokens
		} else {
			payload["max_completion_tokens"] = *req.Options.MaxTokens
		}
	}
	if compat.SupportsStore {
		payload["store"] = false
	}

	b, _ := json.Marshal(payload)
	base := p.resolveBaseURL(req, defaultOpenAIBaseURL)
	apiURL := resolveOpenAIChatCompletionsURL(base, req)

	hreq, _ := http.NewRequestWithContext(providerContext(req.Options), http.MethodPost, apiURL, bytes.NewReader(b))
	hreq.Header.Set("Content-Type", "application/json")
	apiKey := p.resolveAPIKey(req)
	if req.Model.API == "azure-openai-responses" {
		hreq.Header.Set("api-key", apiKey)
	} else {
		hreq.Header.Set("Authorization", "Bearer "+apiKey)
	}
	headers := mergeHeaders(req.Model.Headers, req.Options.Headers, p.providerCfg.Headers)
	for k, v := range copilotDynamicHeaders(req.Model.Provider, req.Context.Messages) {
		headers[k] = v
	}
	for k, v := range headers {
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

	var out openAIChatResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return types.CompletionResponse{}, err
	}
	if len(out.Choices) == 0 {
		return types.CompletionResponse{}, fmt.Errorf("empty response from provider")
	}
	choice := out.Choices[0]

	assistant := types.Message{
		Role:       types.RoleAssistant,
		Timestamp:  types.NowMillis(),
		API:        req.Model.API,
		Provider:   req.Model.Provider,
		Model:      req.Model.ID,
		StopReason: mapOpenAIChatStopReason(choice.FinishReason),
		Usage: types.Usage{
			Input:  out.Usage.PromptTokens,
			Output: out.Usage.CompletionTokens,
			Total:  out.Usage.TotalTokens,
		},
	}
	text := parseOpenAIMessageText(choice.Message.Content)
	if text != "" {
		assistant.Content = append(assistant.Content, types.ContentBlock{Type: "text", Text: text})
	}
	toolCalls := make([]types.ToolCall, 0, len(choice.Message.ToolCalls))
	for _, tc := range choice.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = "call_" + randomOpenAIID()
		}
		args := map[string]interface{}{}
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &args)
		}
		assistant.Content = append(assistant.Content, types.ContentBlock{
			Type:      "toolCall",
			ID:        id,
			Name:      tc.Function.Name,
			Arguments: args,
		})
		toolCalls = append(toolCalls, types.ToolCall{ID: id, Name: tc.Function.Name, Arguments: args})
	}
	return types.CompletionResponse{Assistant: assistant, ToolCalls: toolCalls}, nil
}

func (p *openAICompatibleProvider) completeResponses(
	req types.CompletionRequest,
	mode openAIResponsesMode,
) (types.CompletionResponse, error) {
	apiKey := p.resolveAPIKey(req)
	if apiKey == "" {
		return types.CompletionResponse{}, fmt.Errorf("missing API key for provider %s", req.Model.Provider)
	}

	includeSystemPrompt := mode != openAIResponsesModeCodex
	input := openAIResponsesInput(req.Context, req.Model, includeSystemPrompt)

	modelID := req.Model.ID
	stream := mode == openAIResponsesModeCodex
	payload := map[string]any{
		"model":  modelID,
		"input":  input,
		"stream": stream,
		"store":  false,
	}
	if req.Options.MaxTokens != nil {
		payload["max_output_tokens"] = *req.Options.MaxTokens
	}
	if req.Options.Temperature != nil {
		payload["temperature"] = *req.Options.Temperature
	}
	if req.Options.SessionID != "" {
		payload["prompt_cache_key"] = req.Options.SessionID
	}
	switch mode {
	case openAIResponsesModeCodex:
		payload["instructions"] = req.Context.SystemPrompt
		payload["tool_choice"] = "auto"
		payload["parallel_tool_calls"] = true
	case openAIResponsesModeAzure:
		modelID = resolveAzureDeploymentName(req.Model.ID, os.Getenv("AZURE_OPENAI_DEPLOYMENT_NAME_MAP"))
		payload["model"] = modelID
	}

	if tools := openAIResponsesTools(req.Context.Tools, mode != openAIResponsesModeCodex); len(tools) > 0 {
		payload["tools"] = tools
	}

	endpoint, err := p.resolveResponsesEndpoint(req, mode)
	if err != nil {
		return types.CompletionResponse{}, err
	}

	b, _ := json.Marshal(payload)
	hreq, _ := http.NewRequestWithContext(providerContext(req.Options), http.MethodPost, endpoint, bytes.NewReader(b))
	hreq.Header.Set("Content-Type", "application/json")
	if mode == openAIResponsesModeCodex {
		hreq.Header.Set("Accept", "text/event-stream")
	} else {
		hreq.Header.Set("Accept", "application/json")
	}

	headers := mergeHeaders(req.Model.Headers, req.Options.Headers, p.providerCfg.Headers)
	if mode != openAIResponsesModeCodex {
		for k, v := range copilotDynamicHeaders(req.Model.Provider, req.Context.Messages) {
			headers[k] = v
		}
	}

	switch mode {
	case openAIResponsesModeCodex:
		accountID, err := extractCodexAccountID(apiKey)
		if err != nil {
			return types.CompletionResponse{}, err
		}
		hreq.Header.Set("Authorization", "Bearer "+apiKey)
		hreq.Header.Set("chatgpt-account-id", accountID)
		hreq.Header.Set("OpenAI-Beta", "responses=experimental")
		hreq.Header.Set("originator", "pi")
		hreq.Header.Set("User-Agent", "pi-go")
		if req.Options.SessionID != "" {
			hreq.Header.Set("session_id", req.Options.SessionID)
		}
	case openAIResponsesModeAzure:
		hreq.Header.Set("api-key", apiKey)
	default:
		hreq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	for k, v := range headers {
		hreq.Header.Set(k, v)
	}

	resp, err := p.httpClient.Do(hreq)
	if err != nil {
		return types.CompletionResponse{}, err
	}
	defer resp.Body.Close()

	if mode == openAIResponsesModeCodex {
		if resp.StatusCode >= 300 {
			body, _ := io.ReadAll(resp.Body)
			return types.CompletionResponse{}, fmt.Errorf("provider error %d: %s", resp.StatusCode, string(body))
		}
		out, err := parseCodexSSEResponse(resp.Body)
		if err != nil {
			return types.CompletionResponse{}, err
		}
		return openAIResponsesToCompletion(req.Model, out)
	}

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return types.CompletionResponse{}, fmt.Errorf("provider error %d: %s", resp.StatusCode, string(body))
	}

	var out openAIResponsesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return types.CompletionResponse{}, err
	}
	return openAIResponsesToCompletion(req.Model, out)
}

func (p *openAICompatibleProvider) resolveBaseURL(req types.CompletionRequest, fallback string) string {
	base := strings.TrimSpace(strings.TrimSuffix(req.Model.BaseURL, "/"))
	if base == "" {
		base = strings.TrimSpace(strings.TrimSuffix(p.providerCfg.BaseURL, "/"))
	}
	if base == "" {
		base = fallback
	}
	return strings.TrimSuffix(base, "/")
}

func (p *openAICompatibleProvider) resolveAPIKey(req types.CompletionRequest) string {
	apiKey := req.Options.APIKey
	if apiKey == "" {
		apiKey = p.apiKey
	}
	if apiKey == "" {
		apiKey = p.providerCfg.APIKey
	}
	return apiKey
}

func (p *openAICompatibleProvider) resolveResponsesEndpoint(
	req types.CompletionRequest,
	mode openAIResponsesMode,
) (string, error) {
	switch mode {
	case openAIResponsesModeCodex:
		base := p.resolveBaseURL(req, defaultCodexBaseURL)
		return resolveCodexResponsesURL(base), nil
	case openAIResponsesModeAzure:
		base := p.resolveBaseURL(req, "")
		base = resolveAzureBaseURL(base)
		if base == "" {
			return "", fmt.Errorf("azure-openai-responses requires AZURE_OPENAI_BASE_URL or AZURE_OPENAI_RESOURCE_NAME")
		}
		apiVersion := strings.TrimSpace(os.Getenv("AZURE_OPENAI_API_VERSION"))
		if apiVersion == "" {
			apiVersion = defaultAzureAPIVersion
		}
		return resolveAzureResponsesURL(base, apiVersion), nil
	default:
		base := p.resolveBaseURL(req, defaultOpenAIBaseURL)
		return resolveOpenAIResponsesURL(base), nil
	}
}

func resolveOpenAIChatCompletionsURL(base string, req types.CompletionRequest) string {
	base = strings.TrimSuffix(base, "/")
	apiURL := base + "/chat/completions"
	if req.Model.API == "azure-openai-responses" {
		if strings.Contains(base, "/openai/deployments/") {
			apiURL = base + "/chat/completions?api-version=2024-10-21"
		} else {
			apiURL = base + "/openai/deployments/" + req.Model.ID + "/chat/completions?api-version=2024-10-21"
		}
	}
	return apiURL
}

func resolveOpenAIResponsesURL(base string) string {
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		base = defaultOpenAIBaseURL
	}
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	return base + "/responses"
}

func resolveCodexResponsesURL(base string) string {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	if base == "" {
		base = defaultCodexBaseURL
	}
	if strings.HasSuffix(base, "/codex/responses") {
		return base
	}
	if strings.HasSuffix(base, "/codex") {
		return base + "/responses"
	}
	return base + "/codex/responses"
}

func resolveAzureBaseURL(base string) string {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	if base != "" {
		return base
	}
	if v := strings.TrimSpace(os.Getenv("AZURE_OPENAI_BASE_URL")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	if v := strings.TrimSpace(os.Getenv("AZURE_OPENAI_ENDPOINT")); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	if resource := strings.TrimSpace(os.Getenv("AZURE_OPENAI_RESOURCE_NAME")); resource != "" {
		return "https://" + resource + ".openai.azure.com/openai/v1"
	}
	return ""
}

func resolveAzureResponsesURL(base string, apiVersion string) string {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	if strings.Contains(base, "/openai/deployments/") {
		u := base + "/responses"
		if !strings.Contains(u, "api-version=") {
			sep := "?"
			if strings.Contains(u, "?") {
				sep = "&"
			}
			u += sep + "api-version=" + url.QueryEscape(apiVersion)
		}
		return u
	}
	if strings.HasSuffix(base, "/openai/v1") {
		return base + "/responses"
	}
	if strings.Contains(base, ".openai.azure.com") {
		return base + "/openai/v1/responses"
	}
	if strings.HasSuffix(base, "/responses") {
		return base
	}
	return base + "/responses"
}

func resolveAzureDeploymentName(modelID string, mapping string) string {
	mapping = strings.TrimSpace(mapping)
	if mapping == "" {
		return modelID
	}
	for _, entry := range strings.Split(mapping, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		if strings.EqualFold(key, modelID) && val != "" {
			return val
		}
	}
	return modelID
}

func extractCodexAccountID(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("failed to extract accountId from token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some tokens are padded.
		payload, err = base64.URLEncoding.DecodeString(padBase64(parts[1]))
		if err != nil {
			return "", fmt.Errorf("failed to extract accountId from token")
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", fmt.Errorf("failed to extract accountId from token")
	}
	authRaw, ok := parsed["https://api.openai.com/auth"]
	if !ok {
		return "", fmt.Errorf("failed to extract accountId from token")
	}
	authObj, ok := authRaw.(map[string]any)
	if !ok {
		return "", fmt.Errorf("failed to extract accountId from token")
	}
	accountID, _ := authObj["chatgpt_account_id"].(string)
	if strings.TrimSpace(accountID) == "" {
		return "", fmt.Errorf("failed to extract accountId from token")
	}
	return accountID, nil
}

func padBase64(s string) string {
	if m := len(s) % 4; m != 0 {
		return s + strings.Repeat("=", 4-m)
	}
	return s
}

func openAIResponsesTools(tools []types.Tool, includeStrict bool) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		item := map[string]any{
			"type":        "function",
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.Parameters,
		}
		if includeStrict {
			item["strict"] = false
		}
		out = append(out, item)
	}
	return out
}

func openAIResponsesInput(ctx types.Context, model types.Model, includeSystemPrompt bool) []any {
	transformed := transformMessages(ctx.Messages, model, func(id string) string {
		return normalizeOpenAIResponsesToolCallID(model, id)
	})
	input := make([]any, 0, len(transformed)+1)
	if includeSystemPrompt && strings.TrimSpace(ctx.SystemPrompt) != "" {
		role := "system"
		if model.Reasoning {
			role = "developer"
		}
		input = append(input, map[string]any{
			"role": role,
			"content": []map[string]any{
				{"type": "input_text", "text": ctx.SystemPrompt},
			},
		})
	}

	msgID := 0
	for _, msg := range transformed {
		switch msg.Role {
		case types.RoleUser:
			parts := make([]map[string]any, 0, len(msg.Content))
			for _, c := range msg.Content {
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) != "" {
						parts = append(parts, map[string]any{"type": "input_text", "text": c.Text})
					}
				case "image":
					if c.Data != "" && c.MimeType != "" {
						parts = append(parts, map[string]any{
							"type":      "input_image",
							"detail":    "auto",
							"image_url": fmt.Sprintf("data:%s;base64,%s", c.MimeType, c.Data),
						})
					}
				}
			}
			if len(parts) == 0 {
				parts = append(parts, map[string]any{"type": "input_text", "text": messageText(msg)})
			}
			input = append(input, map[string]any{
				"role":    "user",
				"content": parts,
			})
		case types.RoleAssistant:
			for _, c := range msg.Content {
				switch c.Type {
				case "text":
					if strings.TrimSpace(c.Text) == "" {
						continue
					}
					input = append(input, map[string]any{
						"type":   "message",
						"role":   "assistant",
						"status": "completed",
						"id":     fmt.Sprintf("msg_%d", msgID),
						"content": []map[string]any{
							{
								"type":        "output_text",
								"text":        c.Text,
								"annotations": []any{},
							},
						},
					})
					msgID++
				case "thinking":
					if strings.TrimSpace(c.Thinking) == "" {
						continue
					}
					input = append(input, map[string]any{
						"type":   "message",
						"role":   "assistant",
						"status": "completed",
						"id":     fmt.Sprintf("msg_%d", msgID),
						"content": []map[string]any{
							{
								"type":        "output_text",
								"text":        c.Thinking,
								"annotations": []any{},
							},
						},
					})
					msgID++
				case "toolCall":
					callID, itemID := splitToolCallIDForResponses(c.ID)
					argBytes, _ := json.Marshal(c.Arguments)
					input = append(input, map[string]any{
						"type":      "function_call",
						"id":        itemID,
						"call_id":   callID,
						"name":      c.Name,
						"arguments": string(argBytes),
					})
				}
			}
		case types.RoleTool:
			output := messageText(msg)
			if strings.TrimSpace(output) == "" {
				output = "(no output)"
			}
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": extractCallID(msg.ToolCallID),
				"output":  output,
			})
		}
	}

	if len(input) == 0 {
		input = append(input, map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": ""},
			},
		})
	}
	return input
}

func openAIResponsesToCompletion(model types.Model, out openAIResponsesResponse) (types.CompletionResponse, error) {
	assistant := types.Message{
		Role:      types.RoleAssistant,
		Timestamp: types.NowMillis(),
		API:       model.API,
		Provider:  model.Provider,
		Model:     model.ID,
		Usage: types.Usage{
			Input:     out.Usage.InputTokens - out.Usage.InputDetails.CachedTokens,
			Output:    out.Usage.OutputTokens,
			CacheRead: out.Usage.InputDetails.CachedTokens,
			Total:     out.Usage.TotalTokens,
		},
	}
	if assistant.Usage.Input < 0 {
		assistant.Usage.Input = 0
	}
	if assistant.Usage.Total == 0 {
		assistant.Usage.Total = assistant.Usage.Input + assistant.Usage.Output + assistant.Usage.CacheRead
	}

	toolCalls := make([]types.ToolCall, 0, 4)
	for _, item := range out.Output {
		switch item.Type {
		case "message":
			var b strings.Builder
			for _, c := range item.Content {
				switch c.Type {
				case "output_text":
					if c.Text != "" {
						if b.Len() > 0 {
							b.WriteByte('\n')
						}
						b.WriteString(c.Text)
					}
				case "refusal":
					if c.Refusal != "" {
						if b.Len() > 0 {
							b.WriteByte('\n')
						}
						b.WriteString(c.Refusal)
					}
				}
			}
			if strings.TrimSpace(b.String()) != "" {
				assistant.Content = append(assistant.Content, types.ContentBlock{Type: "text", Text: b.String()})
			}
		case "reasoning":
			parts := make([]string, 0, len(item.Summary))
			for _, s := range item.Summary {
				if strings.TrimSpace(s.Text) != "" {
					parts = append(parts, s.Text)
				}
			}
			if len(parts) > 0 {
				assistant.Content = append(assistant.Content, types.ContentBlock{
					Type:     "thinking",
					Thinking: strings.Join(parts, "\n\n"),
				})
			}
		case "function_call":
			args := map[string]any{}
			if item.Arguments != "" {
				_ = json.Unmarshal([]byte(item.Arguments), &args)
			}
			callID := extractCallID(item.CallID)
			blockID := callID
			if strings.TrimSpace(item.ID) != "" {
				blockID = callID + "|" + strings.TrimSpace(item.ID)
			}
			assistant.Content = append(assistant.Content, types.ContentBlock{
				Type:      "toolCall",
				ID:        blockID,
				Name:      item.Name,
				Arguments: args,
			})
			toolCalls = append(toolCalls, types.ToolCall{
				ID:        blockID,
				Name:      item.Name,
				Arguments: args,
			})
		}
	}

	assistant.StopReason = mapOpenAIResponsesStopReason(out.Status)
	if len(toolCalls) > 0 && assistant.StopReason == "stop" {
		assistant.StopReason = "toolUse"
	}
	if len(assistant.Content) == 0 && len(toolCalls) == 0 {
		return types.CompletionResponse{}, fmt.Errorf("empty response from provider")
	}
	return types.CompletionResponse{Assistant: assistant, ToolCalls: toolCalls}, nil
}

func parseCodexSSEResponse(r io.Reader) (openAIResponsesResponse, error) {
	reader := bufio.NewReader(r)
	dataLines := make([]string, 0, 8)

	parseEvent := func() (*openAIResponsesResponse, error) {
		if len(dataLines) == 0 {
			return nil, nil
		}
		payload := strings.TrimSpace(strings.Join(dataLines, "\n"))
		dataLines = dataLines[:0]
		if payload == "" || payload == "[DONE]" {
			return nil, nil
		}

		var evt struct {
			Type     string          `json:"type"`
			Code     string          `json:"code"`
			Message  string          `json:"message"`
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			// Ignore malformed non-data chunks.
			return nil, nil
		}

		switch evt.Type {
		case "error":
			msg := strings.TrimSpace(evt.Message)
			if msg == "" {
				msg = strings.TrimSpace(evt.Code)
			}
			if msg == "" {
				msg = "codex stream error"
			}
			return nil, fmt.Errorf("provider error: %s", msg)
		case "response.failed":
			var failed struct {
				Response struct {
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				} `json:"response"`
			}
			_ = json.Unmarshal([]byte(payload), &failed)
			msg := strings.TrimSpace(failed.Response.Error.Message)
			if msg == "" {
				msg = "codex response failed"
			}
			return nil, fmt.Errorf("provider error: %s", msg)
		case "response.completed", "response.done":
			if len(evt.Response) == 0 {
				return nil, fmt.Errorf("provider error: codex stream missing completed response")
			}
			var out openAIResponsesResponse
			if err := json.Unmarshal(evt.Response, &out); err != nil {
				return nil, err
			}
			if strings.TrimSpace(out.Status) == "" {
				out.Status = "completed"
			}
			return &out, nil
		default:
			return nil, nil
		}
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil && err != io.EOF {
			return openAIResponsesResponse{}, err
		}

		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			if out, parseErr := parseEvent(); parseErr != nil {
				return openAIResponsesResponse{}, parseErr
			} else if out != nil {
				return *out, nil
			}
		} else if strings.HasPrefix(trimmed, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		}

		if err == io.EOF {
			if out, parseErr := parseEvent(); parseErr != nil {
				return openAIResponsesResponse{}, parseErr
			} else if out != nil {
				return *out, nil
			}
			break
		}
	}

	return openAIResponsesResponse{}, fmt.Errorf("provider error: codex stream ended before completion")
}

func mapOpenAIChatStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "", "stop":
		return "stop"
	case "length", "max_tokens":
		return "length"
	case "tool_calls":
		return "toolUse"
	default:
		return "error"
	}
}

func mapOpenAIResponsesStopReason(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "completed":
		return "stop"
	case "incomplete":
		return "length"
	case "failed", "cancelled":
		return "error"
	case "queued", "in_progress":
		return "stop"
	default:
		return "error"
	}
}

func normalizeOpenAIResponsesToolCallID(model types.Model, id string) string {
	if id == "" {
		return id
	}
	if model.Provider != "openai" && model.Provider != "openai-codex" && model.Provider != "opencode" && model.Provider != "azure-openai-responses" {
		return id
	}
	if !strings.Contains(id, "|") {
		return trimAndSanitizeID(id, 64)
	}
	callID, itemID := splitToolCallIDForResponses(id)
	return callID + "|" + itemID
}

func splitToolCallIDForResponses(id string) (string, string) {
	callID := ""
	itemID := ""
	if strings.Contains(id, "|") {
		parts := strings.SplitN(id, "|", 2)
		callID = trimAndSanitizeID(parts[0], 64)
		itemID = trimAndSanitizeID(parts[1], 64)
	} else {
		callID = trimAndSanitizeID(id, 64)
	}
	callID = strings.TrimRight(callID, "_")
	itemID = strings.TrimRight(itemID, "_")
	if callID == "" {
		callID = "call_" + randomOpenAIID()
	}
	if itemID == "" {
		itemID = "fc_" + randomOpenAIID()
	}
	if !strings.HasPrefix(itemID, "fc") {
		itemID = "fc_" + itemID
		if len(itemID) > 64 {
			itemID = itemID[:64]
		}
	}
	return callID, itemID
}

func extractCallID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return "call_" + randomOpenAIID()
	}
	if strings.Contains(id, "|") {
		id = strings.SplitN(id, "|", 2)[0]
	}
	id = trimAndSanitizeID(id, 64)
	if strings.TrimSpace(id) == "" {
		return "call_" + randomOpenAIID()
	}
	return id
}

func trimAndSanitizeID(id string, max int) string {
	id = sanitizeID(id)
	if len(id) > max {
		id = id[:max]
	}
	return id
}

func openAITools(tools []types.Tool, compat openAICompat) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		fn := map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"parameters":  tool.Parameters,
		}
		if compat.SupportsStrictMode {
			fn["strict"] = false
		}
		out = append(out, map[string]any{
			"type":     "function",
			"function": fn,
		})
	}
	return out
}

func openAIMessagesFromContext(ctx types.Context, model types.Model, compat openAICompat) []map[string]any {
	transformed := transformMessages(ctx.Messages, model, func(id string) string {
		return normalizeOpenAIToolCallID(model, compat, id)
	})

	messages := make([]map[string]any, 0, len(ctx.Messages)+1)
	if ctx.SystemPrompt != "" {
		role := "system"
		if model.Reasoning && compat.SupportsDeveloperRole {
			role = "developer"
		}
		messages = append(messages, map[string]any{"role": role, "content": ctx.SystemPrompt})
	}
	lastRole := ""
	for _, m := range transformed {
		if compat.RequiresAssistantAfterToolRes && lastRole == types.RoleTool && m.Role == types.RoleUser {
			messages = append(messages, map[string]any{"role": "assistant", "content": "I have processed the tool results."})
		}

		switch m.Role {
		case types.RoleUser:
			messages = append(messages, map[string]any{"role": "user", "content": messageText(m)})
			lastRole = types.RoleUser
		case types.RoleAssistant:
			msg := map[string]any{"role": "assistant", "content": messageText(m)}
			var tcs []map[string]any
			for _, c := range m.Content {
				if c.Type == "toolCall" {
					id := c.ID
					if id == "" {
						id = "call_" + randomOpenAIID()
					}
					argBytes, _ := json.Marshal(c.Arguments)
					tcs = append(tcs, map[string]any{
						"id":   id,
						"type": "function",
						"function": map[string]any{
							"name":      c.Name,
							"arguments": string(argBytes),
						},
					})
				}
			}
			if len(tcs) > 0 {
				msg["tool_calls"] = tcs
			}
			content := msg["content"].(string)
			if strings.TrimSpace(content) == "" && len(tcs) == 0 {
				continue
			}
			messages = append(messages, msg)
			lastRole = types.RoleAssistant
		case types.RoleTool:
			content := messageText(m)
			if strings.TrimSpace(content) == "" {
				content = "(no output)"
			}
			messages = append(messages, map[string]any{
				"role":         "tool",
				"tool_call_id": m.ToolCallID,
				"content":      content,
			})
			if compat.RequiresToolResultName && m.ToolName != "" {
				messages[len(messages)-1]["name"] = m.ToolName
			}
			lastRole = types.RoleTool
		}
	}
	return messages
}

func hasToolHistory(messages []types.Message) bool {
	for _, msg := range messages {
		if msg.Role == types.RoleTool {
			return true
		}
		if msg.Role == types.RoleAssistant {
			for _, block := range msg.Content {
				if block.Type == "toolCall" {
					return true
				}
			}
		}
	}
	return false
}

func parseOpenAIMessageText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		return str
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		var b strings.Builder
		for _, part := range parts {
			if part.Type != "text" || part.Text == "" {
				continue
			}
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(part.Text)
		}
		return b.String()
	}
	return ""
}

func normalizeOpenAIToolCallID(model types.Model, compat openAICompat, id string) string {
	if id == "" {
		return id
	}
	if compat.RequiresMistralToolIDs {
		return normalizeMistralToolID(id)
	}
	if strings.Contains(id, "|") {
		callID := strings.SplitN(id, "|", 2)[0]
		sanitized := sanitizeID(callID)
		if len(sanitized) > 40 {
			sanitized = sanitized[:40]
		}
		return sanitized
	}
	if model.Provider == "openai" && len(id) > 40 {
		return id[:40]
	}
	return id
}

func normalizeMistralToolID(id string) string {
	sanitized := make([]byte, 0, 9)
	for i := 0; i < len(id); i++ {
		ch := id[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') {
			sanitized = append(sanitized, ch)
		}
	}
	for len(sanitized) < 9 {
		sanitized = append(sanitized, "ABCDEFGHI"[len(sanitized)])
	}
	if len(sanitized) > 9 {
		sanitized = sanitized[:9]
	}
	return string(sanitized)
}

func sanitizeID(id string) string {
	b := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		ch := id[i]
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-' {
			b = append(b, ch)
		} else {
			b = append(b, '_')
		}
	}
	return string(b)
}

func getOpenAICompat(model types.Model) openAICompat {
	baseURL := strings.ToLower(model.BaseURL)
	provider := strings.ToLower(model.Provider)

	isZAI := provider == "zai" || strings.Contains(baseURL, "api.z.ai")
	isMistral := provider == "mistral" || strings.Contains(baseURL, "mistral.ai")
	isGrok := provider == "xai" || strings.Contains(baseURL, "api.x.ai")
	isNonStandard := provider == "cerebras" ||
		strings.Contains(baseURL, "cerebras.ai") ||
		isGrok ||
		isMistral ||
		strings.Contains(baseURL, "chutes.ai") ||
		strings.Contains(baseURL, "deepseek.com") ||
		isZAI ||
		provider == "opencode" ||
		strings.Contains(baseURL, "opencode.ai")

	compat := openAICompat{
		SupportsStore:                 !isNonStandard,
		SupportsDeveloperRole:         !isNonStandard,
		SupportsReasoningEffort:       !isGrok && !isZAI,
		MaxTokensField:                "max_completion_tokens",
		RequiresToolResultName:        isMistral,
		RequiresAssistantAfterToolRes: false,
		RequiresThinkingAsText:        isMistral,
		RequiresMistralToolIDs:        isMistral,
		SupportsStrictMode:            true,
	}
	if isMistral || strings.Contains(baseURL, "chutes.ai") {
		compat.MaxTokensField = "max_tokens"
	}

	// Allow model-level compat overrides from models.json.
	if v, ok := model.Compat["supportsStore"].(bool); ok {
		compat.SupportsStore = v
	}
	if v, ok := model.Compat["supportsDeveloperRole"].(bool); ok {
		compat.SupportsDeveloperRole = v
	}
	if v, ok := model.Compat["supportsReasoningEffort"].(bool); ok {
		compat.SupportsReasoningEffort = v
	}
	if v, ok := model.Compat["maxTokensField"].(string); ok && v != "" {
		compat.MaxTokensField = v
	}
	if v, ok := model.Compat["requiresToolResultName"].(bool); ok {
		compat.RequiresToolResultName = v
	}
	if v, ok := model.Compat["requiresAssistantAfterToolResult"].(bool); ok {
		compat.RequiresAssistantAfterToolRes = v
	}
	if v, ok := model.Compat["requiresThinkingAsText"].(bool); ok {
		compat.RequiresThinkingAsText = v
	}
	if v, ok := model.Compat["requiresMistralToolIds"].(bool); ok {
		compat.RequiresMistralToolIDs = v
	}
	if v, ok := model.Compat["supportsStrictMode"].(bool); ok {
		compat.SupportsStrictMode = v
	}

	return compat
}

func randomOpenAIID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func copilotDynamicHeaders(provider string, messages []types.Message) map[string]string {
	if provider != "github-copilot" {
		return nil
	}
	headers := map[string]string{
		"X-Initiator":   inferCopilotInitiator(messages),
		"Openai-Intent": "conversation-edits",
	}
	if hasCopilotVisionInput(messages) {
		headers["Copilot-Vision-Request"] = "true"
	}
	return headers
}

func inferCopilotInitiator(messages []types.Message) string {
	if len(messages) == 0 {
		return "user"
	}
	last := messages[len(messages)-1]
	if last.Role != types.RoleUser {
		return "agent"
	}
	return "user"
}

func hasCopilotVisionInput(messages []types.Message) bool {
	for _, msg := range messages {
		switch msg.Role {
		case types.RoleUser, types.RoleTool:
			for _, block := range msg.Content {
				if block.Type == "image" {
					return true
				}
			}
		}
	}
	return false
}

func providerContext(opts types.CompletionOptions) context.Context {
	if opts.Context != nil {
		return opts.Context
	}
	return context.Background()
}
