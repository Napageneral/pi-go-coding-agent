package main

import "testing"

func TestParseCLIArgsKnownAndExtensionFlags(t *testing.T) {
	opts, err := parseCLIArgs([]string{
		"--provider", "openai",
		"--model", "gpt-test",
		"--json=false",
		"--continue",
		"--extension-sidecar-command", "node",
		"--extension-sidecar-arg", "sidecar/main.mjs",
		"--extension", "/tmp/ext.mjs",
		"--fixture-mode=transform",
		"--feature-flag",
		"hello",
		"world",
	})
	if err != nil {
		t.Fatalf("parseCLIArgs: %v", err)
	}
	if opts.Provider != "openai" || opts.Model != "gpt-test" {
		t.Fatalf("unexpected provider/model: %#v", opts)
	}
	if opts.JSONOut {
		t.Fatalf("expected jsonOut false from --json=false")
	}
	if !opts.ContinueOnly {
		t.Fatalf("expected continueOnly true")
	}
	if opts.ExtensionSidecarCommand != "node" {
		t.Fatalf("unexpected sidecar command: %q", opts.ExtensionSidecarCommand)
	}
	if len(opts.ExtensionSidecarArgs) != 1 || opts.ExtensionSidecarArgs[0] != "sidecar/main.mjs" {
		t.Fatalf("unexpected sidecar args: %#v", opts.ExtensionSidecarArgs)
	}
	if len(opts.ExtensionPaths) != 1 || opts.ExtensionPaths[0] != "/tmp/ext.mjs" {
		t.Fatalf("unexpected extension paths: %#v", opts.ExtensionPaths)
	}
	if got := opts.ExtensionFlagValues["fixture-mode"]; got != "transform" {
		t.Fatalf("fixture-mode value = %#v, want transform", got)
	}
	if got := opts.ExtensionFlagValues["feature-flag"]; got != true {
		t.Fatalf("feature-flag value = %#v, want true", got)
	}
	if len(opts.MessageParts) != 2 || opts.MessageParts[0] != "hello" || opts.MessageParts[1] != "world" {
		t.Fatalf("unexpected message parts: %#v", opts.MessageParts)
	}
}

func TestParseCLIArgsMissingValue(t *testing.T) {
	_, err := parseCLIArgs([]string{"--provider"})
	if err == nil {
		t.Fatal("expected missing value error")
	}
}
