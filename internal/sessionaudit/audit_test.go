package sessionaudit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// A session of three rounds: a raw search and a graft call answered before
// the second round, a file view answered before a compaction, and one round
// after it. Round m1 is written as two lines, as Claude Code writes a
// response of two blocks.
const sampleSession = `{"type":"user","message":{"content":"find the parser"}}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":5},"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"grep -rn parse internal/"}}]}}
{"type":"assistant","message":{"id":"m1","usage":{"input_tokens":10,"cache_creation_input_tokens":100,"cache_read_input_tokens":1000,"output_tokens":5},"content":[{"type":"tool_use","id":"t2","name":"mcp__graft__graft_find_code","input":{"query":"parser"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"0123456789"},{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"01234"}]}]}}
{"type":"assistant","message":{"id":"m2","usage":{"input_tokens":1,"cache_creation_input_tokens":20,"cache_read_input_tokens":1100,"output_tokens":7},"content":[{"type":"tool_use","id":"t3","name":"Read","input":{"file_path":"a.go"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t3","content":"abc"}]}}
{"type":"system","subtype":"compact_boundary"}
{"type":"assistant","message":{"id":"m3","usage":{"input_tokens":2,"cache_creation_input_tokens":30,"cache_read_input_tokens":300,"output_tokens":9},"content":[{"type":"text","text":"done"}]}}
`

var sampleGroup = Group{
	Conversations: 1, Rounds: 3, Compactions: 1,
	Input: 13, CacheWrite: 150, CacheRead: 2400, Output: 21,
	Tools: map[string]Tool{
		"search": {Calls: 1, Chars: 10, CharRounds: 10},
		"graft":  {Calls: 1, Chars: 5, CharRounds: 5},
		"view":   {Calls: 1, Chars: 3},
	},
}

// writeTranscript stores text as a transcript of repo's project under
// projects, last written at modified.
func writeTranscript(t *testing.T, projects, name, text string, modified time.Time) {
	t.Helper()
	path := filepath.Join(projects, "-work-my-repo-x", filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("os.MkdirAll() error = %v, want nil", err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v, want nil", err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatalf("os.Chtimes() error = %v, want nil", err)
	}
}

const sampleRepo = "/work/my.repo_x"

var (
	sampleSince = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	sampleAfter = sampleSince.Add(48 * time.Hour)
)

func TestAudit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files map[string]string // transcript name → text, all written after since
		old   map[string]string // transcripts last written before since
		want  Report
	}{
		{
			name:  "one session",
			files: map[string]string{"s1.jsonl": sampleSession},
			want:  Report{Repo: sampleRepo, Main: sampleGroup},
		},
		{
			name:  "a subagent is its own conversation",
			files: map[string]string{"s1.jsonl": sampleSession, "s1/subagents/agent-a.jsonl": sampleSession},
			want:  Report{Repo: sampleRepo, Main: sampleGroup, Subagents: sampleGroup},
		},
		{
			name:  "a session last written before since is left out",
			files: map[string]string{"s1.jsonl": sampleSession},
			old:   map[string]string{"s0.jsonl": sampleSession, "s0/subagents/agent-a.jsonl": sampleSession},
			want:  Report{Repo: sampleRepo, Main: sampleGroup},
		},
		{
			name: "a line still being written ends the session there",
			files: map[string]string{"s1.jsonl": sampleSession +
				`{"type":"assistant","message":{"id":"m4","usage":{"input_tokens":5`},
			want: Report{Repo: sampleRepo, Main: sampleGroup},
		},
		{
			name: "no transcripts",
			want: Report{Repo: sampleRepo},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			projects := t.TempDir()
			for name, text := range tt.files {
				writeTranscript(t, projects, name, text, sampleAfter)
			}
			for name, text := range tt.old {
				writeTranscript(t, projects, name, text, sampleSince.Add(-time.Hour))
			}
			got, err := Audit(projects, sampleRepo, sampleSince)
			if err != nil {
				t.Fatalf("Audit(%q) error = %v, want nil", sampleRepo, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Audit(%q) = %+v, want %+v", sampleRepo, got, tt.want)
			}
		})
	}
}

func TestAuditWithoutProjectsDirectory(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent")
	if _, err := Audit(missing, sampleRepo, sampleSince); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Audit(%q) error = %v, want %v", missing, err, fs.ErrNotExist)
	}
}

// Each tool call counts under the kind of work it did, and a shell command
// under the first program it ran.
func TestAuditSortsCallsByKind(t *testing.T) {
	t.Parallel()
	calls := []struct{ tool, command, want string }{
		{"Bash", "grep -rn x .", "search"},
		{"Bash", "cd internal && rg x", "search"},
		{"Bash", "git grep -n x", "search"},
		{"Bash", "find . -name '*.go'", "search"},
		{"Grep", "", "search"},
		{"Glob", "", "search"},
		{"Bash", "sed -n 10,20p a.go; sed -n 40,50p a.go", "view"},
		{"Bash", "cat a.go", "view"},
		{"Read", "", "view"},
		{"Bash", "graft grep parse", "graft"},
		{"mcp__graft__graft_read_symbol", "", "graft"},
		{"Bash", "cat > a.go <<'EOF'", "shell"},
		{"Bash", "sed -i s/a/b/ a.go", "shell"},
		{"Bash", "graft build .", "shell"},
		{"Bash", "go test ./...", "shell"},
		{"Bash", "git status", "shell"},
		{"Edit", "", "edit"},
		{"Write", "", "edit"},
		{"mcp__other__lookup", "", "other"},
		{"Agent", "", "other"},
	}
	want := make(map[string]Tool)
	var session strings.Builder
	for i, call := range calls {
		tool := want[call.want]
		tool.Calls++
		want[call.want] = tool
		fmt.Fprintf(&session, `{"type":"assistant","message":{"id":"m%d","usage":{"input_tokens":1},`+
			`"content":[{"type":"tool_use","id":"t","name":%q,"input":{"command":%q}}]}}`+"\n", i, call.tool, call.command)
	}
	projects := t.TempDir()
	writeTranscript(t, projects, "s1.jsonl", session.String(), sampleAfter)
	got, err := Audit(projects, sampleRepo, sampleSince)
	if err != nil {
		t.Fatalf("Audit() error = %v, want nil", err)
	}
	if !reflect.DeepEqual(got.Main.Tools, want) {
		t.Errorf("Audit() tools = %+v, want %+v", got.Main.Tools, want)
	}
}

func TestGroupWeights(t *testing.T) {
	t.Parallel()
	if got := sampleGroup.RoundTokens(); got != 854 {
		t.Errorf("RoundTokens() = %d, want 854: (13 + 150 + 2400) / 3 rounds", got)
	}
	if got := sampleGroup.CarriedRounds(); got < 0.83 || got > 0.84 {
		t.Errorf("CarriedRounds() = %v, want 15 character-rounds / 18 characters", got)
	}
	if graft, raw := sampleGroup.Searches(); graft != 1 || raw != 1 {
		t.Errorf("Searches() = (%d, %d), want (1, 1)", graft, raw)
	}
	var empty Group
	if tokens, rounds := empty.RoundTokens(), empty.CarriedRounds(); tokens != 0 || rounds != 0 {
		t.Errorf("empty group weights = (%d, %v), want zeros", tokens, rounds)
	}
}

func TestReportHuman(t *testing.T) {
	t.Parallel()
	got := Report{Repo: sampleRepo, Main: sampleGroup}.Human()
	for _, want := range []string{sampleRepo, "1 conversation, 3 rounds, 1 compaction", "854 tokens", "1 of 2 searches", "search"} {
		if !strings.Contains(got, want) {
			t.Errorf("Human() = %q, want it to contain %q", got, want)
		}
	}
	if got := (Report{Repo: sampleRepo}).Human(); !strings.Contains(got, "no conversations") {
		t.Errorf("Human(no transcripts) = %q, want it to say there are no conversations", got)
	}
}
