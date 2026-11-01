package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/h0rn3t/Graft/internal/graph"
)

// ruffFailed and ruffWorked are the repair the spec's scenarios use.
var (
	ruffFailed = shellObservation{Intent: "ruff format", Form: "ruff format …", Scope: ".", Failed: true, Error: "zsh: command not found: ruff", Tokens: 30}
	ruffWorked = shellObservation{Intent: "ruff format", Form: ".venv/bin/ruff format …", Scope: "."}
	ruffBare   = shellObservation{Intent: "ruff format", Form: "ruff format …", Scope: "."}
)

func TestMicroskillCandidates(t *testing.T) {
	envFailed := shellObservation{Intent: "go test", Form: "go test …", Scope: ".", Failed: true, Error: "panic: DATABASE_URL is not set"}
	envWorked := shellObservation{Intent: "go test", Form: "go test …", Scope: ".", Env: map[string]string{"DATABASE_URL": "postgres://localhost/test"}}
	tokenFailed := shellObservation{Intent: "gh pr", Form: "gh pr …", Scope: ".", Failed: true, Error: "error: GITHUB_TOKEN missing"}
	tokenWorked := shellObservation{Intent: "gh pr", Form: "gh pr …", Scope: ".", Env: map[string]string{"GITHUB_TOKEN": ""}}
	doneFailed := shellObservation{Intent: "make x", Form: "make x", Scope: ".", Failed: true, Error: "error: not done"}
	doneWorked := shellObservation{Intent: "make x", Form: "./make x", Scope: "."}
	tests := []struct {
		name         string
		observations []shellObservation
		want         []string
	}{
		{name: "form repair", observations: []shellObservation{ruffFailed, ruffWorked},
			want: []string{"`ruff format …` fails here with `zsh: command not found: ruff`; `.venv/bin/ruff format …` works."}},
		{name: "env value", observations: []shellObservation{envFailed, envWorked},
			want: []string{"`go test …` needs `DATABASE_URL` set here: `DATABASE_URL=postgres://localhost/test` works."}},
		{name: "secret variable", observations: []shellObservation{tokenFailed, tokenWorked},
			want: []string{"`gh pr …` needs `GITHUB_TOKEN` set here."}},
		{name: "failure without repair", observations: []shellObservation{ruffFailed, ruffFailed}},
		{name: "repair by another agent", observations: []shellObservation{ruffFailed, {Agent: "sub", Intent: "ruff format", Form: ".venv/bin/ruff format …", Scope: "."}}},
		{name: "same repair twice", observations: []shellObservation{ruffFailed, ruffWorked, ruffFailed, ruffWorked},
			want: []string{"`ruff format …` fails here with `zsh: command not found: ruff`; `.venv/bin/ruff format …` works."}},
		{name: "completion wording refused", observations: []shellObservation{doneFailed, doneWorked}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, fact := range microskillCandidates(tt.observations) {
				got = append(got, fact.Instruction)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("microskillCandidates(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}

// TestMicroskillCandidatesAcrossLaunchers pairs a failed tool with the form
// that ran it through an interpreter, a launcher or a virtualenv.
func TestMicroskillCandidatesAcrossLaunchers(t *testing.T) {
	root := t.TempDir()
	tests := []struct{ failed, error, worked, want string }{
		{"pytest -x tests/", "zsh: command not found: pytest", "python3 -m pytest -x tests/",
			"`pytest -x …` fails here with `zsh: command not found: pytest`; `python3 -m pytest -x …` works."},
		{"python3 tools/gen.py", "ModuleNotFoundError: No module named 'yaml'", ".venv/bin/python tools/gen.py",
			"`python3 …` fails here with `ModuleNotFoundError: No module named 'yaml'`; `.venv/bin/python …` works."},
		{"ruff check .", "zsh: command not found: ruff", "uv run ruff check .",
			"`ruff check …` fails here with `zsh: command not found: ruff`; `uv run ruff check …` works."},
	}
	for _, tt := range tests {
		t.Run(tt.failed, func(t *testing.T) {
			failed, ok1 := newShellObservation(root, "", "1", tt.failed, root, tt.error+"\nExit code 1", true)
			worked, ok2 := newShellObservation(root, "", "2", tt.worked, root, "ok", false)
			if !ok1 || !ok2 {
				t.Fatalf("newShellObservation kept %v, %v; want both", ok1, ok2)
			}
			var got []string
			for _, fact := range microskillCandidates([]shellObservation{failed, worked}) {
				got = append(got, fact.Instruction)
			}
			if !slices.Equal(got, []string{tt.want}) {
				t.Errorf("microskillCandidates(%q then %q) = %q, want %q", tt.failed, tt.worked, got, tt.want)
			}
		})
	}
}

// ruffFact is the candidate the ruff repair proposes.
func ruffFact(t *testing.T) microskill {
	t.Helper()
	facts := microskillCandidates([]shellObservation{ruffFailed, ruffWorked})
	if len(facts) != 1 {
		t.Fatalf("microskillCandidates(ruff repair) = %d facts, want 1", len(facts))
	}
	fact := facts[0]
	fact.Status = microskillCandidate
	return fact
}

func TestMicroskillLifecycle(t *testing.T) {
	repair := []shellObservation{ruffFailed, ruffWorked}
	tests := []struct {
		name       string
		sessions   [][]shellObservation
		injected   bool
		wantStatus string
	}{
		{name: "promotion", sessions: [][]shellObservation{repair, repair, repair}, wantStatus: microskillValidated},
		{name: "two sessions", sessions: [][]shellObservation{repair, repair}, wantStatus: microskillCandidate},
		{name: "three repairs in one session", sessions: [][]shellObservation{slices.Concat(repair, repair, repair)}, wantStatus: microskillCandidate},
		{name: "working form alone", sessions: [][]shellObservation{repair, {ruffWorked}, {ruffWorked}, {ruffWorked}}, wantStatus: microskillCandidate},
		{name: "two consecutive contradictions", sessions: [][]shellObservation{repair, repair, repair, repair, repair, repair, {ruffBare}, {ruffBare}}, wantStatus: microskillRetired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fact := ruffFact(t)
			for i, observations := range tt.sessions {
				fact.record(fmt.Sprint("s", i), fmt.Sprintf("2026-10-31T10:00:%02dZ", i), observations, false, 0)
			}
			if fact.Status != tt.wantStatus {
				t.Errorf("status after %s = %q, want %q (sessions %+v)", tt.name, fact.Status, tt.wantStatus, fact.Sessions)
			}
		})
	}
}

func TestMicroskillOneContradictionRetires(t *testing.T) {
	fact := ruffFact(t)
	for i := range 5 {
		fact.record(fmt.Sprint("s", i), "2026-10-31T10:00:00Z", []shellObservation{ruffFailed, ruffWorked}, false, 0)
	}
	fact.record("s5", "2026-10-31T10:00:00Z", []shellObservation{ruffBare}, false, 0)
	if got := fact.support(); math.Abs(got-(-2.31072)) > 1e-9 {
		t.Errorf("support after five confirmations and one contradiction = %v, want -2.31072", got)
	}
	if fact.Status != microskillRetired {
		t.Errorf("status = %q, want retired", fact.Status)
	}
}

func TestMicroskillRefoldIsIdempotent(t *testing.T) {
	fact := ruffFact(t)
	observations := []shellObservation{ruffFailed, ruffWorked}
	fact.record("s", "2026-10-31T10:00:00Z", observations, false, 0)
	first := fact.Sessions
	if fact.record("s", "2026-10-31T11:00:00Z", observations, false, 0) {
		t.Errorf("record(same session, same observations) changed the fact: %+v -> %+v", first, fact.Sessions)
	}
	if len(fact.Sessions) != 1 || fact.Metrics.Confirmed != 1 {
		t.Errorf("after refold sessions = %+v metrics %+v, want one confirming session", fact.Sessions, fact.Metrics)
	}
}

// validatedFact is a validated, firing fact of the given scope and intent,
// with working form working.
func validatedFact(scope, intent, working string) microskill {
	fact := microskill{
		Family: microskillFormRepair, Intent: intent, Scope: scope, Failing: intent + " …", Working: working,
		ErrorClass: "command not found", Status: microskillValidated,
	}
	fact.ID = microskillID(fact.Family, scope, intent, fact.Failing, working)
	fact.Instruction = "`" + fact.Failing + "` fails here with `command not found`; `" + working + "` works."
	for i := range 4 {
		fact.Sessions = append(fact.Sessions, microskillSession{ID: fmt.Sprint("v", i), At: "2026-10-30T00:00:00Z", Verdict: 1, Exercised: true, Fired: true})
	}
	fact.settle()
	return fact
}

func writeMicroskills(t *testing.T, root string, facts ...microskill) {
	t.Helper()
	if err := os.MkdirAll(hookCacheDir(root), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := writeHookJSONAtomic(microskillsPath(root), microskillStore{Version: 1, Facts: facts}); err != nil {
		t.Fatal(err)
	}
}

func TestPickMicroskills(t *testing.T) {
	everywhere := func(string) bool { return true }
	weak := validatedFact(".", "make lint", "./make lint")
	weak.Sessions = append(weak.Sessions, make([]microskillSession, 6)...)
	for i := 4; i < 10; i++ {
		weak.Sessions[i] = microskillSession{ID: fmt.Sprint("w", i), Exercised: true}
	}
	weak.Sessions[0].Fired, weak.Sessions[1].Fired = false, false
	weak.settle()
	candidate := validatedFact(".", "make vet", "./make vet")
	candidate.Status = microskillCandidate
	strong := validatedFact(".", "pytest", "python -m pytest …")
	competitor := validatedFact(".", "pytest", "uv run pytest …")
	competitor.Sessions = competitor.Sessions[:len(competitor.Sessions)-1]
	competitor.Sessions = append(competitor.Sessions, microskillSession{ID: "x", Verdict: -1, Exercised: true, Fired: true})
	competitor.settle()
	competitor.Status = microskillValidated
	var six []microskill
	for i := range 6 {
		six = append(six, validatedFact(".", fmt.Sprint("tool", i, " run"), fmt.Sprint("./tool", i, " run")))
	}
	tests := []struct {
		name     string
		facts    []microskill
		injected []string
		want     int
	}{
		{name: "candidate", facts: []microskill{candidate}, want: 0},
		{name: "fires three of ten", facts: []microskill{weak}, want: 0},
		{name: "six matching", facts: six, want: 4},
		{name: "competing working forms", facts: []microskill{competitor, strong}, want: 1},
		{name: "already injected", facts: []microskill{strong}, injected: []string{strong.ID}, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pickMicroskills(tt.facts, everywhere, tt.injected, 0)
			if len(got) != tt.want {
				t.Fatalf("pickMicroskills(%s) = %d facts, want %d", tt.name, len(got), tt.want)
			}
			if tokens := hookContextTokens(renderMicroskills(got)); len(got) > 0 && tokens > microskillSessionTokens {
				t.Errorf("pickMicroskills(%s) block = %d tokens, want ≤ %d", tt.name, tokens, microskillSessionTokens)
			}
			if tt.name == "competing working forms" && got[0].ID != strong.ID {
				t.Errorf("pickMicroskills(competing) = %q, want the higher-support %q", got[0].Working, strong.Working)
			}
		})
	}
}

func TestMicroskillStoreFailsOpenAndCaps(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, hookCacheDir(root), "microskills.json", "{not json")
	if got := readMicroskills(root); got != nil {
		t.Fatalf("readMicroskills(corrupt) = %+v, want none", got)
	}
	var facts []microskill
	for i := range microskillStoreLimit {
		fact := validatedFact(".", fmt.Sprint("t", i), "x")
		fact.UpdatedAt = fmt.Sprintf("2026-10-%02dT00:00:00Z", 1+i%28)
		if i < 3 {
			fact.Status, fact.UpdatedAt = microskillRetired, fmt.Sprintf("2026-09-0%dT00:00:00Z", 3-i)
		}
		facts = append(facts, fact)
	}
	oldestRetired := facts[2].ID
	if err := updateMicroskills(root, func([]microskill) []microskill {
		return append(facts, validatedFact(".", "new", "y"))
	}); err != nil {
		t.Fatalf("updateMicroskills() error = %v", err)
	}
	got := readMicroskills(root)
	if len(got) != microskillStoreLimit || slices.ContainsFunc(got, func(fact microskill) bool { return fact.ID == oldestRetired }) {
		t.Errorf("store after overflow = %d facts, oldest retired kept = %v; want %d without it",
			len(got), slices.ContainsFunc(got, func(fact microskill) bool { return fact.ID == oldestRetired }), microskillStoreLimit)
	}

	empty := t.TempDir()
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Go(func() {
			_ = updateMicroskills(empty, func(facts []microskill) []microskill {
				return append(facts, validatedFact(".", fmt.Sprint("c", i), "x"))
			})
		})
	}
	wg.Wait()
	if got := readMicroskills(empty); len(got) != 2 {
		t.Errorf("store after two concurrent updates = %d facts, want 2", len(got))
	}
}

func TestMicroskillAnchors(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "services/billing/go.mod", "module billing\n")
	writeFixtureFile(t, root, "services/billing/src/x.go", "package x\n")
	scope, _ := shellScope(root, filepath.Join(root, "services", "billing", "src"))
	if scope != "services/billing" {
		t.Fatalf("shellScope(services/billing/src) = %q, want services/billing", scope)
	}
	fact := validatedFact(scope, "go test", "go test -count=1 …")
	fact.Anchors = microskillAnchors(root, scope)
	retired := validatedFact(scope, "go vet", "go vet -x …")
	retired.Status, retired.Anchors = microskillRetired, fact.Anchors
	writeMicroskills(t, root, fact, retired)
	if got := readMicroskills(root); len(got) != 2 || got[0].Status != microskillValidated || got[1].Status != microskillRetired {
		t.Fatalf("readMicroskills(anchors unchanged) = %+v, want validated and retired kept", got)
	}

	writeFixtureFile(t, root, "services/billing/go.mod", "module billing\n\ngo 1.27\n")
	got := readMicroskills(root)
	if len(got) != 1 || got[0].Status != microskillCandidate || !got[0].Revalidate {
		t.Fatalf("readMicroskills(go.mod changed) = %+v, want only the demoted fact", got)
	}
	got[0].record("after", "2099-01-01T00:00:00Z", []shellObservation{
		{Intent: "go test", Form: "go test …", Scope: scope, Failed: true, Error: "command not found"},
		{Intent: "go test", Form: "go test -count=1 …", Scope: scope},
	}, false, 0)
	if got[0].Status != microskillValidated {
		t.Errorf("demoted fact after one confirming session = %q, want validated", got[0].Status)
	}
}

func TestFoldKeepsRetiredFactRetired(t *testing.T) {
	root := t.TempDir()
	fact := ruffFact(t)
	fact.Status = microskillRetired
	writeMicroskills(t, root, fact)
	if err := updateHookSession(root, "s", func(session *sessionState) bool {
		session.ShellObservations = []shellObservation{ruffFailed, ruffWorked}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	foldMicroskills(root, "s")
	if got := readMicroskills(root); len(got) != 1 || got[0].Status != microskillRetired || len(got[0].Sessions) != 0 {
		t.Errorf("store after the repair recurs = %+v, want the retired fact untouched", got)
	}
}

// appendShellTurn appends a Bash call and its result to a transcript.
func appendShellTurn(t *testing.T, path, id, command, cwd, result string, isError bool) {
	t.Helper()
	use, err := json.Marshal(map[string]any{"type": "assistant", "cwd": cwd, "message": map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	res, err := json.Marshal(map[string]any{"type": "user", "cwd": cwd, "message": map[string]any{
		"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": result, "is_error": isError}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(string(use) + "\n" + string(res) + "\n"); err != nil {
		t.Fatal(err)
	}
}

// appendRuffRepair appends the failing bare ruff call and the working one.
func appendRuffRepair(t *testing.T, path, prefix, root string) {
	t.Helper()
	appendShellTurn(t, path, prefix+"a", "ruff format .", root, "Exit code 127\nzsh: command not found: ruff", true)
	appendShellTurn(t, path, prefix+"b", ".venv/bin/ruff format .", root, "1 file reformatted", false)
}

func TestHookWalkRecordsShellObservations(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	appendRuffRepair(t, path, "c", root)
	appendShellTurn(t, path, "g", "grep -n foo x.go", root, "Exit code 1\n", true)
	countHookTranscriptTools(root, "s", path, "")
	countHookTranscriptTools(root, "s", path, "")
	got := readHookSession(root, "s").ShellObservations
	if len(got) != 2 || !got[0].Failed || got[0].Form != "ruff format …" || got[1].Failed || got[1].Form != ".venv/bin/ruff format …" {
		t.Fatalf("observations after two walks = %+v, want the failed and the working ruff call once each", got)
	}

	// A call whose result lands after the walk is paired by the next one.
	use, _ := json.Marshal(map[string]any{"type": "assistant", "cwd": root, "message": map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "late", "name": "Bash", "input": map[string]any{"command": "make test"}}},
	}})
	writeAppend(t, path, string(use)+"\n")
	countHookTranscriptTools(root, "s", path, "")
	res, _ := json.Marshal(map[string]any{"type": "user", "cwd": root, "message": map[string]any{
		"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "late", "content": "ok", "is_error": false}},
	}})
	writeAppend(t, path, string(res)+"\n")
	countHookTranscriptTools(root, "s", path, "agent-1")
	got = readHookSession(root, "s").ShellObservations
	if len(got) != 3 || got[2].Intent != "make test" || got[2].Agent != "agent-1" {
		t.Errorf("observations after a split call = %+v, want make test from agent-1 last", got)
	}

	t.Setenv("GRAFT_NO_MICROSKILLS", "1")
	off := filepath.Join(t.TempDir(), "off.jsonl")
	appendRuffRepair(t, off, "o", root)
	countHookTranscriptTools(root, "off", off, "")
	if got := readHookSession(root, "off"); len(got.ShellObservations) != 0 || len(got.ShellCalls) != 0 {
		t.Errorf("session with GRAFT_NO_MICROSKILLS = %+v, want no shell state", got)
	}
}

func writeAppend(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestHookStopLearnsAndValidatesRepair(t *testing.T) {
	root := t.TempDir()
	for i := range 3 {
		id := fmt.Sprint("s", i)
		path := filepath.Join(t.TempDir(), id+".jsonl")
		appendRuffRepair(t, path, id, root)
		handleHookStop(hookInput{"session_id": id, "transcript_path": path, "hook_event_name": "Stop"}, root)
		facts := readMicroskills(root)
		want := microskillCandidate
		if i == 2 {
			want = microskillValidated
		}
		if len(facts) != 1 || facts[0].Status != want || len(facts[0].Evidence) != i+1 {
			t.Fatalf("store after session %d = %+v, want one %s fact with %d evidence", i, facts, want, i+1)
		}
	}
}

func TestMicroskillInjectionEvidence(t *testing.T) {
	fact := validatedFact(".", "ruff format", ".venv/bin/ruff format …")
	fact.Failing = "ruff format …"
	tests := []struct {
		name        string
		observation shellObservation
		want        pushTally
	}{
		{name: "working form", observation: shellObservation{Intent: "ruff format", Form: ".venv/bin/ruff format …", Scope: "."}, want: pushTally{Confirmed: 1}},
		{name: "failing form anyway", observation: shellObservation{Intent: "ruff format", Form: "ruff format …", Scope: ".", Failed: true, Error: "command not found"}, want: pushTally{Contradicted: 1}},
		{name: "other intent", observation: shellObservation{Intent: "go test", Form: "go test …", Scope: "."}},
		{name: "other agent", observation: shellObservation{Agent: "sub", Intent: "ruff format", Form: ".venv/bin/ruff format …", Scope: "."}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := emptySessionState()
			addPendingInjection(&session, hookPendingInjection{Channel: pushMicroskills, Fact: fact.ID})
			recordShellObservation(&session, tt.observation, []microskill{fact})
			resolveHookInjections(&session, "", hookMainTurnDecides)
			if got := session.PushTally[pushMicroskills]; got != tt.want {
				t.Errorf("tally after %s = %+v, want %+v", tt.name, got, tt.want)
			}
		})
	}
}

func TestHookSessionStartInjectsFacts(t *testing.T) {
	root := orientedHookRepo(t)
	rootFact := validatedFact(".", "ruff format", ".venv/bin/ruff format …")
	billing := validatedFact("services/billing", "go test", "go test -count=1 …")
	writeFixtureFile(t, root, "services/billing/go.mod", "module billing\n")
	billing.Anchors = microskillAnchors(root, "services/billing")

	if got := runHookEvent(t, root, "session-start", map[string]any{"session_id": "none"}); strings.Contains(got, "Repository facts") {
		t.Fatalf("session-start(no store) = %q, want no facts block", got)
	}
	writeMicroskills(t, root, rootFact, billing)
	got := runHookEvent(t, root, "session-start", map[string]any{"session_id": "s"})
	if !strings.Contains(got, "repo map") || !strings.Contains(got, rootFact.Instruction[:20]) || strings.Contains(got, "services/billing") {
		t.Fatalf("session-start = %q, want orientation then the root fact only", got)
	}
	session := readHookSession(root, "s")
	if !slices.Equal(session.MicroskillsInjected, []string{rootFact.ID}) ||
		!slices.ContainsFunc(session.PendingInjections, func(p hookPendingInjection) bool { return p.Channel == pushMicroskills }) {
		t.Errorf("session after start = %+v, want the root fact injected and pending", session)
	}

	t.Run("channel off", func(t *testing.T) {
		contradictChannel(t, root, hookHostClaude, pushMicroskills)
		if got := runHookEvent(t, root, "session-start", map[string]any{"session_id": "off"}); strings.Contains(got, "Repository facts") {
			t.Errorf("session-start(channel off) = %q, want no facts", got)
		}
	})
}

func TestHookSessionStartFactsDisabled(t *testing.T) {
	root := orientedHookRepo(t)
	writeMicroskills(t, root, validatedFact(".", "ruff format", ".venv/bin/ruff format …"))
	t.Setenv("GRAFT_NO_MICROSKILLS", "1")
	if got := runHookEvent(t, root, "session-start", map[string]any{"session_id": "s"}); strings.Contains(got, "Repository facts") {
		t.Errorf("session-start(GRAFT_NO_MICROSKILLS) = %q, want no facts", got)
	}
}

func TestHookPromptAddsScopedFacts(t *testing.T) {
	turn := newPushTurn(t, "p")
	ask := hookAsk
	hookAsk = func(ctx context.Context, root, dir, prompt, scope string) (graph.AskResult, bool) {
		result, ok := ask(ctx, root, dir, prompt, scope)
		result.Hits[0].Pointer = "services/billing/auth.ts:L1-L4"
		result.Hits[0].SourceHash = prompt
		return result, ok
	}
	billing := validatedFact("services/billing", "go test", "go test -count=1 …")
	writeMicroskills(t, turn.root, billing)
	if got := turn.prompt(); !strings.Contains(got, "services/billing/") {
		t.Fatalf("prompt reaching services/billing = %q, want the billing fact", got)
	}
	var stdout bytes.Buffer
	handleHookPrompt(t.Context(), hookInput{"session_id": "p", "prompt": "how does auth verify work again"}, turn.root, &stdout, io.Discard)
	if strings.Contains(stdout.String(), "Repository facts") {
		t.Errorf("second prompt = %q, want the fact not repeated", stdout.String())
	}
}

func TestHookCodexToolUseObservesShell(t *testing.T) {
	root := indexedHookRepo(t)
	codex := func(id, command string, response any) string {
		var stdout bytes.Buffer
		handleHookToolUse(hookInput{
			"session_id": "c", "transcript_path": "/tmp/rollout-1.jsonl", "cwd": root, "tool_name": "Bash", "tool_use_id": id,
			"tool_input": map[string]any{"command": command}, "tool_response": response,
		}, root, &stdout)
		return stdout.String()
	}
	if got := codex("1", "ruff format .", "Exit code: 127\nWall time: 0 seconds\nOutput:\nzsh: command not found: ruff"); got != "" {
		t.Errorf("codex failing Bash printed %q, want nothing", got)
	}
	if got := codex("2", "grep -rn foo internal/", "internal/graph/x.go:1: foo"); got != "" {
		t.Errorf("codex grep printed %q, want no nudge", got)
	}
	var stdout bytes.Buffer
	handleHookToolUse(hookInput{
		"session_id": "c", "transcript_path": "/tmp/rollout-1.jsonl", "cwd": root, "tool_name": "exec_command", "tool_use_id": "3",
		"tool_input": map[string]any{"cmd": []any{"make", "test"}}, "tool_response": map[string]any{"output": "ok", "exit_code": float64(0)},
	}, root, &stdout)
	got := readHookSession(root, "c").ShellObservations
	if len(got) != 3 || !got[0].Failed || got[0].Intent != "ruff format" || got[1].Failed || got[2].Intent != "make test" {
		t.Errorf("codex observations = %+v, want the failing ruff call, the grep and make test", got)
	}

	stdout.Reset()
	handleHookToolUse(hookInput{"session_id": "cl", "cwd": root, "tool_name": "Bash", "tool_use_id": "x",
		"tool_input": map[string]any{"command": "make test"}, "tool_response": map[string]any{"stdout": "ok"}}, root, &stdout)
	if got := readHookSession(root, "cl").ShellObservations; len(got) != 0 {
		t.Errorf("claude tool hook observations = %+v, want none", got)
	}
}

func TestOpencodeHooks(t *testing.T) {
	root := orientedHookRepo(t)
	opencode := func(event string, input map[string]any) string {
		input["graft_host"] = "opencode"
		return runHookEvent(t, root, event, input)
	}
	if got := opencode("session-start", map[string]any{"session_id": "o1"}); got != "" {
		t.Errorf("opencode session-start without facts = %q, want nothing", got)
	}
	fact := validatedFact(".", "ruff format", ".venv/bin/ruff format …")
	fact.Failing = "ruff format …"
	writeMicroskills(t, root, fact)
	got := opencode("session-start", map[string]any{"session_id": "o2"})
	if !strings.Contains(got, "Repository facts") || strings.Contains(got, "repo map") {
		t.Fatalf("opencode session-start = %q, want the facts block only", got)
	}
	opencode("tool-savings", map[string]any{"session_id": "o2", "tool_name": "bash", "tool_use_id": "t1",
		"tool_input":    map[string]any{"command": "ruff format ."},
		"tool_response": map[string]any{"content": "zsh: command not found: ruff", "isError": true, "exit_code": 127}})
	opencode("stop", map[string]any{"session_id": "o2"})
	ledger := readPushLedger(root, hookHostOpencode).Channels[pushMicroskills]
	if len(ledger.Sessions) != 1 || ledger.Sessions[0].Outcome != -1 {
		t.Errorf("opencode ledger = %+v, want one contradicting session", ledger.Sessions)
	}
	if got := readPushLedger(root, hookHostClaude).Channels[pushMicroskills]; len(got.Sessions) != 0 {
		t.Errorf("claude ledger = %+v, want unchanged", got.Sessions)
	}
}

func TestMicroskillsCommand(t *testing.T) {
	t.Setenv("GRAFT_DIR", "")
	root := t.TempDir()
	validated := validatedFact(".", "ruff format", ".venv/bin/ruff format …")
	validated.Evidence = []microskillEvidence{{Session: "s1", At: "2026-10-31T10:00:00Z", Kind: "repair", Form: "ruff format …", Error: "command not found"}}
	candidate := validatedFact(".", "make lint", "./make lint")
	candidate.Status = microskillCandidate
	writeMicroskills(t, root, validated, candidate)
	microskills := func(args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		if got := run(append([]string{"microskills", root}, args...), &stdout, &stderr); got != 0 {
			t.Fatalf("run(microskills %v) = %d, stderr %q", args, got, stderr.String())
		}
		return stdout.String()
	}
	if got := microskills(); !strings.Contains(got, validated.ID) || !strings.Contains(got, "session s1") || strings.Contains(got, candidate.ID) {
		t.Errorf("microskills() = %q, want the validated fact with its evidence only", got)
	}
	var listed microskillStore
	if err := json.Unmarshal([]byte(microskills("--all", "--json")), &listed); err != nil {
		t.Fatalf("microskills --all --json is not JSON: %v", err)
	}
	if len(listed.Facts) != 2 || listed.Facts[1].Status != microskillCandidate || len(listed.Facts[0].Evidence) != 1 || listed.Facts[0].Metrics.Exercised != 4 {
		t.Errorf("microskills --all --json facts = %+v, want both with status, evidence and metrics", listed.Facts)
	}
}

func TestHookHostNamesOpencode(t *testing.T) {
	tests := []struct {
		name  string
		input hookInput
		want  string
	}{
		{name: "opencode plugin", input: hookInput{"graft_host": "opencode"}, want: hookHostOpencode},
		{name: "unknown graft_host", input: hookInput{"graft_host": "cursor", "transcript_path": "/x/rollout-1.jsonl"}, want: hookHostCodex},
		{name: "codex rollout", input: hookInput{"transcript_path": "/x/rollout-1.jsonl"}, want: hookHostCodex},
		{name: "claude", input: hookInput{"transcript_path": "/x/abc.jsonl"}, want: hookHostClaude},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hookHost(tt.input); got != tt.want {
				t.Errorf("hookHost(%v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
	if got := filepath.Base(pushLedgerPath("/r", hookHostOpencode)); got != "push-ledger-opencode.json" {
		t.Errorf("pushLedgerPath(opencode) = %q, want push-ledger-opencode.json", got)
	}
}

// TestHookCodexCapturedPayloads feeds the PostToolUse payloads Codex 0.166
// sent for a working command, a failure that prints an error line, and a
// silent exit 7: Codex sends only the output text.
func TestHookCodexCapturedPayloads(t *testing.T) {
	root := t.TempDir()
	for _, tt := range []struct {
		fixture    string
		wantFailed bool
	}{
		{fixture: "success", wantFailed: false},
		{fixture: "failure-error-line", wantFailed: true},
		{fixture: "failure-without-error-line", wantFailed: false},
	} {
		t.Run(tt.fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "codex-post-tool-use", tt.fixture+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var input hookInput
			if err := json.Unmarshal(data, &input); err != nil {
				t.Fatal(err)
			}
			id := "codex-" + tt.fixture
			input["session_id"], input["cwd"] = id, root
			var stdout bytes.Buffer
			handleHookToolUse(input, root, &stdout)
			got := readHookSession(root, id).ShellObservations
			if stdout.Len() != 0 || len(got) != 1 || got[0].Intent != "python" || got[0].Failed != tt.wantFailed {
				t.Errorf("codex %s: stdout %q, observations %+v, want one python call with failed=%v", tt.fixture, stdout.String(), got, tt.wantFailed)
			}
		})
	}
}
