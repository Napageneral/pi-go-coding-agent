package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOAuthGoogleGeminiCliAPIKeyShape(t *testing.T) {
	tmp := t.TempDir()
	authPath := filepath.Join(tmp, "auth.json")
	err := os.WriteFile(authPath, []byte(`{
		"google-gemini-cli": {
			"type": "oauth",
			"access": "tok_123",
			"projectId": "proj_abc"
		}
	}`), 0o600)
	if err != nil {
		t.Fatalf("write auth file failed: %v", err)
	}

	a := NewAuthStorage(authPath)
	if !a.HasAuth("google-gemini-cli") {
		t.Fatal("expected HasAuth for google-gemini-cli")
	}
	raw := a.GetAPIKey("google-gemini-cli")
	var parsed map[string]string
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("expected JSON API key shape, got %q: %v", raw, err)
	}
	if parsed["token"] != "tok_123" || parsed["projectId"] != "proj_abc" {
		t.Fatalf("unexpected parsed oauth key payload: %#v", parsed)
	}
}

func TestOAuthAccessFieldFallback(t *testing.T) {
	tmp := t.TempDir()
	authPath := filepath.Join(tmp, "auth.json")
	err := os.WriteFile(authPath, []byte(`{
		"anthropic": {
			"type": "oauth",
			"access": "anthropic_oauth_token"
		}
	}`), 0o600)
	if err != nil {
		t.Fatalf("write auth file failed: %v", err)
	}
	a := NewAuthStorage(authPath)
	if got := a.GetAPIKey("anthropic"); got != "anthropic_oauth_token" {
		t.Fatalf("expected anthropic oauth access token, got %q", got)
	}
}

func TestOpenAICodexRefreshWhenExpired(t *testing.T) {
	tmp := t.TempDir()
	authPath := filepath.Join(tmp, "auth.json")
	err := os.WriteFile(authPath, []byte(`{
		"openai-codex": {
			"type": "oauth",
			"access": "expired_access",
			"refresh": "refresh_123",
			"expires": 1
		}
	}`), 0o600)
	if err != nil {
		t.Fatalf("write auth file failed: %v", err)
	}

	origURL := openAICodexTokenURL
	defer func() { openAICodexTokenURL = origURL }()

	sawRequest := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if r.Method != http.MethodPost {
			t.Fatalf("expected POST, got %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("unexpected content-type: %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form failed: %v", err)
		}
		assertFormValue(t, r.Form, "grant_type", "refresh_token")
		assertFormValue(t, r.Form, "refresh_token", "refresh_123")
		assertFormValue(t, r.Form, "client_id", "app_EMoamEEZ73f0CkXaXp7hrann")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"access_token":"new_access_token",
			"refresh_token":"new_refresh_token",
			"expires_in":3600
		}`))
	}))
	defer srv.Close()

	openAICodexTokenURL = srv.URL

	a := NewAuthStorage(authPath)
	got := a.GetAPIKey("openai-codex")
	if got != "new_access_token" {
		t.Fatalf("expected refreshed token, got %q", got)
	}
	if !sawRequest {
		t.Fatal("expected refresh request")
	}

	updatedRaw, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read updated auth file failed: %v", err)
	}
	var updated map[string]authCredential
	if err := json.Unmarshal(updatedRaw, &updated); err != nil {
		t.Fatalf("unmarshal updated auth failed: %v", err)
	}
	cred := updated["openai-codex"]
	if cred.Access != "new_access_token" {
		t.Fatalf("expected updated access token, got %q", cred.Access)
	}
	if cred.Refresh != "new_refresh_token" {
		t.Fatalf("expected updated refresh token, got %q", cred.Refresh)
	}
	if cred.Expires <= time.Now().UnixMilli() {
		t.Fatalf("expected future expires, got %d", cred.Expires)
	}
}

func assertFormValue(t *testing.T, form url.Values, key, want string) {
	t.Helper()
	if got := form.Get(key); got != want {
		t.Fatalf("expected %s=%q, got %q", key, want, got)
	}
}
