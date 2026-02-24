package providers

import (
	"fmt"
	"strings"

	"github.com/badlogic/pi-mono/go-coding-agent/pkg/config"
	"github.com/badlogic/pi-mono/go-coding-agent/pkg/types"
)

func BuildProvider(model types.Model, providerCfg config.ProviderConfig, apiKey string) (types.Provider, error) {
	api := strings.ToLower(model.API)
	switch api {
	case "openai-completions", "openai-responses", "openai-codex-responses", "azure-openai-responses":
		return NewOpenAICompatibleProvider(model, providerCfg, apiKey), nil
	case "anthropic-messages":
		return NewAnthropicProvider(model, providerCfg, apiKey), nil
	case "google-generative-ai", "google-gemini-cli", "google-vertex":
		return NewGoogleProvider(model, providerCfg, apiKey), nil
	case "bedrock-converse-stream":
		return NewBedrockProvider(model, providerCfg, apiKey), nil
	default:
		return nil, fmt.Errorf("unsupported api: %s", model.API)
	}
}

func mergeHeaders(modelHeaders map[string]string, reqHeaders map[string]string, cfgHeaders map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range cfgHeaders {
		out[k] = v
	}
	for k, v := range modelHeaders {
		out[k] = v
	}
	for k, v := range reqHeaders {
		out[k] = v
	}
	return out
}

func messageText(m types.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type == "text" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c.Text)
		} else if c.Type == "thinking" {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(c.Thinking)
		}
	}
	return b.String()
}
