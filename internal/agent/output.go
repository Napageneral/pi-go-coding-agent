package agent

import "github.com/badlogic/pi-mono/go-coding-agent/internal/types"

func AssistantText(msg types.Message) string {
	out := ""
	for _, c := range msg.Content {
		if c.Type == "text" {
			if out != "" {
				out += "\n"
			}
			out += c.Text
		}
	}
	return out
}
