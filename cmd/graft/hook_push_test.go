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
	"github.com/h0rn3t/Graft/internal/savings"
)

func TestPushLedgerSupport(t *testing.T) {
	confirm, contradict := pushOutcome{ID: "c", Outcome: 1}, pushOutcome{ID: "x", Outcome: -1}
	for _, tc := range []struct {
		name     string
		sessions []pushOutcome
		want     float64
	}{
		{"empty", nil, 0},
		{"one confirm", []pushOutcome{confirm}, 1},
		{"one contradiction", []pushOutcome{contradict}, -1},
		{"recent contradictions outweigh", []pushOutcome{confirm, confirm, contradict, contradict}, -0.648},
		{"recent confirmation outweighs", []pushOutcome{contradict, confirm}, 0.2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := (pushChannel{Sessions: tc.sessions}).support(); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("pushChannel{%v}.support() = %v, want %v", tc.sessions, got, tc.want)
			}
		})
	}
}

func TestPushLedgerFailsOpen(t *testing.T) {
	root := t.TempDir()
	if !hookPushAllowed(root, hookHostClaude, "s1", pushRetrieval) {
		t.Error("hookPushAllowed(no ledger) = false, want true")
	}
	writeFixtureFile(t, root, "graft/.cache/push-ledger-claude.json", "{not json")
	if !hookPushAllowed(root, hookHostClaude, "s2", pushRetrieval) {
		t.Error("hookPushAllowed(corrupt ledger) = false, want true")
	}
	data, err := os.ReadFile(pushLedgerPath(root, hookHostClaude))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Errorf("ledger after a prompt = %q, want valid JSON", data)
	}
}

// recordRetrievalOutcome records session id's retrieval tally.
func recordRetrievalOutcome(root, host, id string, confirmed, contradicted int) {
	recordPushOutcomes(root, host, id, map[string]pushTally{pushRetrieval: {Confirmed: confirmed, Contradicted: contradicted}})
}

// negativeLedger writes a ledger whose support is -1.8 (two contradictions).
func negativeLedger(t *testing.T, root string) {
	t.Helper()
	recordRetrievalOutcome(root, hookHostClaude, "bad1", 0, 1)
	recordRetrievalOutcome(root, hookHostClaude, "bad2", 0, 2)
	if got := readPushLedger(root, hookHostClaude).Channels[pushRetrieval].support(); got >= 0 {
		t.Fatalf("support after two contradicting sessions = %v, want negative", got)
	}
}

func TestPushReauditEveryFifthNewSession(t *testing.T) {
	root := t.TempDir()
	negativeLedger(t, root)
	for i := 1; i <= 4; i++ {
		if id := fmt.Sprintf("new%d", i); hookPushAllowed(root, hookHostClaude, id, pushRetrieval) {
			t.Errorf("hookPushAllowed(%s, negative support) = true, want false", id)
		}
	}
	if hookPushAllowed(root, hookHostClaude, "new1", pushRetrieval) {
		t.Error("hookPushAllowed(resumed new1) = true, want false")
	}
	if got := readPushLedger(root, hookHostClaude).Channels[pushRetrieval].NewSince; got != 4 {
		t.Errorf("newSince after four new sessions and a resume = %d, want 4", got)
	}
	if !hookPushAllowed(root, hookHostClaude, "audit", pushRetrieval) {
		t.Fatal("hookPushAllowed(fifth new session) = false, want a re-audit push")
	}
	if !hookPushAllowed(root, hookHostClaude, "audit", pushRetrieval) {
		t.Error("hookPushAllowed(re-audit session, second prompt) = false, want true")
	}
	if hookPushAllowed(root, hookHostClaude, "new6", pushRetrieval) {
		t.Error("hookPushAllowed(sixth new session) = true, want false")
	}

	// One confirming re-audit leaves -1.44+1; two more lift support above zero.
	recordRetrievalOutcome(root, hookHostClaude, "audit", 1, 0)
	recordRetrievalOutcome(root, hookHostClaude, "good1", 1, 0)
	recordRetrievalOutcome(root, hookHostClaude, "good2", 1, 0)
	if !hookPushAllowed(root, hookHostClaude, "new7", pushRetrieval) || !hookPushAllowed(root, hookHostClaude, "new2", pushRetrieval) {
		t.Error("hookPushAllowed(after support recovered) = false, want true for every session")
	}
	if ledger := readPushLedger(root, hookHostClaude).Channels[pushRetrieval]; ledger.NewSince != 0 || len(ledger.Audit) != 0 {
		t.Errorf("ledger after recovery = newSince %d, audit %v; want both reset", ledger.NewSince, ledger.Audit)
	}
}

func TestRecordPushOutcome(t *testing.T) {
	root := t.TempDir()
	recordRetrievalOutcome(root, hookHostClaude, "neutral", 0, 0)
	if got := readPushLedger(root, hookHostClaude).Channels[pushRetrieval].Sessions; len(got) != 0 {
		t.Errorf("recordPushOutcome(neutral) sessions = %v, want none", got)
	}
	recordRetrievalOutcome(root, hookHostClaude, "s1", 3, 1)
	recordRetrievalOutcome(root, hookHostClaude, "s1", 3, 3)
	if got := readPushLedger(root, hookHostClaude).Channels[pushRetrieval].Sessions; !slices.Equal(got, []pushOutcome{{ID: "s1", Outcome: 1}}) {
		t.Errorf("recordPushOutcome(3 confirmed, then 3 of each) = %v, want one confirming outcome", got)
	}
	recordRetrievalOutcome(root, hookHostClaude, "s1", 3, 4)
	if got := readPushLedger(root, hookHostClaude).Channels[pushRetrieval].Sessions; !slices.Equal(got, []pushOutcome{{ID: "s1", Outcome: -1}}) {
		t.Errorf("recordPushOutcome(contradicted overtakes) = %v, want the one outcome flipped", got)
	}
	for i := range pushLedgerSessions + 3 {
		recordRetrievalOutcome(root, hookHostClaude, fmt.Sprintf("many%d", i), 1, 0)
	}
	if got := readPushLedger(root, hookHostClaude).Channels[pushRetrieval].Sessions; len(got) != pushLedgerSessions || got[len(got)-1].ID != fmt.Sprintf("many%d", pushLedgerSessions+2) {
		t.Errorf("ledger after %d sessions holds %d, newest %q; want the newest %d", pushLedgerSessions+4, len(got), got[len(got)-1].ID, pushLedgerSessions)
	}
}

func TestRecordPushOutcomeConcurrentSessions(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { recordRetrievalOutcome(root, hookHostClaude, fmt.Sprintf("s%d", i), 1, 0) })
	}
	wg.Wait()
	if got := readPushLedger(root, hookHostClaude).Channels[pushRetrieval].Sessions; len(got) != 8 {
		t.Errorf("ledger after 8 concurrent sessions = %v, want 8 outcomes", got)
	}
}

func TestHookHost(t *testing.T) {
	for _, tc := range []struct {
		transcript, want string
	}{
		{"/home/u/.codex/sessions/2026/04/07/rollout-2026-04-07T22-09-42-019d.jsonl", hookHostCodex},
		{"/home/u/.claude/projects/-repo/2348b5c6.jsonl", hookHostClaude},
		{"", hookHostClaude},
	} {
		if got := hookHost(hookInput{"transcript_path": tc.transcript}); got != tc.want {
			t.Errorf("hookHost(transcript %q) = %q, want %q", tc.transcript, got, tc.want)
		}
	}
}

func TestPushLedgerIsPerHost(t *testing.T) {
	root := t.TempDir()
	negativeLedger(t, root)
	if hookPushAllowed(root, hookHostClaude, "claude-new", pushRetrieval) {
		t.Error("hookPushAllowed(claude, negative claude support) = true, want false")
	}
	if !hookPushAllowed(root, hookHostCodex, "codex-new", pushRetrieval) {
		t.Error("hookPushAllowed(codex, negative claude support) = false, want the codex ledger to push")
	}
	if got := readPushLedger(root, hookHostCodex).Channels[pushRetrieval]; len(got.Sessions) != 0 || got.NewSince != 0 {
		t.Errorf("codex ledger after claude outcomes = %+v, want no sessions and no re-audit count", got)
	}
}

func TestContainsWord(t *testing.T) {
	for _, tc := range []struct {
		s, word string
		want    bool
	}{
		{"/repo/src/auth.ts", "src/auth.ts", true},
		{"src/auth.ts:L1-L4", "src/auth.ts", true},
		{"internal/data.go", "a.go", false},
		{"src/auth.tsx", "src/auth.ts", false},
		{"call verify() first", "verify", true},
		{"verifyAll", "verify", false},
		{"a.go then b/a.go", "a.go", true},
		{"проверка verify", "verify", true},
		{"", "verify", false},
		{"verify", "", false},
	} {
		if got := containsWord(tc.s, tc.word); got != tc.want {
			t.Errorf("containsWord(%q, %q) = %t, want %t", tc.s, tc.word, got, tc.want)
		}
	}
}

func TestHookRetrievalSymbols(t *testing.T) {
	for _, tc := range []struct {
		title string
		want  []string
	}{
		{"verify · function", []string{"verify"}},
		{"Server.Handle · method", []string{"Server.Handle", "Handle"}},
		{"", nil},
	} {
		if got := hookRetrievalSymbols(tc.title); !slices.Equal(got, tc.want) {
			t.Errorf("hookRetrievalSymbols(%q) = %v, want %v", tc.title, got, tc.want)
		}
	}
}

func TestPendingRetrievalIsBounded(t *testing.T) {
	session := &sessionState{PerAgentQuery: map[string]string{}, InjectedPointers: []string{}}
	for i := range pendingInjectionLimit + 1 {
		result := graph.AskResult{Mode: "structural", Hits: []graph.AskHit{{Title: fmt.Sprintf("fn%d", i), Pointer: fmt.Sprintf("f%d.go:L1", i)}}}
		if relevantHookRetrieval(&result, session, 3, "") == "" {
			t.Fatalf("relevantHookRetrieval(hit %d) = empty, want retrieval", i)
		}
	}
	if got := session.PendingInjections; len(got) != pendingInjectionLimit || got[0].Files[0] != "f1.go" {
		t.Errorf("pending after %d injections = %d, oldest %v; want %d, oldest f1.go", pendingInjectionLimit+1, len(got), got[0].Files, pendingInjectionLimit)
	}
}

// pushTurn drives one prompt and turn end of session id: the prompt injects
// a hit for src/auth.ts (symbol verify, baseline 4,000 tokens), then the
// assistant runs tools and replies with reply.
type pushTurn struct {
	t          *testing.T
	root, path string
	id         string
}

func newPushTurn(t *testing.T, id string) *pushTurn {
	t.Helper()
	t.Setenv("GRAFT_DIR", "")
	ask := hookAsk
	t.Cleanup(func() { hookAsk = ask })
	hookAsk = func(context.Context, string, string, string, string) (graph.AskResult, bool) {
		return graph.AskResult{
			Query: "auth verify", Mode: "structural",
			Hits:  []graph.AskHit{{Kind: "symbol", Title: "verify · function", Pointer: "src/auth.ts:L1-L4", Score: 1}},
			Saved: &graph.AskSavings{BaselineChars: 16_000},
		}, true
	}
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), id+".jsonl")
	writeFixtureFile(t, filepath.Dir(path), filepath.Base(path), "")
	return &pushTurn{t: t, root: root, path: path, id: id}
}

func (turn *pushTurn) prompt() string {
	turn.t.Helper()
	var stdout bytes.Buffer
	handleHookPrompt(turn.t.Context(), hookInput{"session_id": turn.id, "prompt": "how does auth verify work"}, turn.root, &stdout, io.Discard)
	return stdout.String()
}

// tools appends an assistant entry calling each name with input, then a
// text reply.
func (turn *pushTurn) tools(reply string, calls ...map[string]any) {
	turn.t.Helper()
	var lines []string
	for i, call := range calls {
		data, err := json.Marshal(map[string]any{"type": "assistant", "uuid": fmt.Sprintf("t%d", i), "message": map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": fmt.Sprint(i), "name": call["name"], "input": call["input"]}},
		}})
		if err != nil {
			turn.t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	data, _ := json.Marshal(map[string]any{"type": "assistant", "uuid": "reply", "message": map[string]any{
		"role": "assistant", "content": []any{map[string]any{"type": "text", "text": reply}},
	}})
	lines = append(lines, string(data))
	file, err := os.OpenFile(turn.path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		turn.t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		turn.t.Fatal(err)
	}
}

func (turn *pushTurn) stop() sessionState {
	turn.t.Helper()
	handleHookStop(hookInput{"session_id": turn.id, "transcript_path": turn.path, "hook_event_name": "Stop"}, turn.root)
	return readHookSession(turn.root, turn.id)
}

func call(name string, input map[string]any) map[string]any {
	return map[string]any{"name": name, "input": input}
}

func TestHookRetrievalResolution(t *testing.T) {
	for _, tc := range []struct {
		name          string
		reply         string
		calls         []map[string]any
		wantConfirmed int
		wantContra    int
		wantOutcome   []pushOutcome
	}{
		{"read the injected file", "fixed", []map[string]any{call("Read", map[string]any{"file_path": "/repo/src/auth.ts"})}, 1, 0, []pushOutcome{{ID: "s", Outcome: 1}}},
		{"edit through bash", "done", []map[string]any{call("Bash", map[string]any{"command": "sed -i s/a/b/ src/auth.ts"})}, 1, 0, []pushOutcome{{ID: "s", Outcome: 1}}},
		{"graft read of the symbol", "done", []map[string]any{call("mcp__graft__graft_read_symbol", map[string]any{"symbol": "verify"})}, 1, 0, []pushOutcome{{ID: "s", Outcome: 1}}},
		{"grep for the symbol name is not graft", "done", []map[string]any{call("Grep", map[string]any{"pattern": "verify", "path": "lib"})}, 0, 1, []pushOutcome{{ID: "s", Outcome: -1}}},
		{"explored elsewhere", "done", []map[string]any{call("Read", map[string]any{"file_path": "/repo/src/other.ts"})}, 0, 1, []pushOutcome{{ID: "s", Outcome: -1}}},
		{"reply names the symbol", "verify checks the signature", nil, 1, 0, []pushOutcome{{ID: "s", Outcome: 1}}},
		{"reply names the symbol after other reads", "see verify", []map[string]any{call("Read", map[string]any{"file_path": "/repo/README.md"})}, 1, 0, []pushOutcome{{ID: "s", Outcome: 1}}},
		{"no evidence", "sure", nil, 0, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			turn := newPushTurn(t, "s")
			if turn.prompt() == "" {
				t.Fatal("prompt hook injected nothing, want retrieval")
			}
			if got := readHookSession(turn.root, "s"); got.SavedTokens != 0 || len(got.PendingInjections) != 1 {
				t.Fatalf("after injection saved = %d, pending = %d; want 0 and 1", got.SavedTokens, len(got.PendingInjections))
			}
			turn.tools(tc.reply, tc.calls...)
			got := turn.stop()
			if got.PushTally[pushRetrieval].Confirmed != tc.wantConfirmed || got.PushTally[pushRetrieval].Contradicted != tc.wantContra || len(got.PendingInjections) != 0 {
				t.Errorf("after turn end confirmed %d, contradicted %d, pending %d; want %d, %d, 0",
					got.PushTally[pushRetrieval].Confirmed, got.PushTally[pushRetrieval].Contradicted, len(got.PendingInjections), tc.wantConfirmed, tc.wantContra)
			}
			if wantSaved := tc.wantConfirmed * 4_000; got.SavedTokens != wantSaved {
				t.Errorf("savedTokens after turn end = %d, want %d", got.SavedTokens, wantSaved)
			}
			if ledger := readPushLedger(turn.root, hookHostClaude).Channels[pushRetrieval].Sessions; !slices.Equal(ledger, tc.wantOutcome) {
				t.Errorf("ledger sessions = %v, want %v", ledger, tc.wantOutcome)
			}
			if again := turn.stop(); again.PushTally[pushRetrieval].Confirmed != got.PushTally[pushRetrieval].Confirmed || again.PushTally[pushRetrieval].Contradicted != got.PushTally[pushRetrieval].Contradicted || again.SavedTokens != got.SavedTokens {
				t.Errorf("second turn end changed the session: %+v -> %+v", got, again)
			}
		})
	}
}

func TestHookSubagentReadConfirmsAtParentStop(t *testing.T) {
	turn := newPushTurn(t, "s")
	turn.prompt()
	agent := filepath.Join(t.TempDir(), "agent.jsonl")
	writeFixtureFile(t, filepath.Dir(agent), filepath.Base(agent),
		`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"/repo/src/auth.ts"}}]}}`+"\n")
	handleHookStop(hookInput{"session_id": "s", "transcript_path": turn.path, "agent_transcript_path": agent, "hook_event_name": "SubagentStop"}, turn.root)
	if got := readHookSession(turn.root, "s"); got.PushTally[pushRetrieval].Confirmed != 0 || len(got.PendingInjections) != 1 || !got.PendingInjections[0].Touched {
		t.Fatalf("after SubagentStop confirmed %d, pending %v; want unresolved and touched", got.PushTally[pushRetrieval].Confirmed, got.PendingInjections)
	}
	turn.tools("done")
	if got := turn.stop(); got.PushTally[pushRetrieval].Confirmed != 1 {
		t.Errorf("after the parent's turn end confirmed = %d, want 1", got.PushTally[pushRetrieval].Confirmed)
	}
}

func TestHookPromptRespectsPushDecision(t *testing.T) {
	turn := newPushTurn(t, "fresh")
	negativeLedger(t, turn.root)
	asked := false
	hookAsk = func(context.Context, string, string, string, string) (graph.AskResult, bool) {
		asked = true
		return graph.AskResult{}, false
	}
	if got := turn.prompt(); got != "" || asked {
		t.Errorf("prompt hook with negative support = (%q, asked %t), want nothing and no query", got, asked)
	}
	if got := readHookSession(turn.root, "fresh"); len(got.InjectedPointers) != 0 || got.InjectedTokens != 0 {
		t.Errorf("session after a suppressed prompt = %+v, want nothing recorded", got)
	}
}

func TestHookInjectedTokensAreCharged(t *testing.T) {
	turn := newPushTurn(t, "s")
	text := turn.prompt()
	var payload struct {
		HookSpecificOutput struct{ AdditionalContext string } `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("prompt output %q: %v", text, err)
	}
	if got, want := readHookSession(turn.root, "s").InjectedTokens, hookContextTokens(payload.HookSpecificOutput.AdditionalContext); got != want || want == 0 {
		t.Errorf("injectedTokens after a prompt = %d, want %d", got, want)
	}

	root := t.TempDir()
	var stdout bytes.Buffer
	emitChargedHookContext(&stdout, root, "c", "SessionStart", strings.Repeat("x", 400), nil)
	if got := readHookSession(root, "c").InjectedTokens; got != savings.Tokens(400) {
		t.Errorf("injectedTokens after a 400-char note = %d, want %d", got, savings.Tokens(400))
	}
	big := strings.Repeat("y", hookContextMaxChars*2)
	emitChargedHookContext(io.Discard, root, "big", "SessionStart", big, nil)
	if got, want := readHookSession(root, "big").InjectedTokens, hookContextTokens(big); got != want || want >= savings.Tokens(len(big)) {
		t.Errorf("injectedTokens for a clipped note = %d, want %d (the clipped size)", got, want)
	}
}

func TestHookSearchNudgesAreCharged(t *testing.T) {
	root := indexedHookRepo(t)
	note := runToolSavingsHook(t, root, "claude", "Bash", map[string]any{"command": `grep -rn "fooBar" .`})
	if note == "" {
		t.Fatal("tool-savings(recursive grep) emitted no nudge")
	}
	if got, want := readHookSession(root, "claude").InjectedTokens, hookContextTokens(note); got != want {
		t.Errorf("injectedTokens after a search nudge = %d, want %d", got, want)
	}

	var stdout bytes.Buffer
	handleHookCursorPostTool(hookInput{"session_id": "cursor", "cwd": root, "tool_name": "Shell", "tool_input": map[string]any{"command": `grep -rn "fooBar" .`}}, root, &stdout)
	var payload struct {
		AdditionalContext string `json:"additional_context"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil || payload.AdditionalContext == "" {
		t.Fatalf("cursor-post-tool(recursive grep) stdout = %q, want a nudge (%v)", stdout.String(), err)
	}
	if got, want := readHookSession(root, "cursor").InjectedTokens, savings.Tokens(savings.Length(payload.AdditionalContext)); got != want {
		t.Errorf("injectedTokens after a Cursor nudge = %d, want %d", got, want)
	}
}

func TestMCPIgnoresPushLedger(t *testing.T) {
	t.Setenv("GRAFT_DIR", "")
	t.Setenv("GRAFT_NO_REFRESH", "1")
	root := t.TempDir()
	writeFixtureFile(t, root, "main.go", "package main\n\nfunc main() { helper() }\n\nfunc helper() {}\n")
	if status := runBuild(callersOptions{command: "build", root: root, rootSet: true}, io.Discard, io.Discard); status != 0 {
		t.Fatalf("runBuild(%q) status = %d, want 0", root, status)
	}
	dir := hookContextDir(root)
	args := map[string]any{"query": "helper function"}
	before := mcpCall(t.Context(), root, dir, "", "graft_find_code", args)
	negativeLedger(t, root)
	after := mcpCall(t.Context(), root, dir, "", "graft_find_code", args)
	if before.isError || after.text != before.text {
		t.Errorf("mcpCall(graft_find_code) with negative support = %q, want the answer without a ledger %q", after.text, before.text)
	}
}
