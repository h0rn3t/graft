package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()
	projects := t.TempDir()
	project := filepath.Join(projects, "-work-repo")
	if err := os.MkdirAll(project, 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v, want nil", err)
	}
	session := `{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":7},"content":[]}}` + "\n"
	if err := os.WriteFile(filepath.Join(project, "s1.jsonl"), []byte(session), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v, want nil", err)
	}
	tests := []struct {
		name       string
		args       []string
		wantStatus int
		wantOut    string
	}{
		{"terminal report", []string{"--projects", projects, "/work/repo"}, 0, "main: 1 conversation, 1 round, 0 compactions"},
		{"JSON report", []string{"--projects", projects, "--json", "/work/repo"}, 0, `"inputTokens": 7`},
		{"a day after the session", []string{"--projects", projects, "--since", "2999-01-01", "/work/repo"}, 0, "no conversations"},
		{"two repositories", []string{"--projects", projects, "/work/repo", "/work/other"}, 0, "/work/other\n  no conversations"},
		{"not a day", []string{"--projects", projects, "--since", "yesterday", "/work/repo"}, 2, ""},
		{"no projects directory", []string{"--projects", filepath.Join(projects, "absent"), "/work/repo"}, 2, ""},
		{"unknown flag", []string{"--verbose"}, 2, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var stdout, stderr bytes.Buffer
			status := run(tt.args, &stdout, &stderr)
			if status != tt.wantStatus || !strings.Contains(stdout.String(), tt.wantOut) {
				t.Errorf("run(%q) = %d with stdout %q, want %d and %q", tt.args, status, stdout.String(), tt.wantStatus, tt.wantOut)
			}
			if tt.wantStatus != 0 && stderr.Len() == 0 {
				t.Errorf("run(%q) wrote nothing to stderr, want the reason", tt.args)
			}
		})
	}
}
