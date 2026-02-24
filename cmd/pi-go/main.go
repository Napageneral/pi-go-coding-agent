package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/agent"
)

func main() {
	provider := flag.String("provider", "", "Model provider")
	model := flag.String("model", "", "Model ID")
	apiKey := flag.String("api-key", "", "Provider API key")
	sessionPath := flag.String("session", "", "Session file path")
	sessionDir := flag.String("session-dir", "", "Session directory")
	noSession := flag.Bool("no-session", false, "Do not reuse existing session")
	cwd := flag.String("cwd", ".", "Working directory")
	jsonOut := flag.Bool("json", false, "Print JSON output")
	continueOnly := flag.Bool("continue", false, "Continue current session without adding a user message")
	systemPrompt := flag.String("system-prompt", "", "Override system prompt")
	flag.Parse()

	rt, err := agent.NewRuntime(agent.NewRuntimeOptions{
		CWD:          *cwd,
		SessionDir:   *sessionDir,
		Session:      *sessionPath,
		NoSession:    *noSession,
		Provider:     *provider,
		Model:        *model,
		APIKey:       *apiKey,
		SystemPrompt: *systemPrompt,
	})
	if err != nil {
		die(err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		rt.Abort()
	}()

	msg := strings.TrimSpace(strings.Join(flag.Args(), " "))
	if msg == "" && !*continueOnly {
		// If nothing in args, read stdin.
		if st, err := os.Stdin.Stat(); err == nil && (st.Mode()&os.ModeCharDevice) == 0 {
			b, _ := io.ReadAll(os.Stdin)
			msg = strings.TrimSpace(string(b))
		}
	}

	if msg == "" {
		if !*continueOnly {
			die(fmt.Errorf("no message provided; pass a prompt or use --continue"))
		}
		assistant, err := rt.Continue()
		if err != nil {
			if errors.Is(err, agent.ErrAborted) {
				os.Exit(130)
			}
			die(err)
		}
		if *jsonOut {
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
	if *jsonOut {
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
