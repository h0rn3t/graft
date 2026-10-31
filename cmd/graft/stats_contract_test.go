package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatsContract(t *testing.T) {
	t.Setenv("GRAFT_DIR", "")
	root := t.TempDir()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"empty text", []string{"stats", root}, "graft stats: no session recorded yet — use graft in an agent session, then look again.\n"},
		{"empty json", []string{"stats", root, "--json"}, "null\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := run(tc.args, &stdout, &stderr); got != 0 || stdout.String() != tc.want || stderr.Len() != 0 {
				t.Errorf("run(%v) = (%d, %q, %q), want (0, %q, empty)", tc.args, got, stdout.String(), stderr.String(), tc.want)
			}
		})
	}
	sessions := filepath.Join(root, "graft", ".cache", "session")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "recent.json"), []byte(`{"lastQuery":"auth","perAgentQuery":{},"graftReads":3,"sourceReads":1,"savedTokens":1200,"inputCostMicros":25000000,"inputTokensBilled":10000}`), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if got := run([]string{"stats", root}, &stdout, &stderr); got != 0 || stderr.Len() != 0 {
		t.Fatalf("run(stats %q) = (%d, %q, %q), want success", root, got, stdout.String(), stderr.String())
	}
	for _, want := range []string{"session recent", "graft reads:   3", "source reads:  1", "75% graft", "tokens saved:  ~1,200", "value saved:   ~$3.00", "last query:    auth"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("run(stats %q) stdout = %q, want %q", root, stdout.String(), want)
		}
	}
	stdout.Reset()
	if got := run([]string{"stats", root, "--json"}, &stdout, &stderr); got != 0 || !strings.Contains(stdout.String(), `"id": "recent"`) {
		t.Errorf("run(stats %q --json) = (%d, %q, %q), want session JSON", root, got, stdout.String(), stderr.String())
	}
}

func TestGroupedNumber(t *testing.T) {
	for _, tc := range []struct {
		value float64
		want  string
	}{
		{0, "0"},
		{146, "146"},
		{12345, "12,345"},
		{1234567, "1,234,567"},
		{-146, "-146"},
		{-1234567, "-1,234,567"},
		{-0.4, "0"},
	} {
		if got := groupedNumber(tc.value); got != tc.want {
			t.Errorf("groupedNumber(%v) = %q, want %q", tc.value, got, tc.want)
		}
	}
}

func TestStatsReportsNetAndPush(t *testing.T) {
	t.Setenv("GRAFT_DIR", "")
	root := t.TempDir()
	writeFixtureFile(t, root, "graft/.cache/session/s1.json", `{"perAgentQuery":{},"graftReads":1,"sourceReads":1,"savedTokens":10000,"injectedTokens":2500}`)
	stats := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if got := run(append([]string{"stats", root}, args...), &stdout, &stderr); got != 0 {
			t.Fatalf("run(stats %v) = %d, stderr %q; want 0", args, got, stderr.String())
		}
		return stdout.String()
	}
	for _, want := range []string{"tokens saved:  ~10,000", "injected:      ~2,500", "net saved:     ~7,500", "push retrieval:   on (support 0)", "push blast:       on (support 0)"} {
		if got := stats(); !strings.Contains(got, want) {
			t.Errorf("stats() = %q, want %q", got, want)
		}
	}

	// Only the blast-radius channel turns off.
	for _, id := range []string{"bad1", "bad2"} {
		recordPushOutcomes(root, hookHostClaude, id, map[string]pushTally{pushBlast: {Contradicted: 1}}, nil)
	}
	for _, want := range []string{"push blast:       off (support -1.8, re-audit in 5 new sessions)", "push retrieval:   on (support 0)", "push subagents:   on (support 0)"} {
		if got := stats(); !strings.Contains(got, want) {
			t.Errorf("stats(negative blast support) = %q, want %q", got, want)
		}
	}
	got := stats("--json")
	for _, want := range []string{`"blast": {
      "state": "off",
      "support": -1.8,
      "untilReaudit": 5
    }`, `"retrieval": {
      "state": "on",
      "support": 0,
      "untilReaudit": 0
    }`, `"injectedTokens": 2500`} {
		if !strings.Contains(got, want) {
			t.Errorf("stats(--json, negative blast support) = %q, want %q", got, want)
		}
	}

	codex := filepath.Join(root, "graft", ".cache", "session", "codex.json")
	writeFixtureFile(t, root, "graft/.cache/session/codex.json", `{"perAgentQuery":{},"host":"codex"}`)
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(codex, later, later); err != nil {
		t.Fatal(err)
	}
	if got, want := stats(), "push blast:       on (support 0)"; !strings.Contains(got, "session codex") || !strings.Contains(got, want) {
		t.Errorf("stats(latest session is codex) = %q, want the codex ledger: %q", got, want)
	}
}

func TestStatsReportsHiddenTools(t *testing.T) {
	t.Setenv("GRAFT_DIR", "")
	root := t.TempDir()
	writeFixtureFile(t, root, "graft/.cache/session/s1.json", `{"perAgentQuery":{}}`)
	stats := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if got := run(append([]string{"stats", root}, args...), &stdout, &stderr); got != 0 {
			t.Fatalf("run(stats %v) = %d, stderr %q; want 0", args, got, stderr.String())
		}
		return stdout.String()
	}
	if got, want := stats(), "  tools:            all listed\n"; !strings.Contains(got, want) {
		t.Errorf("stats(no tool record) = %q, want %q", got, want)
	}
	seedToolRecord(t, root, 2)
	if got, want := stats(), "  tools:            hidden graft_check_freshness (re-audit in 3 starts)\n"; !strings.Contains(got, want) {
		t.Errorf("stats(graft_check_freshness unused) = %q, want %q", got, want)
	}
	if got, want := stats("--json"), `"tools": {
    "hidden": [
      "graft_check_freshness"
    ],
    "untilReaudit": 3
  }`; !strings.Contains(got, want) {
		t.Errorf("stats(--json, graft_check_freshness unused) = %q, want %q", got, want)
	}
}

func TestStatsCountsMicroskills(t *testing.T) {
	t.Setenv("GRAFT_DIR", "")
	root := t.TempDir()
	writeFixtureFile(t, root, "graft/.cache/session/s1.json", `{"perAgentQuery":{},"graftReads":1}`)
	silent := validatedFact(".", "make lint", "./make lint")
	for i := range silent.Sessions {
		silent.Sessions[i].Fired = false
	}
	silent.settle()
	facts := []microskill{validatedFact(".", "ruff format", ".venv/bin/ruff format …"), silent}
	for i := range 3 {
		candidate := validatedFact(".", fmt.Sprint("tool", i), "x")
		candidate.Status = microskillCandidate
		facts = append(facts, candidate)
	}
	writeMicroskills(t, root, facts...)
	var stdout, stderr bytes.Buffer
	if got := run([]string{"stats", root}, &stdout, &stderr); got != 0 {
		t.Fatalf("run(stats) = %d, stderr %q", got, stderr.String())
	}
	if want := "microskills:      2 validated (1 injectable), 3 candidates"; !strings.Contains(stdout.String(), want) {
		t.Errorf("stats() = %q, want %q", stdout.String(), want)
	}
	stdout.Reset()
	if got := run([]string{"stats", root, "--json"}, &stdout, &stderr); got != 0 {
		t.Fatalf("run(stats --json) = %d", got)
	}
	if want := "\"microskills\": {\n    \"validated\": 2,\n    \"injectable\": 1,\n    \"candidates\": 3\n  }"; !strings.Contains(stdout.String(), want) {
		t.Errorf("stats --json = %q, want %q", stdout.String(), want)
	}
}
