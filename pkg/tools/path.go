package tools

import (
	"os"
	"path/filepath"
	"strings"
)

func resolvePath(cwd, p string) (string, error) {
	p = normalizePathInput(p)
	if p == "" {
		p = "."
	}

	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		if p == "~" {
			return home, nil
		}
		p = filepath.Join(home, p[2:])
	}

	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}

	abs, err := filepath.Abs(filepath.Join(cwd, p))
	if err != nil {
		return "", err
	}
	return abs, nil
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

func normalizePathInput(p string) string {
	if strings.HasPrefix(p, "@") {
		return p[1:]
	}
	return p
}
