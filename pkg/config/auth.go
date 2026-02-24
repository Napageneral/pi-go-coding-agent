package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type authCredential struct {
	Type string `json:"type"`
	Key  string `json:"key,omitempty"`
	// OAuth fields: older files may use accessToken, current files use access.
	AccessToken string `json:"accessToken,omitempty"`
	Access      string `json:"access,omitempty"`
	Refresh     string `json:"refresh,omitempty"`
	Expires     int64  `json:"expires,omitempty"`
	ProjectID   string `json:"projectId,omitempty"`
	// GitHub Copilot enterprise domain (if configured).
	EnterpriseURL string `json:"enterpriseUrl,omitempty"`
}

type AuthStorage struct {
	path      string
	data      map[string]authCredential
	overrides map[string]string
	fallback  func(provider string) string
}

var (
	openAICodexTokenURL = "https://auth.openai.com/oauth/token"
	anthropicTokenURL   = "https://console.anthropic.com/v1/oauth/token"
	googleTokenURL      = "https://oauth2.googleapis.com/token"
)

var oauthHTTPClient = &http.Client{Timeout: 20 * time.Second}

func NewAuthStorage(path string) *AuthStorage {
	if path == "" {
		path = GetAuthPath()
	}
	a := &AuthStorage{
		path:      path,
		data:      map[string]authCredential{},
		overrides: map[string]string{},
	}
	a.reload()
	return a
}

func (a *AuthStorage) reload() {
	b, err := os.ReadFile(a.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &a.data)
}

func (a *AuthStorage) SetRuntimeAPIKey(provider, key string) {
	a.overrides[provider] = key
}

func (a *AuthStorage) persist() {
	b, err := json.MarshalIndent(a.data, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(a.path, b, 0o600)
}

func (a *AuthStorage) SetFallbackResolver(fn func(provider string) string) {
	a.fallback = fn
}

func (a *AuthStorage) HasAuth(provider string) bool {
	if _, ok := a.overrides[provider]; ok {
		return true
	}
	if c, ok := a.data[provider]; ok {
		if c.Type == "api_key" && c.Key != "" {
			return true
		}
		if c.Type == "oauth" && (c.AccessToken != "" || c.Access != "") {
			return true
		}
	}
	if env := envAPIKey(provider); env != "" {
		return true
	}
	if a.fallback != nil && a.fallback(provider) != "" {
		return true
	}
	return false
}

func (a *AuthStorage) GetAPIKey(provider string) string {
	if v, ok := a.overrides[provider]; ok && v != "" {
		return v
	}
	if c, ok := a.data[provider]; ok {
		if c.Type == "api_key" && c.Key != "" {
			return resolveConfigValue(c.Key)
		}
		if c.Type == "oauth" {
			if updated, ok := refreshOAuthCredentialIfNeeded(provider, c); ok {
				c = updated
				a.data[provider] = c
				a.persist()
			}
			token := c.AccessToken
			if token == "" {
				token = c.Access
			}
			if token != "" {
				// google-gemini-cli/google-antigravity API expects JSON-encoded token+projectId.
				if (provider == "google-gemini-cli" || provider == "google-antigravity") && c.ProjectID != "" {
					b, _ := json.Marshal(map[string]string{
						"token":     token,
						"projectId": c.ProjectID,
					})
					return string(b)
				}
				return token
			}
		}
	}
	if env := envAPIKey(provider); env != "" {
		return env
	}
	if a.fallback != nil {
		return resolveConfigValue(a.fallback(provider))
	}
	return ""
}

func resolveConfigValue(v string) string {
	if strings.HasPrefix(v, "$") {
		return os.Getenv(strings.TrimPrefix(v, "$"))
	}
	return v
}

func refreshOAuthCredentialIfNeeded(provider string, cred authCredential) (authCredential, bool) {
	if strings.TrimSpace(cred.Refresh) == "" {
		return cred, false
	}
	token := strings.TrimSpace(cred.AccessToken)
	if token == "" {
		token = strings.TrimSpace(cred.Access)
	}
	nowMillis := time.Now().UnixMilli()
	needsRefresh := token == "" || (cred.Expires > 0 && nowMillis >= cred.Expires)
	if !needsRefresh {
		return cred, false
	}

	refreshed, err := refreshOAuthCredential(provider, cred)
	if err != nil {
		return cred, false
	}
	if refreshed.Access != "" && refreshed.AccessToken == "" {
		refreshed.AccessToken = refreshed.Access
	}
	if refreshed.AccessToken != "" && refreshed.Access == "" {
		refreshed.Access = refreshed.AccessToken
	}
	if refreshed.Refresh == "" {
		refreshed.Refresh = cred.Refresh
	}
	return refreshed, true
}

func refreshOAuthCredential(provider string, cred authCredential) (authCredential, error) {
	switch provider {
	case "openai-codex":
		return refreshOpenAICodexCredential(cred)
	case "anthropic":
		return refreshAnthropicCredential(cred)
	case "google-gemini-cli":
		return refreshGoogleCredential(
			cred,
			strings.TrimSpace(os.Getenv("GOOGLE_GEMINI_CLI_CLIENT_ID")),
			strings.TrimSpace(os.Getenv("GOOGLE_GEMINI_CLI_CLIENT_SECRET")),
		)
	case "google-antigravity":
		return refreshGoogleCredential(
			cred,
			strings.TrimSpace(os.Getenv("GOOGLE_ANTIGRAVITY_CLIENT_ID")),
			strings.TrimSpace(os.Getenv("GOOGLE_ANTIGRAVITY_CLIENT_SECRET")),
		)
	case "github-copilot":
		return refreshGitHubCopilotCredential(cred)
	default:
		return cred, fmt.Errorf("oauth refresh unsupported for provider %s", provider)
	}
}

func refreshOpenAICodexCredential(cred authCredential) (authCredential, error) {
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", cred.Refresh)
	form.Set("client_id", "app_EMoamEEZ73f0CkXaXp7hrann")

	req, _ := http.NewRequest(http.MethodPost, openAICodexTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return cred, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return cred, fmt.Errorf("openai-codex token refresh failed: %s", strings.TrimSpace(string(body)))
	}

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return cred, err
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return cred, fmt.Errorf("openai-codex token refresh response missing fields")
	}

	cred.Access = out.AccessToken
	cred.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		cred.Refresh = out.RefreshToken
	}
	cred.Expires = time.Now().UnixMilli() + out.ExpiresIn*1000 - 5*60*1000
	return cred, nil
}

func refreshAnthropicCredential(cred authCredential) (authCredential, error) {
	body := map[string]any{
		"grant_type":    "refresh_token",
		"client_id":     "9d1c250a-e61b-44d9-88ed-5944d1962f5e",
		"refresh_token": cred.Refresh,
	}
	b, _ := json.Marshal(body)

	req, _ := http.NewRequest(http.MethodPost, anthropicTokenURL, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return cred, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return cred, fmt.Errorf("anthropic token refresh failed: %s", strings.TrimSpace(string(respBody)))
	}

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return cred, err
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return cred, fmt.Errorf("anthropic token refresh response missing fields")
	}

	cred.Access = out.AccessToken
	cred.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		cred.Refresh = out.RefreshToken
	}
	cred.Expires = time.Now().UnixMilli() + out.ExpiresIn*1000 - 5*60*1000
	return cred, nil
}

func refreshGoogleCredential(cred authCredential, clientID, clientSecret string) (authCredential, error) {
	if clientID == "" || clientSecret == "" {
		return cred, fmt.Errorf("google oauth refresh requires client id/secret env vars")
	}

	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("refresh_token", cred.Refresh)
	form.Set("grant_type", "refresh_token")

	req, _ := http.NewRequest(http.MethodPost, googleTokenURL, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return cred, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return cred, fmt.Errorf("google token refresh failed: %s", strings.TrimSpace(string(body)))
	}

	var out struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return cred, err
	}
	if out.AccessToken == "" || out.ExpiresIn <= 0 {
		return cred, fmt.Errorf("google token refresh response missing fields")
	}

	cred.Access = out.AccessToken
	cred.AccessToken = out.AccessToken
	if out.RefreshToken != "" {
		cred.Refresh = out.RefreshToken
	}
	cred.Expires = time.Now().UnixMilli() + out.ExpiresIn*1000 - 5*60*1000
	return cred, nil
}

func refreshGitHubCopilotCredential(cred authCredential) (authCredential, error) {
	domain := normalizeCopilotDomain(cred.EnterpriseURL)
	if domain == "" {
		domain = "github.com"
	}
	endpoint := fmt.Sprintf("https://api.%s/copilot_internal/v2/token", domain)

	req, _ := http.NewRequest(http.MethodGet, endpoint, nil)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+cred.Refresh)
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.35.0")
	req.Header.Set("Editor-Version", "vscode/1.107.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.35.0")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")

	resp, err := oauthHTTPClient.Do(req)
	if err != nil {
		return cred, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return cred, fmt.Errorf("github-copilot token refresh failed: %s", strings.TrimSpace(string(body)))
	}

	var out struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return cred, err
	}
	if out.Token == "" || out.ExpiresAt <= 0 {
		return cred, fmt.Errorf("github-copilot token refresh response missing fields")
	}

	cred.Access = out.Token
	cred.AccessToken = out.Token
	cred.Expires = out.ExpiresAt*1000 - 5*60*1000
	return cred, nil
}

func normalizeCopilotDomain(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return ""
	}
	if strings.Contains(v, "://") {
		u, err := url.Parse(v)
		if err == nil {
			return strings.TrimSpace(u.Hostname())
		}
	}
	u, err := url.Parse("https://" + v)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Hostname())
}

func envAPIKey(provider string) string {
	if provider == "anthropic" {
		if v := os.Getenv("ANTHROPIC_OAUTH_TOKEN"); v != "" {
			return v
		}
		return os.Getenv("ANTHROPIC_API_KEY")
	}
	if provider == "github-copilot" {
		if v := os.Getenv("COPILOT_GITHUB_TOKEN"); v != "" {
			return v
		}
		if v := os.Getenv("GH_TOKEN"); v != "" {
			return v
		}
		return os.Getenv("GITHUB_TOKEN")
	}

	envMap := map[string]string{
		"openai":                 "OPENAI_API_KEY",
		"azure-openai-responses": "AZURE_OPENAI_API_KEY",
		"google":                 "GEMINI_API_KEY",
		"google-gemini-cli":      "GOOGLE_API_KEY",
		"google-antigravity":     "GOOGLE_API_KEY",
		"google-vertex":          "GOOGLE_VERTEX_ACCESS_TOKEN",
		"groq":                   "GROQ_API_KEY",
		"cerebras":               "CEREBRAS_API_KEY",
		"xai":                    "XAI_API_KEY",
		"openrouter":             "OPENROUTER_API_KEY",
		"vercel-ai-gateway":      "AI_GATEWAY_API_KEY",
		"zai":                    "ZAI_API_KEY",
		"mistral":                "MISTRAL_API_KEY",
		"minimax":                "MINIMAX_API_KEY",
		"minimax-cn":             "MINIMAX_CN_API_KEY",
		"huggingface":            "HF_TOKEN",
		"opencode":               "OPENCODE_API_KEY",
		"kimi-coding":            "KIMI_API_KEY",
		"openai-codex":           "OPENAI_API_KEY",
	}

	if envVar, ok := envMap[provider]; ok {
		return os.Getenv(envVar)
	}
	return ""
}
