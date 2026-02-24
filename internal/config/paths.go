package config

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	AppName       = "pi"
	ConfigDirName = ".pi"
)

func agentEnvVar() string {
	return "PI_CODING_AGENT_DIR"
}

func expandHome(path string) string {
	if path == "~" {
		h, _ := os.UserHomeDir()
		return h
	}
	if len(path) >= 2 && path[:2] == "~/" {
		h, _ := os.UserHomeDir()
		return filepath.Join(h, path[2:])
	}
	return path
}

func GetAgentDir() string {
	if v := os.Getenv(agentEnvVar()); v != "" {
		return expandHome(v)
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ConfigDirName, "agent")
}

func GetAuthPath() string {
	return filepath.Join(GetAgentDir(), "auth.json")
}

func GetModelsPath() string {
	return filepath.Join(GetAgentDir(), "models.json")
}

func GetSettingsPath() string {
	return filepath.Join(GetAgentDir(), "settings.json")
}

func GetSessionsDir() string {
	return filepath.Join(GetAgentDir(), "sessions")
}

// GetSessionsDirForCWD mirrors coding-agent behavior by isolating sessions per cwd.
func GetSessionsDirForCWD(cwd string) string {
	safe := cwd
	safe = strings.TrimLeft(safe, `/\`)
	safe = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(safe)
	if safe == "" {
		safe = "root"
	}
	return filepath.Join(GetSessionsDir(), "--"+safe+"--")
}
