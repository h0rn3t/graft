package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// loopCall runs the tool hook for one Bash call of agent printing output, and
// returns what the hook emitted.
func loopCall(t *testing.T, root, agent, callID, output string) string {
	t.Helper()
	var stdout bytes.Buffer
	handleHookToolUse(hookInput{
		"session_id": "s", "hook_event_name": "PostToolUse", "tool_name": "Bash", "agent_id": agent, "tool_use_id": callID,
		"tool_input":    map[string]any{"command": "go test ./pkg"},
		"tool_response": map[string]any{"stdout": output, "stderr": "", "interrupted": false},
	}, root, &stdout)
	return stdout.String()
}

// loopRoot is a repository without a graph, so the tool hook never nudges.
func loopRoot(t *testing.T) string {
	t.Helper()
	t.Setenv("GRAFT_DIR", "")
	return t.TempDir()
}

func loopPending(root string) []hookPendingInjection {
	var loops []hookPendingInjection
	for _, pending := range readHookSession(root, "s").PendingInjections {
		if pending.Channel == pushLoop {
			loops = append(loops, pending)
		}
	}
	return loops
}

func TestHookLoopNoteOnThirdIdenticalCall(t *testing.T) {
	root := loopRoot(t)
	var notes []string
	for i := range 3 {
		if out := loopCall(t, root, "", strconv.Itoa(i), "FAIL: TestX"); out != "" {
			notes = append(notes, strconv.Itoa(i))
		}
	}
	if len(notes) != 1 || notes[0] != "2" {
		t.Fatalf("loop notes over three identical calls came at %v, want only the third", notes)
	}
	pending := loopPending(root)
	if len(pending) != 1 || pending[0].After != "2" || pending[0].Signature == "" {
		t.Fatalf("pending loop notes = %+v, want one after call 2 with its signature", pending)
	}
	if got := readHookSession(root, "s").InjectedTokens; got != hookContextTokens(hookLoopNote) {
		t.Errorf("injectedTokens = %d, want the note's %d", got, hookContextTokens(hookLoopNote))
	}
	if out := loopCall(t, root, "", "3", "FAIL: TestX"); out != "" {
		t.Errorf("fourth identical call emitted %q, want nothing", out)
	}
	if pending := loopPending(root); len(pending) != 1 || !pending[0].Repeated {
		t.Errorf("pending loop note after a fourth repeat = %+v, want it marked repeated", pending)
	}
	data, err := os.ReadFile(hookSessionPath(root, "s"))
	if err != nil {
		t.Fatal(err)
	}
	if text := string(data); strings.Contains(text, "go test") || strings.Contains(text, "FAIL: TestX") {
		t.Errorf("session file holds the command or its output: %s", text)
	}
}

func TestHookLoopNoNote(t *testing.T) {
	for _, tc := range []struct {
		name  string
		calls [][2]string // agent, output
	}{
		{"outputs differ", [][2]string{{"", "a"}, {"", "b"}, {"", "c"}}},
		{"agents do not share counts", [][2]string{{"", "x"}, {"a1", "x"}, {"", "x"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := loopRoot(t)
			for i, call := range tc.calls {
				if out := loopCall(t, root, call[0], strconv.Itoa(i), call[1]); out != "" {
					t.Errorf("call %d %v emitted %q, want nothing", i, call, out)
				}
			}
		})
	}
}

func TestHookLoopSessionCap(t *testing.T) {
	root := loopRoot(t)
	emitted := 0
	for _, output := range []string{"one", "two", "three"} {
		for i := range loopRepeatThreshold {
			if loopCall(t, root, "", output+strconv.Itoa(i), output) != "" {
				emitted++
			}
		}
	}
	if emitted != loopNoteLimit {
		t.Errorf("loop notes over three looping signatures = %d, want %d", emitted, loopNoteLimit)
	}
}

func TestHookLoopRespectsPush(t *testing.T) {
	root := loopRoot(t)
	contradictChannel(t, root, hookHostClaude, pushLoop)
	for i := range 3 {
		if out := loopCall(t, root, "", strconv.Itoa(i), "same"); out != "" {
			t.Fatalf("call %d with the loop channel off emitted %q", i, out)
		}
	}
	if session := readHookSession(root, "s"); session.InjectedTokens != 0 || session.LoopNotes != 0 || len(session.LoopNoted) != 0 {
		t.Fatalf("session after a gated note = %d injected, %d notes, noted %v; want nothing spent", session.InjectedTokens, session.LoopNotes, session.LoopNoted)
	}
	for _, id := range []string{"good1", "good2"} {
		recordPushOutcomes(root, hookHostClaude, id, map[string]pushTally{pushLoop: {Confirmed: 1}}, nil)
	}
	if out := loopCall(t, root, "", "3", "same"); out == "" {
		t.Errorf("repeat with the loop channel back on emitted nothing, want the note")
	}
}

func TestHookLoopEvidence(t *testing.T) {
	note := hookPendingInjection{Channel: pushLoop, After: "n", Signature: "h"}
	noteCall := toolCall{"n", "Bash", map[string]any{"command": "go test"}}
	read := toolCall{"r", "Read", map[string]any{"file_path": "/repo/a.go"}}
	repeated := note
	repeated.Repeated = true
	for _, tc := range []struct {
		name    string
		pending hookPendingInjection
		agent   string
		calls   []toolCall
		want    pushTally
	}{
		{"agent changes approach", note, "", []toolCall{noteCall, read}, pushTally{Confirmed: 1}},
		{"agent repeats the call", repeated, "", []toolCall{noteCall, noteCall}, pushTally{Contradicted: 1}},
		{"turn ends after the note", note, "", []toolCall{noteCall}, pushTally{}},
		{"another agent's calls", note, "a1", []toolCall{noteCall, read}, pushTally{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := indexedHookRepo(t)
			if err := writeHookSession(root, "s", sessionState{PendingInjections: []hookPendingInjection{tc.pending}}); err != nil {
				t.Fatal(err)
			}
			transcript := filepath.Join(t.TempDir(), "t.jsonl")
			appendToolCalls(t, transcript, tc.calls...)
			countHookTranscriptTools(root, "s", transcript, tc.agent)
			got := stopSession(root, transcript)
			if len(got.PendingInjections) != 0 || got.PushTally[pushLoop] != tc.want {
				t.Errorf("after %v pending %v, loop tally %+v; want none and %+v", tc.calls, got.PendingInjections, got.PushTally[pushLoop], tc.want)
			}
		})
	}
}
