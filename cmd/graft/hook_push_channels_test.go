package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// contradictChannel records two contradicting sessions in a channel, which
// leaves its support at -1.8.
func contradictChannel(t *testing.T, root, host, channel string) {
	t.Helper()
	for _, id := range []string{"bad1-" + channel, "bad2-" + channel} {
		recordPushOutcomes(root, host, id, map[string]pushTally{channel: {Contradicted: 1}}, nil)
	}
	if got := readPushLedger(root, host).Channels[channel].support(); got >= 0 {
		t.Fatalf("%s support after two contradicting sessions = %v, want negative", channel, got)
	}
}

func TestPushChannelsAreIndependent(t *testing.T) {
	root := t.TempDir()
	contradictChannel(t, root, hookHostClaude, pushBlast)
	for _, channel := range pushChannels {
		want := channel != pushBlast
		if got := hookPushAllowed(root, hookHostClaude, "s", channel); got != want {
			t.Errorf("hookPushAllowed(%s, negative blast support) = %t, want %t", channel, got, want)
		}
	}
}

func TestPushLedgerReadsVersion1(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "graft/.cache/push-ledger-claude.json",
		`{"version":1,"sessions":[{"id":"a","outcome":-1},{"id":"b","outcome":-1}],"seen":["a","b"],"newSince":2}`)
	ledger := readPushLedger(root, hookHostClaude)
	if got := ledger.Channels[pushRetrieval]; math.Abs(got.support()+1.8) > 1e-9 || got.sessionsUntilReaudit() != 3 {
		t.Errorf("retrieval from a v1 ledger = support %v, re-audit in %d; want -1.8 and 3", got.support(), got.sessionsUntilReaudit())
	}
	if got := ledger.Channels[pushBlast].support(); got != 0 {
		t.Errorf("blast support from a v1 ledger = %v, want 0", got)
	}
	if hookPushAllowed(root, hookHostClaude, "c", pushRetrieval) {
		t.Error("hookPushAllowed(retrieval, v1 negative support) = true, want false")
	}
	if !hookPushAllowed(root, hookHostClaude, "d", pushBlast) {
		t.Error("hookPushAllowed(blast, v1 ledger) = false, want true")
	}
	data, err := os.ReadFile(pushLedgerPath(root, hookHostClaude))
	if err != nil {
		t.Fatal(err)
	}
	var written struct {
		Version  int                        `json:"version"`
		Sessions []pushOutcome              `json:"sessions"`
		Channels map[string]json.RawMessage `json:"channels"`
	}
	if err := json.Unmarshal(data, &written); err != nil || written.Version != 2 || written.Sessions != nil || written.Channels[pushRetrieval] == nil {
		t.Errorf("ledger after an update = %s, want version 2 with the retrieval channel and no top-level sessions", data)
	}
	if got := readPushLedger(root, hookHostClaude).Channels[pushRetrieval].NewSince; got != 4 {
		t.Errorf("retrieval newSince after two new sessions = %d, want 4", got)
	}
}

func TestPushLedgerCorruptPushesEveryChannel(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "graft/.cache/push-ledger-claude.json", "{not json")
	for _, channel := range pushChannels {
		if !hookPushAllowed(root, hookHostClaude, "s", channel) {
			t.Errorf("hookPushAllowed(%s, corrupt ledger) = false, want true", channel)
		}
	}
}

func TestPushNewSessionCountsOnceAcrossChannels(t *testing.T) {
	root := t.TempDir()
	contradictChannel(t, root, hookHostClaude, pushRetrieval)
	contradictChannel(t, root, hookHostClaude, pushBlast)

	// Session start decides orientation first; the prompt must not count
	// the session again.
	if !hookPushAllowed(root, hookHostClaude, "s1", pushOrientation) {
		t.Error("hookPushAllowed(s1, orientation) = false, want true")
	}
	if hookPushAllowed(root, hookHostClaude, "s1", pushRetrieval) {
		t.Error("hookPushAllowed(s1, negative retrieval) = true, want false")
	}
	ledger := readPushLedger(root, hookHostClaude)
	if retrieval, blast := ledger.Channels[pushRetrieval].NewSince, ledger.Channels[pushBlast].NewSince; retrieval != 1 || blast != 1 {
		t.Errorf("newSince after one session's two decisions = retrieval %d, blast %d; want 1 and 1", retrieval, blast)
	}

	for i := 2; i <= 5; i++ {
		hookPushAllowed(root, hookHostClaude, fmt.Sprintf("s%d", i), pushOrientation)
	}
	if !hookPushAllowed(root, hookHostClaude, "s5", pushRetrieval) || !hookPushAllowed(root, hookHostClaude, "s5", pushBlast) {
		t.Error("hookPushAllowed(fifth new session) = false for retrieval or blast, want a re-audit in both")
	}

	// Two confirmations lift blast from -1.8 to 0.648; only its counters reset.
	for _, id := range []string{"good1", "good2"} {
		recordPushOutcomes(root, hookHostClaude, id, map[string]pushTally{pushBlast: {Confirmed: 1}}, nil)
	}
	hookPushAllowed(root, hookHostClaude, "s6", pushOrientation)
	ledger = readPushLedger(root, hookHostClaude)
	if blast := ledger.Channels[pushBlast]; blast.NewSince != 0 || len(blast.Audit) != 0 {
		t.Errorf("blast after recovery = newSince %d, audit %v; want both reset", blast.NewSince, blast.Audit)
	}
	if retrieval := ledger.Channels[pushRetrieval]; retrieval.NewSince != 6 || !slices.Equal(retrieval.Audit, []string{"s5"}) {
		t.Errorf("retrieval after blast recovered = newSince %d, audit %v; want 6 and [s5]", retrieval.NewSince, retrieval.Audit)
	}
}

func TestRecordPushOutcomesSeveralChannels(t *testing.T) {
	root := t.TempDir()
	recordPushOutcomes(root, hookHostClaude, "s", map[string]pushTally{
		pushRetrieval: {Confirmed: 1},
		pushBlast:     {Contradicted: 2},
		pushNudge:     {},
	}, nil)
	ledger := readPushLedger(root, hookHostClaude)
	for channel, want := range map[string][]pushOutcome{
		pushRetrieval: {{ID: "s", Outcome: 1}},
		pushBlast:     {{ID: "s", Outcome: -1}},
		pushNudge:     nil,
	} {
		if got := ledger.Channels[channel].Sessions; !slices.Equal(got, want) {
			t.Errorf("%s sessions = %v, want %v", channel, got, want)
		}
	}
}

// toolCall is one tool_use block of a transcript fixture.
type toolCall struct {
	id, name string
	input    map[string]any
}

// appendToolCalls appends one assistant entry per call to the transcript.
func appendToolCalls(t *testing.T, path string, calls ...toolCall) {
	t.Helper()
	var text strings.Builder
	for _, call := range calls {
		data, err := json.Marshal(map[string]any{"type": "assistant", "uuid": "u" + call.id, "message": map[string]any{
			"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": call.id, "name": call.name, "input": call.input}},
		}})
		if err != nil {
			t.Fatal(err)
		}
		text.Write(data)
		text.WriteByte('\n')
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(text.String()); err != nil {
		t.Fatal(err)
	}
}

func TestHookInjectionEvidence(t *testing.T) {
	blast := hookPendingInjection{Channel: pushBlast, Files: []string{"client.ts"}, Symbols: []string{"call"}}
	armedBlast := blast
	blast.After = "edit"
	nudge := hookPendingInjection{Channel: pushNudge}
	orientation := hookPendingInjection{Channel: pushOrientation}
	read := func(id, file string) toolCall {
		return toolCall{id, "Read", map[string]any{"file_path": file}}
	}
	edit := toolCall{"edit", "Edit", map[string]any{"file_path": "/repo/src/auth.ts"}}
	graftCall := toolCall{"g", "mcp__graft__graft_read_symbol", map[string]any{"symbol": "call"}}
	grep := toolCall{"s", "Grep", map[string]any{"pattern": "fooBar"}}
	for _, tc := range []struct {
		name    string
		pending hookPendingInjection
		agent   string
		calls   []toolCall
		want    hookPendingInjection
	}{
		{"read before the arming edit", blast, "", []toolCall{read("r", "/repo/src/client.ts"), edit},
			armedBlast},
		{"read after the arming edit", blast, "", []toolCall{edit, read("r", "/repo/src/client.ts")},
			hookPendingInjection{Channel: pushBlast, Files: blast.Files, Symbols: blast.Symbols, Touched: true, Explored: true}},
		{"graft read of a dependent", armedBlast, "", []toolCall{graftCall},
			hookPendingInjection{Channel: pushBlast, Files: blast.Files, Symbols: blast.Symbols, Touched: true, Explored: true}},
		{"grep for a dependent's name", armedBlast, "", []toolCall{{"s", "Grep", map[string]any{"pattern": "call"}}},
			hookPendingInjection{Channel: pushBlast, Files: blast.Files, Symbols: blast.Symbols, Explored: true}},
		{"a subagent's read", armedBlast, "a1", []toolCall{read("r", "/repo/src/client.ts")},
			armedBlast},
		{"nudge followed by graft", nudge, "", []toolCall{graftCall, grep},
			hookPendingInjection{Channel: pushNudge, First: hookToolGraft}},
		{"nudge followed by grep", nudge, "", []toolCall{grep, graftCall},
			hookPendingInjection{Channel: pushNudge, First: hookRawSearchCall}},
		{"nudge followed by a read", nudge, "", []toolCall{read("r", "/repo/x.go")},
			nudge},
		{"nudge followed by rg", nudge, "", []toolCall{{"b", "Bash", map[string]any{"command": "rg fooBar"}}},
			hookPendingInjection{Channel: pushNudge, First: hookRawSearchCall}},
		{"nudge followed by recursive grep", nudge, "", []toolCall{{"b", "Bash", map[string]any{"command": "grep -rn fooBar ."}}},
			hookPendingInjection{Channel: pushNudge, First: hookRawSearchCall}},
		{"nudge followed by ls", nudge, "", []toolCall{{"b", "Bash", map[string]any{"command": "ls"}}},
			nudge},
		{"orientation takes the first exploration call", orientation, "", []toolCall{edit, grep, graftCall},
			hookPendingInjection{Channel: pushOrientation, First: hookToolSource}},
		{"orientation starts with graft", orientation, "", []toolCall{graftCall, grep},
			hookPendingInjection{Channel: pushOrientation, First: hookToolGraft}},
		{"subagent orientation ignores the main agent", hookPendingInjection{Channel: pushSubagentOrientation, Agent: "a1"}, "", []toolCall{grep},
			hookPendingInjection{Channel: pushSubagentOrientation, Agent: "a1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := indexedHookRepo(t)
			if err := writeHookSession(root, "s", sessionState{PerAgentQuery: map[string]string{}, PendingInjections: []hookPendingInjection{tc.pending}}); err != nil {
				t.Fatal(err)
			}
			transcript := filepath.Join(t.TempDir(), "t.jsonl")
			appendToolCalls(t, transcript, tc.calls...)
			countHookTranscriptTools(root, "s", transcript, tc.agent)
			got := readHookSession(root, "s").PendingInjections
			if len(got) != 1 || fmt.Sprintf("%+v", got[0]) != fmt.Sprintf("%+v", tc.want) {
				t.Errorf("pending after %v = %+v, want [%+v]", tc.calls, got, tc.want)
			}
		})
	}
}

// stopSession runs the main agent's turn end for session s over transcript.
func stopSession(root, transcript string) sessionState {
	handleHookStop(hookInput{"session_id": "s", "transcript_path": transcript, "hook_event_name": "Stop"}, root)
	return readHookSession(root, "s")
}

func TestHookMainStopResolvesChannels(t *testing.T) {
	armedBlast := hookPendingInjection{Channel: pushBlast, Files: []string{"client.ts"}, Symbols: []string{"call"}}
	unarmedBlast := armedBlast
	unarmedBlast.After = "never"
	nudge := hookPendingInjection{Channel: pushNudge}
	read := func(file string) toolCall { return toolCall{"r", "Read", map[string]any{"file_path": file}} }
	graftCall := toolCall{"g", "mcp__graft__graft_find_all", map[string]any{"pattern": "x"}}
	for _, tc := range []struct {
		name    string
		pending hookPendingInjection
		calls   []toolCall
		want    pushTally
		outcome int
	}{
		{"blast dependent read", armedBlast, []toolCall{read("/repo/src/client.ts")}, pushTally{Confirmed: 1}, 1},
		{"blast dependents ignored", armedBlast, []toolCall{read("/repo/src/other.ts")}, pushTally{Contradicted: 1}, -1},
		{"blast never armed", unarmedBlast, []toolCall{read("/repo/src/other.ts")}, pushTally{}, 0},
		{"nudge followed", nudge, []toolCall{graftCall}, pushTally{Confirmed: 1}, 1},
		{"nudge ignored", nudge, []toolCall{{"s", "Grep", map[string]any{"pattern": "fooBar"}}}, pushTally{Contradicted: 1}, -1},
		{"nudge without evidence", nudge, []toolCall{read("/repo/x.go")}, pushTally{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := indexedHookRepo(t)
			if err := writeHookSession(root, "s", sessionState{PerAgentQuery: map[string]string{}, PendingInjections: []hookPendingInjection{tc.pending}}); err != nil {
				t.Fatal(err)
			}
			transcript := filepath.Join(t.TempDir(), "t.jsonl")
			appendToolCalls(t, transcript, tc.calls...)
			got := stopSession(root, transcript)
			if len(got.PendingInjections) != 0 || got.PushTally[tc.pending.Channel] != tc.want {
				t.Errorf("after turn end pending %v, %s tally %+v; want none and %+v", got.PendingInjections, tc.pending.Channel, got.PushTally[tc.pending.Channel], tc.want)
			}
			var want []pushOutcome
			if tc.outcome != 0 {
				want = []pushOutcome{{ID: "s", Outcome: tc.outcome}}
			}
			if ledger := readPushLedger(root, hookHostClaude).Channels[tc.pending.Channel].Sessions; !slices.Equal(ledger, want) {
				t.Errorf("%s ledger sessions = %v, want %v", tc.pending.Channel, ledger, want)
			}
		})
	}
}

func TestHookOrientationWaitsForEvidence(t *testing.T) {
	root := indexedHookRepo(t)
	if err := writeHookSession(root, "s", sessionState{PerAgentQuery: map[string]string{}, PendingInjections: []hookPendingInjection{{Channel: pushOrientation}}}); err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	appendToolCalls(t, transcript, toolCall{"e", "Edit", map[string]any{"file_path": "/repo/a.go"}})
	if got := stopSession(root, transcript); len(got.PendingInjections) != 1 || got.PushTally != nil {
		t.Fatalf("after a turn without exploration pending %v, tally %v; want the orientation still pending", got.PendingInjections, got.PushTally)
	}
	appendToolCalls(t, transcript, toolCall{"s", "Grep", map[string]any{"pattern": "x"}})
	if got := stopSession(root, transcript); len(got.PendingInjections) != 0 || got.PushTally[pushOrientation] != (pushTally{Contradicted: 1}) {
		t.Errorf("after a turn starting with Grep pending %v, tally %v; want the orientation contradicted", got.PendingInjections, got.PushTally)
	}
}

func TestHookSubagentOrientationResolvesAtSubagentStop(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls []toolCall
		want  pushTally
	}{
		{"starts with Grep", []toolCall{{"s", "Grep", map[string]any{"pattern": "x"}}}, pushTally{Contradicted: 1}},
		{"starts with graft", []toolCall{{"g", "mcp__graft__graft_find_code", map[string]any{"query": "x"}}}, pushTally{Confirmed: 1}},
		{"no exploration", []toolCall{{"e", "Edit", map[string]any{"file_path": "/repo/a.go"}}}, pushTally{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := indexedHookRepo(t)
			pending := []hookPendingInjection{{Channel: pushSubagentOrientation, Agent: "a1"}, {Channel: pushSubagentOrientation, Agent: "a2"}}
			if err := writeHookSession(root, "s", sessionState{PerAgentQuery: map[string]string{}, PendingInjections: pending}); err != nil {
				t.Fatal(err)
			}
			main := filepath.Join(t.TempDir(), "main.jsonl")
			appendToolCalls(t, main, toolCall{"m", "mcp__graft__graft_find_code", map[string]any{"query": "x"}})
			if got := stopSession(root, main); len(got.PendingInjections) != 2 {
				t.Fatalf("after the main turn end pending %v, want both subagent orientations", got.PendingInjections)
			}
			agent := filepath.Join(t.TempDir(), "agent.jsonl")
			appendToolCalls(t, agent, tc.calls...)
			handleHookStop(hookInput{"session_id": "s", "transcript_path": main, "agent_transcript_path": agent, "agent_id": "a1", "hook_event_name": "SubagentStop"}, root)
			got := readHookSession(root, "s")
			if len(got.PendingInjections) != 1 || got.PendingInjections[0].Agent != "a2" || got.PushTally[pushSubagentOrientation] != tc.want {
				t.Errorf("after a1 stopped pending %v, tally %+v; want only a2 pending and %+v", got.PendingInjections, got.PushTally[pushSubagentOrientation], tc.want)
			}
		})
	}
}

// orientedHookRepo is an indexed repository with an INDEX.md, so session and
// subagent start have orientation to emit.
func orientedHookRepo(t *testing.T) string {
	t.Helper()
	root := indexedHookRepo(t)
	t.Setenv("CLAUDE_PROJECT_DIR", "")
	writeFixtureFile(t, root, "graft/INDEX.md", "# repo map\n")
	return root
}

func runHookEvent(t *testing.T, root, event string, input map[string]any) string {
	t.Helper()
	input["cwd"] = root
	stdin, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if status := runWithInput([]string{"_hook", event}, bytes.NewReader(stdin), &stdout, &stderr); status != 0 {
		t.Fatalf("runWithInput(_hook %s) status = %d, stderr %q", event, status, stderr.String())
	}
	return stdout.String()
}

func TestHookSessionStartRespectsOrientationPush(t *testing.T) {
	root := orientedHookRepo(t)
	if got := runHookEvent(t, root, "session-start", map[string]any{"session_id": "s"}); !strings.Contains(got, "repo map") {
		t.Fatalf("session-start(push on) = %q, want the orientation", got)
	}
	runHookEvent(t, root, "session-start", map[string]any{"session_id": "s", "source": "resume"})
	if got := readHookSession(root, "s").PendingInjections; len(got) != 1 || got[0].Channel != pushOrientation {
		t.Errorf("pending after start and resume = %+v, want one orientation", got)
	}

	contradictChannel(t, root, hookHostClaude, pushOrientation)
	if got := runHookEvent(t, root, "session-start", map[string]any{"session_id": "off"}); strings.Contains(got, "repo map") {
		t.Errorf("session-start(orientation off) = %q, want no orientation", got)
	}
	if got := readHookSession(root, "off"); len(got.PendingInjections) != 0 {
		t.Errorf("session after a gated start = %+v, want nothing pending", got)
	}
}

func TestHookSubagentStartRespectsPush(t *testing.T) {
	root := orientedHookRepo(t)
	if got := runHookEvent(t, root, "subagent-start", map[string]any{"session_id": "s", "agent_id": "a1"}); !strings.Contains(got, "repo map") {
		t.Fatalf("subagent-start(push on) = %q, want the orientation", got)
	}
	if got := readHookSession(root, "s").PendingInjections; len(got) != 1 || got[0].Channel != pushSubagentOrientation || got[0].Agent != "a1" {
		t.Errorf("pending after subagent start = %+v, want a1's orientation", got)
	}

	contradictChannel(t, root, hookHostClaude, pushSubagentOrientation)
	if got := runHookEvent(t, root, "subagent-start", map[string]any{"session_id": "off", "agent_id": "a1"}); got != "" {
		t.Errorf("subagent-start(off, fresh index) = %q, want nothing", got)
	}
}

func TestHookPostEditRespectsBlastPush(t *testing.T) {
	root := indexedHookRepo(t)
	check := hookCheck
	t.Cleanup(func() { hookCheck = check })
	hookCheck = func(string, string) int { return 1 }
	writeFixtureFile(t, root, "src/auth.ts", "export function verify() {}\n")
	writeFixtureFile(t, root, "graft/.graph/wiring.json", `{"meta":{"nodeCount":2,"edgeCount":1,"languages":["typescript"]},`+
		`"nodes":[{"id":"src/auth.ts","name":"auth.ts","kind":"file","path":"src/auth.ts","span":"L1-L1"},`+
		`{"id":"src/client.ts#call","name":"call","kind":"function","path":"src/client.ts","span":"L1-L1"}],`+
		`"edges":[{"source":"src/client.ts#call","target":"src/auth.ts","relation":"calls","confidence":"extracted"}]}`)
	edit := func(id string) string {
		return runHookEvent(t, root, "post-edit", map[string]any{"session_id": id, "tool_use_id": "e1", "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": filepath.Join(root, "src", "auth.ts")}})
	}
	if got := edit("on"); !strings.Contains(got, "blast radius") {
		t.Fatalf("post-edit(push on) = %q, want a blast radius", got)
	}
	want := hookPendingInjection{Channel: pushBlast, After: "e1", Files: []string{"client.ts"}, Symbols: []string{"call"}}
	if got := readHookSession(root, "on").PendingInjections; len(got) != 1 || fmt.Sprintf("%+v", got[0]) != fmt.Sprintf("%+v", want) {
		t.Errorf("pending after an edit = %+v, want [%+v]", got, want)
	}

	contradictChannel(t, root, hookHostClaude, pushBlast)
	if got := edit("off"); got != "" {
		t.Errorf("post-edit(blast off) = %q, want nothing", got)
	}
	if got := readHookSession(root, "off"); got.InjectedTokens != 0 || len(got.PendingInjections) != 0 {
		t.Errorf("session after a gated edit = %+v, want nothing charged or pending", got)
	}
	if stats := readHookStats(root); stats == nil || !stats.Dirty || stats.LastFile == nil || *stats.LastFile != "src/auth.ts" {
		t.Errorf("stats after a gated edit = %+v, want dirty with last file src/auth.ts", stats)
	}
}

func TestHookSearchNudgeRespectsPush(t *testing.T) {
	root := indexedHookRepo(t)
	runToolSavingsHook(t, root, "s", "Read", map[string]any{"file_path": filepath.Join(root, "x.go")})
	if _, err := os.Stat(pushLedgerPath(root, hookHostClaude)); !os.IsNotExist(err) {
		t.Errorf("ledger after a non-search tool call: stat error %v, want not exist", err)
	}

	contradictChannel(t, root, hookHostClaude, pushNudge)
	grep := map[string]any{"command": `grep -rn "fooBar" .`}
	if got := runToolSavingsHook(t, root, "s", "Bash", grep); got != "" {
		t.Errorf("tool-savings(grep, nudge off) = %q, want nothing", got)
	}
	if got := readHookSession(root, "s").SearchNudges; got != 0 {
		t.Errorf("searchNudges after a gated nudge = %d, want 0", got)
	}

	if err := os.Remove(pushLedgerPath(root, hookHostClaude)); err != nil {
		t.Fatal(err)
	}
	if got := runToolSavingsHook(t, root, "s", "Bash", grep); got == "" {
		t.Error("tool-savings(grep, nudge back on) = empty, want the nudge the gated call did not spend")
	}
	if got := readHookSession(root, "s").PendingInjections; len(got) != 1 || got[0].Channel != pushNudge {
		t.Errorf("pending after a nudge = %+v, want one nudge", got)
	}
}

func TestHookWalkRecordsGraftTools(t *testing.T) {
	root := indexedHookRepo(t)
	main := filepath.Join(t.TempDir(), "main.jsonl")
	appendToolCalls(t, main,
		toolCall{"1", "mcp__graft__graft_find_code", map[string]any{"query": "x"}},
		toolCall{"2", "mcp__graft__graft_find_code", map[string]any{"query": "y"}},
		toolCall{"3", "mcp__graft__graft_map", map[string]any{}},
		toolCall{"4", "Bash", map[string]any{"command": "graft ask auth"}},
		toolCall{"5", "Read", map[string]any{"file_path": "/repo/a.go"}},
	)
	agent := filepath.Join(t.TempDir(), "agent.jsonl")
	appendToolCalls(t, agent, toolCall{"6", "graft_trace_calls", map[string]any{"symbol": "x"}})
	countHookTranscriptTools(root, "s", main, "")
	countHookTranscriptTools(root, "s", agent, "a1")
	want := []string{"graft_find_code", "graft_repo_map", "graft_trace_calls"}
	if got := readHookSession(root, "s").GraftTools; !slices.Equal(got, want) {
		t.Errorf("graftTools after the walks = %v, want %v", got, want)
	}
}

func TestHookStopRecordsGraftTools(t *testing.T) {
	root := indexedHookRepo(t)
	transcript := filepath.Join(t.TempDir(), "t.jsonl")
	appendToolCalls(t, transcript, toolCall{"1", "Read", map[string]any{"file_path": "/repo/a.go"}})
	stopSession(root, transcript)
	if tools := readPushLedger(root, hookHostClaude).Tools; tools != nil {
		t.Fatalf("tool record after a turn without graft calls = %+v, want none", tools)
	}
	appendToolCalls(t, transcript, toolCall{"2", "mcp__graft__graft_find_all", map[string]any{"pattern": "x"}})
	stopSession(root, transcript)
	want := []toolSession{{ID: "s", Used: []string{"graft_find_all"}}}
	if got := readPushLedger(root, hookHostClaude).Tools; got == nil || fmt.Sprint(got.Sessions) != fmt.Sprint(want) {
		t.Fatalf("tool record after a graft call = %+v, want %v", got, want)
	}
	appendToolCalls(t, transcript, toolCall{"3", "mcp__graft__graft_find_code", map[string]any{"query": "x"}})
	stopSession(root, transcript)
	want = []toolSession{{ID: "s", Used: []string{"graft_find_all", "graft_find_code"}}}
	if got := readPushLedger(root, hookHostClaude).Tools; got == nil || fmt.Sprint(got.Sessions) != fmt.Sprint(want) {
		t.Errorf("tool record after a second turn = %+v, want %v", got, want)
	}
}
