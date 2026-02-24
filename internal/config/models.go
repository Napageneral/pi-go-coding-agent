package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/types"
)

type ProviderConfig struct {
	BaseURL   string                 `json:"baseUrl,omitempty"`
	APIKey    string                 `json:"apiKey,omitempty"`
	API       string                 `json:"api,omitempty"`
	Headers   map[string]string      `json:"headers,omitempty"`
	Models    []types.Model          `json:"models,omitempty"`
	Overrides map[string]interface{} `json:"modelOverrides,omitempty"`
}

type modelsConfigFile struct {
	Providers map[string]ProviderConfig `json:"providers"`
}

type ModelRegistry struct {
	auth         *AuthStorage
	modelsPath   string
	models       []types.Model
	providerCfg  map[string]ProviderConfig
	defaultModel map[string]string
}

func NewModelRegistry(auth *AuthStorage, modelsPath string) *ModelRegistry {
	if modelsPath == "" {
		modelsPath = GetModelsPath()
	}
	m := &ModelRegistry{
		auth:         auth,
		modelsPath:   modelsPath,
		providerCfg:  map[string]ProviderConfig{},
		defaultModel: defaultModelPerProvider(),
	}
	m.Refresh()
	if m.auth != nil {
		m.auth.SetFallbackResolver(func(provider string) string {
			if cfg, ok := m.providerCfg[provider]; ok {
				return cfg.APIKey
			}
			return ""
		})
	}
	return m
}

func (m *ModelRegistry) Refresh() {
	m.models = builtInModels(m.defaultModel)
	m.providerCfg = defaultProviderConfigs()

	cfg := m.readModelsConfig()
	for provider, p := range cfg.Providers {
		baseBefore := m.providerCfg[provider]
		base := m.providerCfg[provider]
		if p.BaseURL != "" {
			base.BaseURL = p.BaseURL
		}
		if p.API != "" {
			base.API = p.API
		}
		if len(p.Headers) > 0 {
			if base.Headers == nil {
				base.Headers = map[string]string{}
			}
			for k, v := range p.Headers {
				base.Headers[k] = v
			}
		}
		if p.APIKey != "" {
			base.APIKey = p.APIKey
		}
		m.providerCfg[provider] = base
		m.applyProviderOverridesToBuiltIns(provider, baseBefore, base, p)

		if len(p.Models) > 0 {
			m.upsertProviderModels(provider, p.Models, base)
		}
	}
}

func (m *ModelRegistry) applyProviderOverridesToBuiltIns(
	provider string,
	baseBefore ProviderConfig,
	baseAfter ProviderConfig,
	override ProviderConfig,
) {
	applyBaseURL := override.BaseURL != "" && baseAfter.BaseURL != baseBefore.BaseURL
	applyAPI := override.API != "" && baseAfter.API != baseBefore.API
	if !applyBaseURL && !applyAPI {
		return
	}
	for i := range m.models {
		if !strings.EqualFold(m.models[i].Provider, provider) {
			continue
		}
		if applyBaseURL {
			m.models[i].BaseURL = baseAfter.BaseURL
		}
		if applyAPI {
			m.models[i].API = baseAfter.API
		}
	}
}

func (m *ModelRegistry) readModelsConfig() modelsConfigFile {
	b, err := os.ReadFile(m.modelsPath)
	if err != nil {
		return modelsConfigFile{Providers: map[string]ProviderConfig{}}
	}
	var cfg modelsConfigFile
	if err := json.Unmarshal(b, &cfg); err != nil {
		return modelsConfigFile{Providers: map[string]ProviderConfig{}}
	}
	if cfg.Providers == nil {
		cfg.Providers = map[string]ProviderConfig{}
	}
	return cfg
}

func (m *ModelRegistry) upsertProviderModels(provider string, models []types.Model, cfg ProviderConfig) {
	index := map[string]int{}
	for i, model := range m.models {
		if strings.EqualFold(model.Provider, provider) {
			index[strings.ToLower(model.ID)] = i
		}
	}
	for _, model := range models {
		if model.Provider == "" {
			model.Provider = provider
		}
		if model.API == "" {
			model.API = cfg.API
		}
		if model.BaseURL == "" {
			model.BaseURL = cfg.BaseURL
		}
		if model.MaxTokens == 0 {
			model.MaxTokens = 4096
		}
		if model.ContextWindow == 0 {
			model.ContextWindow = 200000
		}
		if len(model.Input) == 0 {
			model.Input = []string{"text"}
		}
		if i, ok := index[strings.ToLower(model.ID)]; ok {
			m.models[i] = model
		} else {
			m.models = append(m.models, model)
		}
	}
}

func (m *ModelRegistry) GetAll() []types.Model {
	out := make([]types.Model, len(m.models))
	copy(out, m.models)
	return out
}

func (m *ModelRegistry) GetProviderConfig(provider string) ProviderConfig {
	return m.providerCfg[provider]
}

func (m *ModelRegistry) GetAPIKey(provider string) string {
	if m.auth == nil {
		return ""
	}
	return m.auth.GetAPIKey(provider)
}

func (m *ModelRegistry) GetAvailable() []types.Model {
	if m.auth == nil {
		return m.GetAll()
	}
	all := m.GetAll()
	out := make([]types.Model, 0, len(all))
	for _, model := range all {
		if m.auth.HasAuth(model.Provider) {
			out = append(out, model)
		}
	}
	return out
}

func (m *ModelRegistry) ResolveModel(provider, modelID string) (types.Model, error) {
	all := m.GetAll()
	if provider != "" && modelID == "" {
		if id, ok := m.defaultModel[provider]; ok {
			modelID = id
		}
	}
	if provider == "" && modelID == "" {
		provider = "anthropic"
		modelID = m.defaultModel[provider]
	}

	if provider != "" {
		for _, model := range all {
			if strings.EqualFold(model.Provider, provider) && strings.EqualFold(model.ID, modelID) {
				return model, nil
			}
		}
		if modelID == "" {
			for _, model := range all {
				if strings.EqualFold(model.Provider, provider) {
					return model, nil
				}
			}
		}
		return types.Model{}, fmt.Errorf("model not found for provider=%s model=%s", provider, modelID)
	}

	// provider empty, search globally
	for _, model := range all {
		if strings.EqualFold(model.ID, modelID) {
			return model, nil
		}
	}

	// Fuzzy fallback
	matches := make([]types.Model, 0)
	for _, model := range all {
		if strings.Contains(strings.ToLower(model.ID), strings.ToLower(modelID)) ||
			strings.Contains(strings.ToLower(model.Name), strings.ToLower(modelID)) {
			matches = append(matches, model)
		}
	}
	if len(matches) == 0 {
		return types.Model{}, fmt.Errorf("model not found: %s", modelID)
	}
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].ID < matches[j].ID
	})
	return matches[0], nil
}

func defaultModelPerProvider() map[string]string {
	return map[string]string{
		"amazon-bedrock":         "us.anthropic.claude-opus-4-6-v1",
		"anthropic":              "claude-opus-4-6",
		"openai":                 "gpt-5.1-codex",
		"azure-openai-responses": "gpt-5.2",
		"openai-codex":           "gpt-5.3-codex",
		"google":                 "gemini-2.5-pro",
		"google-gemini-cli":      "gemini-2.5-pro",
		"google-antigravity":     "gemini-3-pro-high",
		"google-vertex":          "gemini-3-pro-preview",
		"github-copilot":         "gpt-4o",
		"openrouter":             "openai/gpt-5.1-codex",
		"vercel-ai-gateway":      "anthropic/claude-opus-4-6",
		"xai":                    "grok-4-fast-non-reasoning",
		"groq":                   "openai/gpt-oss-120b",
		"cerebras":               "zai-glm-4.6",
		"zai":                    "glm-4.6",
		"mistral":                "devstral-medium-latest",
		"minimax":                "MiniMax-M2.1",
		"minimax-cn":             "MiniMax-M2.1",
		"huggingface":            "moonshotai/Kimi-K2.5",
		"opencode":               "claude-opus-4-6",
		"kimi-coding":            "kimi-k2-thinking",
	}
}

func defaultProviderConfigs() map[string]ProviderConfig {
	return map[string]ProviderConfig{
		"openai":                 {API: "openai-completions", BaseURL: "https://api.openai.com/v1"},
		"azure-openai-responses": {API: "azure-openai-responses", BaseURL: defaultAzureOpenAIBaseURL()},
		"openai-codex":           {API: "openai-codex-responses", BaseURL: "https://chatgpt.com/backend-api"},
		"anthropic":              {API: "anthropic-messages", BaseURL: "https://api.anthropic.com"},
		"google":                 {API: "google-generative-ai", BaseURL: "https://generativelanguage.googleapis.com"},
		"google-gemini-cli":      {API: "google-gemini-cli", BaseURL: "https://cloudcode-pa.googleapis.com"},
		"google-antigravity":     {API: "google-gemini-cli", BaseURL: "https://daily-cloudcode-pa.sandbox.googleapis.com"},
		"google-vertex":          {API: "google-vertex", BaseURL: "https://us-central1-aiplatform.googleapis.com"},
		"amazon-bedrock":         {API: "bedrock-converse-stream", BaseURL: ""},
		"github-copilot":         {API: "openai-responses", BaseURL: "https://api.githubcopilot.com"},
		"xai":                    {API: "openai-completions", BaseURL: "https://api.x.ai/v1"},
		"groq":                   {API: "openai-completions", BaseURL: "https://api.groq.com/openai/v1"},
		"cerebras":               {API: "openai-completions", BaseURL: "https://api.cerebras.ai/v1"},
		"openrouter":             {API: "openai-completions", BaseURL: "https://openrouter.ai/api/v1"},
		"vercel-ai-gateway":      {API: "openai-completions", BaseURL: "https://ai-gateway.vercel.sh/v1"},
		"zai":                    {API: "openai-completions", BaseURL: "https://api.z.ai/api/paas/v4"},
		"mistral":                {API: "openai-completions", BaseURL: "https://api.mistral.ai/v1"},
		"minimax":                {API: "openai-completions", BaseURL: "https://api.minimax.io/v1"},
		"minimax-cn":             {API: "openai-completions", BaseURL: "https://api.minimax.chat/v1"},
		"huggingface":            {API: "openai-completions", BaseURL: "https://router.huggingface.co/v1"},
		"opencode":               {API: "openai-completions", BaseURL: "https://api.opencode.ai/v1"},
		"kimi-coding":            {API: "openai-completions", BaseURL: "https://api.moonshot.ai/anthropic"},
	}
}

func defaultAzureOpenAIBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("AZURE_OPENAI_BASE_URL")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("AZURE_OPENAI_ENDPOINT")); v != "" {
		return v
	}
	if resource := strings.TrimSpace(os.Getenv("AZURE_OPENAI_RESOURCE_NAME")); resource != "" {
		return "https://" + resource + ".openai.azure.com/openai/v1"
	}
	return ""
}

func builtInModels(defaults map[string]string) []types.Model {
	cfg := defaultProviderConfigs()
	out := make([]types.Model, 0, len(defaults))
	for provider, modelID := range defaults {
		pc := cfg[provider]
		out = append(out, types.Model{
			ID:            modelID,
			Name:          modelID,
			API:           pc.API,
			Provider:      provider,
			BaseURL:       pc.BaseURL,
			Reasoning:     true,
			Input:         []string{"text"},
			ContextWindow: 200000,
			MaxTokens:     32768,
			Headers:       pc.Headers,
		})
	}
	return out
}
