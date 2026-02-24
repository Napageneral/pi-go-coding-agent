package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGetSessionsDirForCWD(t *testing.T) {
	base := GetSessionsDir()
	got := GetSessionsDirForCWD("/Users/tyler/work/test")
	if !strings.HasPrefix(got, base) {
		t.Fatalf("expected sessions dir %q to be under %q", got, base)
	}
	wantSuffix := filepath.Join("--Users-tyler-work-test--")
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("expected encoded suffix %q, got %q", wantSuffix, got)
	}
}
