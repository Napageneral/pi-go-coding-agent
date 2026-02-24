package providers

import (
	"os"
	"testing"
)

func TestParseCloudCodeAssistAuthJSON(t *testing.T) {
	token, project, err := parseCloudCodeAssistAuth(`{"token":"tok","projectId":"proj"}`)
	if err != nil {
		t.Fatalf("parseCloudCodeAssistAuth returned error: %v", err)
	}
	if token != "tok" || project != "proj" {
		t.Fatalf("unexpected token/project: %q %q", token, project)
	}
}

func TestParseCloudCodeAssistAuthRawWithEnvProject(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_PROJECT", "env-proj")
	token, project, err := parseCloudCodeAssistAuth("rawtok")
	if err != nil {
		t.Fatalf("parseCloudCodeAssistAuth returned error: %v", err)
	}
	if token != "rawtok" || project != "env-proj" {
		t.Fatalf("unexpected token/project: %q %q", token, project)
	}
}

func TestParseCloudCodeAssistAuthMissingProject(t *testing.T) {
	_ = os.Unsetenv("GOOGLE_CLOUD_PROJECT")
	_ = os.Unsetenv("GCLOUD_PROJECT")
	_, _, err := parseCloudCodeAssistAuth("rawtok")
	if err == nil {
		t.Fatal("expected error when project id is missing")
	}
}

func TestMapGoogleStopReason(t *testing.T) {
	if got := mapGoogleStopReason("STOP", false); got != "stop" {
		t.Fatalf("expected stop, got %q", got)
	}
	if got := mapGoogleStopReason("MAX_TOKENS", false); got != "length" {
		t.Fatalf("expected length, got %q", got)
	}
	if got := mapGoogleStopReason("OTHER", true); got != "toolUse" {
		t.Fatalf("expected toolUse when tool calls are present, got %q", got)
	}
}
