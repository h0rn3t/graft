package main

import (
	"crypto/sha256"
	"encoding/hex"
	jsonv2 "encoding/json/v2"
	"io"
	"slices"
)

// An agent that gets the same result from the same Bash or Grep call for the
// loopRepeatThreshold-th time is told it is repeating itself, at most once
// per call signature and loopNoteLimit times per session.
const (
	loopSignatureLimit  = 32
	loopRepeatThreshold = 3
	loopNoteLimit       = 2
)

const hookLoopNote = "[graft] This call returned the same result as the last two identical calls. " +
	"Repeating it will not change the outcome: read the output for the actual cause, check the assumption behind the call, " +
	"or try a different approach. graft_find_code and graft_trace_calls can locate the code involved."

// hookLoopSignature hashes a Bash or Grep call together with its result, so
// a rerun whose output changed is a new signature. Other tools have none.
func hookLoopSignature(input hookInput) (string, bool) {
	name := input.string("tool_name")
	if name != "Bash" && name != "Grep" {
		return "", false
	}
	data, err := jsonv2.Marshal([]any{name, input["tool_input"], input["tool_response"]}, jsonv2.Deterministic(true))
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8]), true
}

// hookLoopGuard records the call's signature for its agent, marks a loop
// note on the same signature as repeated, and, when the signature reaches
// its third occurrence and the loop channel allows it, emits the loop note.
// emit is false when the hook already wrote other context for this call; the
// note's allowance is then left for a later repeat.
func hookLoopGuard(input hookInput, root string, stdout io.Writer, emit bool) {
	hash, ok := hookLoopSignature(input)
	if !ok {
		return
	}
	id, agent := hookSessionID(input), input.string("agent_id")
	entry := agent + "\x00" + hash
	candidate := false
	_ = updateHookSession(root, id, func(session *sessionState) bool {
		session.LoopSignatures = keepLast(append(session.LoopSignatures, entry), loopSignatureLimit)
		for i := range session.PendingInjections {
			pending := &session.PendingInjections[i]
			if pending.Channel == pushLoop && pending.Agent == agent && pending.Signature == hash {
				pending.Repeated = true
			}
		}
		occurrences := 0
		for _, signature := range session.LoopSignatures {
			if signature == entry {
				occurrences++
			}
		}
		candidate = occurrences >= loopRepeatThreshold && !slices.Contains(session.LoopNoted, hash) && session.LoopNotes < loopNoteLimit
		return true
	})
	if !candidate || !emit || !hookPushAllowed(root, hookHost(input), id, pushLoop) {
		return
	}
	noted := false
	_ = updateHookSession(root, id, func(session *sessionState) bool {
		if slices.Contains(session.LoopNoted, hash) || session.LoopNotes >= loopNoteLimit {
			return false
		}
		session.LoopNoted = append(session.LoopNoted, hash)
		session.LoopNotes++
		session.InjectedTokens += hookContextTokens(hookLoopNote)
		addPendingInjection(session, hookPendingInjection{Channel: pushLoop, Agent: agent, After: input.string("tool_use_id"), Signature: hash})
		noted = true
		return true
	})
	if noted {
		emitHookContext(stdout, "PostToolUse", hookLoopNote)
	}
}
