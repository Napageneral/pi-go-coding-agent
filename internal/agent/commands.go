package agent

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/badlogic/pi-mono/go-coding-agent/internal/config"
	"github.com/badlogic/pi-mono/go-coding-agent/internal/extensionsidecar"
)

func (r *Runtime) ExtensionCommands() []extensionsidecar.ExtensionCommandDefinition {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]extensionsidecar.ExtensionCommandDefinition, len(r.extensionCommands))
	copy(out, r.extensionCommands)
	return out
}

func (r *Runtime) Commands() []Command {
	commands := make([]Command, 0, 16)

	for _, command := range r.ExtensionCommands() {
		name := strings.TrimSpace(command.Name)
		if name == "" {
			continue
		}
		commands = append(commands, Command{
			Name:        name,
			Description: strings.TrimSpace(command.Description),
			Source:      "extension",
			Path:        strings.TrimSpace(command.Path),
		})
	}

	agentDir := config.GetAgentDir()
	cwd := strings.TrimSpace(r.session.CWD())
	if cwd == "" {
		cwd = "."
	}
	commands = append(commands, loadPromptCommands(filepath.Join(agentDir, "prompts"), "user")...)
	commands = append(commands, loadPromptCommands(filepath.Join(cwd, config.ConfigDirName, "prompts"), "project")...)
	commands = append(commands, loadSkillCommands(filepath.Join(agentDir, "skills"), "user")...)
	commands = append(commands, loadSkillCommands(filepath.Join(cwd, config.ConfigDirName, "skills"), "project")...)
	return commands
}

func loadPromptCommands(dir string, location string) []Command {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	commands := make([]Command, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		description := readMarkdownDescription(path)
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		commands = append(commands, Command{
			Name:        name,
			Description: description,
			Source:      "prompt",
			Location:    location,
			Path:        absPathOrOriginal(path),
		})
	}
	return commands
}

func loadSkillCommands(dir string, location string) []Command {
	if _, err := os.Stat(dir); err != nil {
		return nil
	}
	return loadSkillCommandsRecursive(dir, location, true)
}

func loadSkillCommandsRecursive(dir string, location string, includeRootMarkdown bool) []Command {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	commands := make([]Command, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			commands = append(commands, loadSkillCommandsRecursive(path, location, false)...)
			continue
		}
		name := entry.Name()
		isSkillMarkdown := (!includeRootMarkdown && name == "SKILL.md") ||
			(includeRootMarkdown && strings.HasSuffix(strings.ToLower(name), ".md"))
		if !isSkillMarkdown {
			continue
		}
		frontmatterName, description := readMarkdownMetadata(path)
		skillName := strings.TrimSpace(frontmatterName)
		if skillName == "" {
			if name == "SKILL.md" {
				skillName = filepath.Base(filepath.Dir(path))
			} else {
				skillName = strings.TrimSpace(strings.TrimSuffix(name, filepath.Ext(name)))
			}
		}
		if skillName == "" {
			continue
		}
		commands = append(commands, Command{
			Name:        "skill:" + skillName,
			Description: description,
			Source:      "skill",
			Location:    location,
			Path:        absPathOrOriginal(path),
		})
	}
	return commands
}

func readMarkdownDescription(path string) string {
	_, description := readMarkdownMetadata(path)
	return description
}

func readMarkdownMetadata(path string) (name string, description string) {
	contentBytes, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	content := string(contentBytes)
	meta, body := parseFrontmatter(content)
	name = strings.TrimSpace(meta["name"])
	description = strings.TrimSpace(meta["description"])
	if description != "" {
		return name, description
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return name, truncate(line, 120)
	}
	return name, ""
}

func parseFrontmatter(content string) (map[string]string, string) {
	meta := map[string]string{}
	content = strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(content, "---\n") {
		return meta, content
	}
	remaining := content[len("---\n"):]
	endIndex := strings.Index(remaining, "\n---\n")
	if endIndex < 0 {
		return meta, content
	}
	frontmatter := remaining[:endIndex]
	body := remaining[endIndex+len("\n---\n"):]
	for _, line := range strings.Split(frontmatter, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		value = strings.Trim(value, `"'`)
		if key != "" {
			meta[strings.ToLower(key)] = value
		}
	}
	return meta, body
}

func truncate(value string, max int) string {
	if max <= 0 || len(value) <= max {
		return value
	}
	return strings.TrimSpace(value[:max])
}

func absPathOrOriginal(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return abs
}
