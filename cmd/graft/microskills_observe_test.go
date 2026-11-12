package main

import (
	jsonv2 "encoding/json/v2"
	"maps"
	"os"
	"path/filepath"
	"testing"
)

func TestParseShellCommand(t *testing.T) {
	tests := []struct {
		command    string
		wantIntent string
		wantForm   string
		wantDir    string
		wantEnv    map[string]string
	}{
		{command: "go test ./...", wantIntent: "go test", wantForm: "go test …"},
		{command: ".venv/bin/ruff format .", wantIntent: "ruff format", wantForm: ".venv/bin/ruff format …"},
		{command: "cd web && npm run build", wantIntent: "npm run", wantForm: "npm run …", wantDir: "web"},
		{command: "FOO=1 make test", wantIntent: "make test", wantForm: "make test", wantEnv: map[string]string{"FOO": "1"}},
		{command: "export FOO=1 && make test", wantIntent: "make test", wantForm: "make test", wantEnv: map[string]string{"FOO": "1"}},
		{command: `openspec instructions x --json | python3 -c "import json"`, wantIntent: ""},
		{command: "grep -n x f.go && psql -c 'select 1'", wantIntent: ""},
		{command: "make test; cd sub", wantIntent: ""},
		{command: "go test ./...\n", wantIntent: "go test", wantForm: "go test …"},
		{command: "go test -run 'TestX|TestY' ./cmd/graft", wantIntent: "go test", wantForm: "go test -run …"},
		{command: "golangci-lint run --fix=true ./...", wantIntent: "golangci-lint run", wantForm: "golangci-lint run --fix=… …"},
		{command: "python3 -m pytest -x tests/", wantIntent: "pytest", wantForm: "python3 -m pytest -x …"},
		{command: ".venv/bin/python3.12 -m pip install -r req.txt", wantIntent: "pip install", wantForm: ".venv/bin/python3.12 -m pip install -r …"},
		{command: "uv run ruff check .", wantIntent: "ruff check", wantForm: "uv run ruff check …"},
		{command: "npx eslint .", wantIntent: "eslint", wantForm: "npx eslint …"},
		{command: "python3 scripts/gen.py --out x", wantIntent: "python gen.py", wantForm: "python3 … --out …"},
		{command: "python -m", wantIntent: "python", wantForm: "python -m"},
		{command: "uv run --with x pytest", wantIntent: "uv run", wantForm: "uv run --with …"},
		{command: "", wantIntent: ""},
		{command: "cd sub", wantIntent: ""},
	}
	for _, tt := range tests {
		t.Run(tt.command, func(t *testing.T) {
			got := parseShellCommand(tt.command)
			if got.intent != tt.wantIntent || tt.wantIntent != "" && (got.form != tt.wantForm || got.dir != tt.wantDir) {
				t.Errorf("parseShellCommand(%q) = intent %q form %q dir %q, want %q %q %q",
					tt.command, got.intent, got.form, got.dir, tt.wantIntent, tt.wantForm, tt.wantDir)
			}
			if tt.wantEnv != nil && !maps.Equal(got.env, tt.wantEnv) {
				t.Errorf("parseShellCommand(%q) env = %v, want %v", tt.command, got.env, tt.wantEnv)
			}
		})
	}
}

func TestShellErrorLine(t *testing.T) {
	tests := []struct {
		name, text, want string
		skip             bool
	}{
		{name: "command not found", text: "Exit code 127\nzsh: command not found: ruff", want: "zsh: command not found: ruff"},
		{name: "empty grep", text: "Exit code 1\n", want: ""},
		{name: "no error words", text: "Exit code 1\nfoo.go:12: x", want: ""},
		{name: "hook denial", text: "PreToolUse:Bash hook error: [bash x]: blocked, error", skip: true},
		{name: "user rejection", text: "The user doesn't want to proceed with this tool use. Error", skip: true},
		{name: "classifier denial", text: "Permission for this action was denied by the Claude Code auto mode classifier. Reason: error", skip: true},
		{name: "lost output", text: "Command output was lost: the temp filesystem is full. Error", skip: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, skip := shellErrorLine(tt.text)
			if got != tt.want || skip != tt.skip {
				t.Errorf("shellErrorLine(%q) = %q, %v, want %q, %v", tt.text, got, skip, tt.want, tt.skip)
			}
		})
	}
}

func TestMaskShellText(t *testing.T) {
	root := filepath.FromSlash("/repo/app")
	tests := []struct{ in, want string }{
		{in: "API_TOKEN=abc123 make test", want: "API_TOKEN=… make test"},
		{in: "DB_PASSWORD=x go test", want: "DB_PASSWORD=… go test"},
		{in: "MODE=dev make", want: "MODE=dev make"},
		{in: "open /repo/app/internal/x.go: no such file", want: "open internal/x.go: no such file"},
		{in: "token ghp_0123456789abcdefghijklmnopqrstuvwxyzAB rejected", want: "token … rejected"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := maskShellText(tt.in, root); got != tt.want {
				t.Errorf("maskShellText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestShellResponseOutcome(t *testing.T) {
	tests := []struct {
		name       string
		response   any
		wantFailed bool
		wantText   string
	}{
		{name: "opencode failure", response: map[string]any{"content": "sh: ruff: command not found", "isError": true, "exit_code": float64(127)}, wantFailed: true, wantText: "sh: ruff: command not found"},
		{name: "exit code zero", response: map[string]any{"output": "ok", "exit_code": float64(0)}, wantText: "ok"},
		{name: "metadata exit", response: map[string]any{"output": "boom error", "metadata": map[string]any{"exit": float64(2)}}, wantFailed: true, wantText: "boom error"},
		{name: "is_error flag", response: map[string]any{"stdout": "", "stderr": "fatal error", "is_error": true}, wantFailed: true, wantText: "fatal error"},
		{name: "string with trailing code", response: "cannot find module\nProcess exited with code 1", wantFailed: true, wantText: "cannot find module\nProcess exited with code 1"},
		{name: "string with leading code", response: "Exit code: 3\nerror: x", wantFailed: true, wantText: "Exit code: 3\nerror: x"},
		{name: "plain string", response: "done", wantText: "done"},
		{name: "returncode", response: map[string]any{"output": "x: error", "returncode": float64(1)}, wantFailed: true, wantText: "x: error"},
		{name: "codex header", response: "Exit code: 0\nWall time: 0.1 seconds\nOutput:\nerror count 0", wantText: "Exit code: 0\nWall time: 0.1 seconds\nOutput:\nerror count 0"},
		{name: "numeric success overrides error line", response: map[string]any{"output": "error: handled", "exit_code": float64(0), "isError": true}, wantText: "error: handled"},
		{name: "numeric failure overrides flag", response: map[string]any{"output": "error: failed", "exitCode": float64(7), "isError": false}, wantFailed: true, wantText: "error: failed"},
		{name: "flag success overrides text", response: map[string]any{"content": "error: handled\nProcess exited with code 7", "isError": false}, wantText: "error: handled\nProcess exited with code 7"},
		{name: "object error line fallback", response: map[string]any{"content": "error: failed"}, wantFailed: true, wantText: "error: failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, failed := shellResponseOutcome(tt.response)
			if failed != tt.wantFailed || text != tt.wantText {
				t.Errorf("shellResponseOutcome(%v) = %q, %v, want %q, %v", tt.response, text, failed, tt.wantText, tt.wantFailed)
			}
		})
	}
}

func TestShellResponseOutcomeCodexPayloads(t *testing.T) {
	tests := []struct {
		file       string
		wantText   string
		wantFailed bool
	}{
		{file: "success.json", wantText: "GRAFT_CODEX_CAPTURE_OK\n"},
		{file: "failure-error-line.json", wantText: "error: GRAFT_CODEX_CAPTURE_FAILURE\n", wantFailed: true},
		// Codex omits the exit status; this payload cannot identify its exit 7.
		{file: "failure-without-error-line.json", wantText: "GRAFT_CODEX_CAPTURE_FAILURE\n"},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			t.Parallel()
			data, err := os.ReadFile(filepath.Join("testdata", "codex-post-tool-use", tt.file))
			if err != nil {
				t.Fatal(err)
			}
			var input hookInput
			if err := jsonv2.Unmarshal(data, &input); err != nil {
				t.Fatal(err)
			}
			text, failed := shellResponseOutcome(input["tool_response"])
			if text != tt.wantText || failed != tt.wantFailed {
				t.Errorf("shellResponseOutcome(%s) = %q, %v, want %q, %v", tt.file, text, failed, tt.wantText, tt.wantFailed)
			}
		})
	}
}

func TestShellScope(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"go.mod", "services/billing/go.mod"} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("module x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "services", "billing", "src"), 0o750); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		cwd       string
		want      string
		wantFound bool
	}{
		{cwd: filepath.Join(root, "services", "billing", "src"), want: "services/billing", wantFound: true},
		{cwd: filepath.Join(root, "services"), want: ".", wantFound: true},
		{cwd: root, want: ".", wantFound: true},
		{cwd: "", want: ".", wantFound: true},
		{cwd: filepath.Dir(root), wantFound: false},
	}
	for _, tt := range tests {
		t.Run(tt.cwd, func(t *testing.T) {
			got, ok := shellScope(root, tt.cwd)
			if ok != tt.wantFound || ok && got != tt.want {
				t.Errorf("shellScope(root, %q) = %q, %v, want %q, %v", tt.cwd, got, ok, tt.want, tt.wantFound)
			}
		})
	}
}
