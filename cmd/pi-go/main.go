package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/agent"
)

func main() {
	opts, err := parseCLIArgs(os.Args[1:])
	if err != nil {
		die(err)
	}

	rt, err := agent.NewRuntime(agent.NewRuntimeOptions{
		CWD:                     opts.CWD,
		SessionDir:              opts.SessionDir,
		Session:                 opts.SessionPath,
		NoSession:               opts.NoSession,
		Provider:                opts.Provider,
		Model:                   opts.Model,
		APIKey:                  opts.APIKey,
		SystemPrompt:            opts.SystemPrompt,
		ExtensionSidecarCommand: strings.TrimSpace(opts.ExtensionSidecarCommand),
		ExtensionSidecarArgs:    opts.ExtensionSidecarArgs,
		ExtensionPaths:          opts.ExtensionPaths,
		ExtensionFlagValues:     opts.ExtensionFlagValues,
	})
	if err != nil {
		die(err)
	}
	defer func() {
		_ = rt.Close()
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		rt.Abort()
	}()

	msg := strings.TrimSpace(strings.Join(opts.MessageParts, " "))
	if msg == "" && !opts.ContinueOnly {
		// If nothing in args, read stdin.
		if st, err := os.Stdin.Stat(); err == nil && (st.Mode()&os.ModeCharDevice) == 0 {
			b, _ := io.ReadAll(os.Stdin)
			msg = strings.TrimSpace(string(b))
		}
	}

	if msg == "" {
		if !opts.ContinueOnly {
			die(fmt.Errorf("no message provided; pass a prompt or use --continue"))
		}
		assistant, err := rt.Continue()
		if err != nil {
			if errors.Is(err, agent.ErrAborted) {
				os.Exit(130)
			}
			die(err)
		}
		if opts.JSONOut {
			printJSON(assistant)
		} else {
			fmt.Println(agent.AssistantText(assistant))
		}
		return
	}

	assistant, err := rt.Prompt(msg)
	if err != nil {
		if errors.Is(err, agent.ErrAborted) {
			os.Exit(130)
		}
		die(err)
	}
	if opts.JSONOut {
		printJSON(assistant)
	} else {
		fmt.Println(agent.AssistantText(assistant))
	}
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func die(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}

type cliOptions struct {
	Provider                string
	Model                   string
	APIKey                  string
	SessionPath             string
	SessionDir              string
	NoSession               bool
	CWD                     string
	JSONOut                 bool
	ContinueOnly            bool
	SystemPrompt            string
	ExtensionSidecarCommand string
	ExtensionSidecarArgs    []string
	ExtensionPaths          []string
	ExtensionFlagValues     map[string]any
	MessageParts            []string
}

func parseCLIArgs(args []string) (cliOptions, error) {
	opts := cliOptions{
		CWD:                 ".",
		ExtensionFlagValues: map[string]any{},
	}

	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			opts.MessageParts = append(opts.MessageParts, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "--") {
			opts.MessageParts = append(opts.MessageParts, arg)
			continue
		}

		name, inlineValue, hasInline := splitLongFlag(arg)
		switch name {
		case "provider":
			v, consumed, err := parseRequiredValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.Provider = v
			i += consumed
		case "model":
			v, consumed, err := parseRequiredValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.Model = v
			i += consumed
		case "api-key":
			v, consumed, err := parseRequiredValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.APIKey = v
			i += consumed
		case "session":
			v, consumed, err := parseRequiredValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.SessionPath = v
			i += consumed
		case "session-dir":
			v, consumed, err := parseRequiredValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.SessionDir = v
			i += consumed
		case "cwd":
			v, consumed, err := parseRequiredValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.CWD = v
			i += consumed
		case "system-prompt":
			v, consumed, err := parseRequiredValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.SystemPrompt = v
			i += consumed
		case "extension-sidecar-command":
			v, consumed, err := parseRequiredValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.ExtensionSidecarCommand = v
			i += consumed
		case "extension-sidecar-arg":
			v, consumed, err := parseRequiredValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.ExtensionSidecarArgs = append(opts.ExtensionSidecarArgs, v)
			i += consumed
		case "extension":
			v, consumed, err := parseRequiredValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.ExtensionPaths = append(opts.ExtensionPaths, v)
			i += consumed
		case "no-session":
			b, consumed, err := parseOptionalBool(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.NoSession = b
			i += consumed
		case "json":
			b, consumed, err := parseOptionalBool(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.JSONOut = b
			i += consumed
		case "continue":
			b, consumed, err := parseOptionalBool(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.ContinueOnly = b
			i += consumed
		default:
			value, consumed, err := parseExtensionFlagValue(args, i, inlineValue, hasInline)
			if err != nil {
				return opts, err
			}
			opts.ExtensionFlagValues[name] = value
			i += consumed
		}
	}

	return opts, nil
}

func splitLongFlag(arg string) (name, value string, hasValue bool) {
	raw := strings.TrimPrefix(arg, "--")
	if idx := strings.IndexByte(raw, '='); idx >= 0 {
		return raw[:idx], raw[idx+1:], true
	}
	return raw, "", false
}

func parseRequiredValue(args []string, index int, inlineValue string, hasInline bool) (string, int, error) {
	if hasInline {
		return inlineValue, 0, nil
	}
	if index+1 >= len(args) {
		return "", 0, fmt.Errorf("missing value for %s", args[index])
	}
	next := args[index+1]
	if strings.HasPrefix(next, "--") {
		return "", 0, fmt.Errorf("missing value for %s", args[index])
	}
	return next, 1, nil
}

func parseOptionalBool(args []string, index int, inlineValue string, hasInline bool) (bool, int, error) {
	if hasInline {
		v, err := strconv.ParseBool(inlineValue)
		if err != nil {
			return false, 0, fmt.Errorf("invalid boolean for %s: %q", args[index], inlineValue)
		}
		return v, 0, nil
	}
	if index+1 < len(args) && isBoolToken(args[index+1]) {
		v, _ := strconv.ParseBool(args[index+1])
		return v, 1, nil
	}
	return true, 0, nil
}

func parseExtensionFlagValue(args []string, index int, inlineValue string, hasInline bool) (any, int, error) {
	if hasInline {
		return normalizeExtFlagValue(inlineValue), 0, nil
	}
	return true, 0, nil
}

func normalizeExtFlagValue(v string) any {
	if b, err := strconv.ParseBool(v); err == nil {
		return b
	}
	return v
}

func isBoolToken(v string) bool {
	_, err := strconv.ParseBool(v)
	return err == nil
}
