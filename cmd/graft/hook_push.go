package main

import (
	jsonv2 "encoding/json/v2"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Each push channel is pushed per repository only while sessions that
// received it go on to use it: each scored session adds +1 when it confirms
// and -pushContradictionWeight when it contradicts, older sessions decaying
// by pushDecay per newer one.
const (
	pushDecay               = 0.8
	pushContradictionWeight = 1.0
	pushReauditEvery        = 5 // while support is negative, every fifth new session pushes again
	pushLedgerSessions      = 32
	pushLedgerSeen          = 64
	pushLedgerAudits        = 8
	pendingInjectionLimit   = 32
)

// The context graft pushes, each gated by its own support.
const (
	pushRetrieval           = "retrieval"
	pushOrientation         = "orientation"
	pushSubagentOrientation = "subagentOrientation"
	pushNudge               = "nudge"
	pushBlast               = "blast"
	pushLoop                = "loop"
	pushMicroskills         = "microskills"
)

var pushChannels = []string{pushRetrieval, pushOrientation, pushSubagentOrientation, pushNudge, pushBlast, pushLoop, pushMicroskills}

// A graft MCP tool is hidden from tools/list once the last
// toolAuditSessions recorded sessions that used graft never called it.
const (
	toolAuditSessions  = 10
	toolLedgerSessions = 20
)

// toolsAlwaysListed are never hidden, whatever the record says.
var toolsAlwaysListed = []string{"graft_find_code", "graft_read_symbol"}

// hookRawSearchCall marks a nudge's first later call as another raw search.
const hookRawSearchCall = "search"

// hookPendingInjection is one pushed context waiting for later tool calls to
// show whether the agent used it.
type hookPendingInjection struct {
	Channel string `json:"channel"`
	// Agent is the subagent's agent_id, "" for the main agent. Retrieval takes
	// evidence from every agent; the other channels only from their own.
	Agent string `json:"agent,omitempty"`
	// After is the tool_use id of the call that produced the injection; calls
	// count as evidence only once it has passed, and it is cleared then.
	After          string   `json:"after,omitempty"`
	Files          []string `json:"files,omitempty"`
	Symbols        []string `json:"symbols,omitempty"`
	BaselineTokens int      `json:"baselineTokens,omitzero"`
	Touched        bool     `json:"touched,omitzero"`
	Explored       bool     `json:"explored,omitzero"`
	// First is the first deciding call: graft or source for orientation,
	// graft or search for a nudge.
	First string `json:"first,omitempty"`
	// Signature is a loop note's call hash; Repeated marks that the same call
	// returned the same result again after the note.
	Signature string `json:"signature,omitempty"`
	Repeated  bool   `json:"repeated,omitzero"`
	// Fact is an injected micro-skill's id; First then holds how the agent's
	// next call of its intent decided it.
	Fact string `json:"fact,omitempty"`
}

// pushTally counts a session's resolved injections in one channel.
type pushTally struct {
	Confirmed    int `json:"confirmed,omitzero"`
	Contradicted int `json:"contradicted,omitzero"`
}

// outcome is the session's single score in the channel: +1, -1, or 0 when
// it resolved only neutral injections.
func (tally pushTally) outcome() int {
	switch {
	case tally.Contradicted > tally.Confirmed:
		return -1
	case tally.Confirmed > 0:
		return 1
	}
	return 0
}

// pushLedger is a repository's record of whether pushed context paid off,
// per channel. It holds session ids and outcomes only, never prompts, paths
// or source.
type pushLedger struct {
	Version  int                     `json:"version"`
	Seen     []string                `json:"seen,omitempty"` // sessions a push decision has already counted
	Channels map[string]*pushChannel `json:"channels,omitempty"`
	// Version 1 kept the retrieval channel at the top level.
	Sessions []pushOutcome `json:"sessions,omitempty"`
	NewSince int           `json:"newSince,omitzero"`
	Audit    []string      `json:"audit,omitempty"`
	Tools    *toolRecord   `json:"tools,omitempty"`
}

// toolRecord is which graft MCP tools recent sessions called, and how many
// MCP server starts have hidden some of them.
type toolRecord struct {
	Sessions []toolSession `json:"sessions,omitempty"` // oldest first
	Starts   int           `json:"starts,omitzero"`
}

type toolSession struct {
	ID   string   `json:"id"`
	Used []string `json:"used"`
}

// recordTools sets session id's set of called tools, keeping the newest
// toolLedgerSessions sessions, and reports whether the ledger changed.
func (ledger *pushLedger) recordTools(id string, used []string) bool {
	if ledger.Tools == nil {
		ledger.Tools = &toolRecord{}
	}
	sessions := ledger.Tools.Sessions
	i := slices.IndexFunc(sessions, func(session toolSession) bool { return session.ID == id })
	switch {
	case i < 0:
		ledger.Tools.Sessions = keepLast(append(sessions, toolSession{ID: id, Used: slices.Clone(used)}), toolLedgerSessions)
	case !slices.Equal(sessions[i].Used, used):
		sessions[i].Used = slices.Clone(used)
	default:
		return false
	}
	return true
}

// hidden returns the tools of catalog that none of the last toolAuditSessions
// recorded sessions called, once that many are recorded; it never hides
// toolsAlwaysListed.
func (ledger *pushLedger) hidden(catalog []string) []string {
	if ledger.Tools == nil || len(ledger.Tools.Sessions) < toolAuditSessions {
		return nil
	}
	recent := ledger.Tools.Sessions[len(ledger.Tools.Sessions)-toolAuditSessions:]
	var hidden []string
	for _, tool := range catalog {
		used := slices.ContainsFunc(recent, func(session toolSession) bool { return slices.Contains(session.Used, tool) })
		if !used && !slices.Contains(toolsAlwaysListed, tool) {
			hidden = append(hidden, tool)
		}
	}
	return hidden
}

type pushChannel struct {
	Sessions []pushOutcome `json:"sessions,omitempty"` // first-scored order, oldest first
	NewSince int           `json:"newSince,omitzero"`  // new sessions since support went negative
	Audit    []string      `json:"audit,omitempty"`    // re-audit sessions, pushed whatever the support
}

type pushOutcome struct {
	ID      string `json:"id"`
	Outcome int    `json:"outcome"` // +1 confirmed, -1 contradicted
}

// channel returns the named channel, adding it when the ledger has none.
func (ledger *pushLedger) channel(name string) *pushChannel {
	if ledger.Channels == nil {
		ledger.Channels = make(map[string]*pushChannel)
	}
	if ledger.Channels[name] == nil {
		ledger.Channels[name] = &pushChannel{}
	}
	return ledger.Channels[name]
}

// Agent hosts whose hooks push context; each keeps its own ledger.
const (
	hookHostClaude   = "claude"
	hookHostCodex    = "codex"
	hookHostCursor   = "cursor"
	hookHostOpencode = "opencode"
)

// hookHost names the agent behind a Claude-style hook: the opencode plugin
// says so in graft_host, Codex writes rollout-*.jsonl transcripts, and every
// other host that reaches these hooks is Claude Code. Cursor's own events
// name their host themselves.
func hookHost(input hookInput) string {
	if input.string("graft_host") == hookHostOpencode {
		return hookHostOpencode
	}
	if strings.HasPrefix(filepath.Base(input.string("transcript_path")), "rollout-") {
		return hookHostCodex
	}
	return hookHostClaude
}

func pushLedgerPath(root, host string) string {
	if host != hookHostCodex && host != hookHostCursor && host != hookHostOpencode {
		host = hookHostClaude
	}
	return filepath.Join(hookCacheDir(root), "push-ledger-"+host+".json")
}

// readPushLedger returns the repository's ledger with every channel present;
// a missing or malformed one reads as empty, which pushes everything. A
// version 1 ledger becomes the retrieval channel.
func readPushLedger(root, host string) pushLedger {
	var ledger pushLedger
	data, err := os.ReadFile(pushLedgerPath(root, host))
	if err != nil || jsonv2.Unmarshal(data, &ledger) != nil {
		ledger = pushLedger{}
	}
	if ledger.Channels == nil && (len(ledger.Sessions) > 0 || ledger.NewSince != 0 || len(ledger.Audit) > 0) {
		*ledger.channel(pushRetrieval) = pushChannel{Sessions: ledger.Sessions, NewSince: ledger.NewSince, Audit: ledger.Audit}
	}
	ledger.Sessions, ledger.NewSince, ledger.Audit = nil, 0, nil
	for _, name := range pushChannels {
		ledger.channel(name)
	}
	ledger.Version = 2
	return ledger
}

// updatePushLedger applies change under the ledger's lock and writes the
// result when change reports that it modified the ledger.
func updatePushLedger(root, host string, change func(*pushLedger) bool) error {
	path := pushLedgerPath(root, host)
	return withHookStateLock(path, func() error {
		ledger := readPushLedger(root, host)
		if !change(&ledger) {
			return nil
		}
		maps.DeleteFunc(ledger.Channels, func(_ string, channel *pushChannel) bool {
			return len(channel.Sessions) == 0 && channel.NewSince == 0 && len(channel.Audit) == 0
		})
		return writeHookJSONAtomic(path, ledger)
	})
}

// support is q = Σ γ^(n−k)·(c_k − λ·x_k) over the channel's scored sessions.
func (channel pushChannel) support() float64 {
	q := 0.0
	for _, session := range channel.Sessions {
		q *= pushDecay
		if session.Outcome > 0 {
			q++
		} else {
			q -= pushContradictionWeight
		}
	}
	return q
}

// sessionsUntilReaudit is how many new sessions must start before the
// channel's next re-audit session, counting that one.
func (channel pushChannel) sessionsUntilReaudit() int {
	return pushReauditEvery - channel.NewSince%pushReauditEvery
}

// hookPushAllowed reports whether session id may receive the named channel,
// counting the session as new for every channel the first time any push
// decision sees it. Without the ledger lock it pushes.
func hookPushAllowed(root, host, id, name string) bool {
	allowed := true
	err := updatePushLedger(root, host, func(ledger *pushLedger) bool {
		changed := false
		fresh := !slices.Contains(ledger.Seen, id)
		if fresh {
			ledger.Seen = keepLast(append(ledger.Seen, id), pushLedgerSeen)
			changed = true
		}
		for _, channel := range ledger.Channels {
			switch {
			case channel.support() >= 0:
				if channel.NewSince != 0 || len(channel.Audit) != 0 {
					channel.NewSince, channel.Audit = 0, nil
					changed = true
				}
			case fresh:
				channel.NewSince++
				if channel.NewSince%pushReauditEvery == 0 {
					channel.Audit = keepLast(append(channel.Audit, id), pushLedgerAudits)
				}
			}
		}
		channel := ledger.channel(name)
		allowed = channel.support() >= 0 || slices.Contains(channel.Audit, id)
		return changed
	})
	return err != nil || allowed
}

// recordPushOutcomes sets session id's single outcome in each channel of
// tally that resolved more than neutral injections, and, when tools is not
// empty, the graft MCP tools the session called.
func recordPushOutcomes(root, host, id string, tally map[string]pushTally, tools []string) {
	_ = updatePushLedger(root, host, func(ledger *pushLedger) bool {
		changed := len(tools) > 0 && ledger.recordTools(id, tools)
		for name, counts := range tally {
			outcome := counts.outcome()
			if outcome == 0 {
				continue
			}
			channel := ledger.channel(name)
			i := slices.IndexFunc(channel.Sessions, func(session pushOutcome) bool { return session.ID == id })
			switch {
			case i < 0:
				channel.Sessions = keepLast(append(channel.Sessions, pushOutcome{ID: id, Outcome: outcome}), pushLedgerSessions)
			case channel.Sessions[i].Outcome != outcome:
				channel.Sessions[i].Outcome = outcome
			default:
				continue
			}
			changed = true
		}
		return changed
	})
}

// addPendingInjection queues injection for resolution. A new orientation
// replaces the same agent's unresolved one, so a resumed session never holds
// two.
func addPendingInjection(session *sessionState, injection hookPendingInjection) {
	if injection.Channel == pushOrientation || injection.Channel == pushSubagentOrientation {
		session.PendingInjections = slices.DeleteFunc(session.PendingInjections, func(pending hookPendingInjection) bool {
			return pending.Channel == injection.Channel && pending.Agent == injection.Agent
		})
	}
	session.PendingInjections = keepLast(append(session.PendingInjections, injection), pendingInjectionLimit)
}

// keepLast drops the oldest elements of s beyond the newest n.
func keepLast[T any](s []T, n int) []T {
	if extra := len(s) - n; extra > 0 {
		return s[extra:]
	}
	return s
}

// markHookInjections applies one tool call of agent's transcript to the
// pending injections it can be evidence for. kind is the call's
// classification and search whether it was a raw search over indexed code.
func markHookInjections(session *sessionState, agent, callID, kind string, search bool, input any) {
	var values []string
	explores := kind == hookToolGraft || kind == hookToolSource
	for i := range session.PendingInjections {
		pending := &session.PendingInjections[i]
		if pending.Channel != pushRetrieval && pending.Agent != agent {
			continue
		}
		if pending.After != "" {
			if pending.After == callID {
				pending.After = ""
			}
			continue
		}
		switch pending.Channel {
		case pushRetrieval, pushBlast:
			if !pending.Touched {
				if values == nil {
					values = hookInputStrings(input, []string{})
				}
				pending.Touched = hookInjectionTouched(*pending, kind == hookToolGraft, values)
			}
			pending.Explored = pending.Explored || explores
		case pushNudge:
			switch {
			case pending.First != "":
			case kind == hookToolGraft:
				pending.First = hookToolGraft
			case search:
				pending.First = hookRawSearchCall
			}
		case pushOrientation, pushSubagentOrientation:
			if pending.First == "" && explores {
				pending.First = kind
			}
		case pushLoop:
			pending.Explored = true
		}
	}
}

// hookInjectionTouched reports whether a call's input strings name an
// injected file, or, for a graft call, an injected symbol.
func hookInjectionTouched(pending hookPendingInjection, graftCall bool, values []string) bool {
	for _, value := range values {
		value = filepath.ToSlash(value)
		if slices.ContainsFunc(pending.Files, func(file string) bool { return containsWord(value, file) }) ||
			graftCall && slices.ContainsFunc(pending.Symbols, func(symbol string) bool { return containsWord(value, symbol) }) {
			return true
		}
	}
	return false
}

// outcome is +1 when the injection was used, -1 when the agent worked past
// it, and 0 without evidence either way. reply is the main agent's last
// reply, which can confirm retrieval by naming a hit.
func (pending hookPendingInjection) outcome(reply string) int {
	switch pending.Channel {
	case pushRetrieval:
		named := slices.ContainsFunc(pending.Symbols, func(symbol string) bool { return containsWord(reply, symbol) }) ||
			slices.ContainsFunc(pending.Files, func(file string) bool { return containsWord(reply, filepath.Base(file)) })
		switch {
		case pending.Touched || named:
			return 1
		case pending.Explored:
			return -1
		}
	case pushBlast:
		switch {
		case pending.After != "":
		case pending.Touched:
			return 1
		case pending.Explored:
			return -1
		}
	case pushLoop:
		switch {
		case pending.Repeated:
			return -1
		case pending.Explored:
			return 1
		}
	case pushMicroskills:
		switch pending.First {
		case microskillConfirmed:
			return 1
		case microskillContradicted:
			return -1
		}
	default:
		switch pending.First {
		case hookToolGraft:
			return 1
		case hookToolSource, hookRawSearchCall:
			return -1
		}
	}
	return 0
}

// resolveHookInjections settles the pending injections decides selects,
// tallying them per channel and crediting the baseline of confirmed
// retrieval. It reports whether any was resolved.
func resolveHookInjections(session *sessionState, reply string, decides func(hookPendingInjection) bool) bool {
	resolved := false
	kept := session.PendingInjections[:0]
	for _, pending := range session.PendingInjections {
		if !decides(pending) {
			kept = append(kept, pending)
			continue
		}
		resolved = true
		if session.PushTally == nil {
			session.PushTally = make(map[string]pushTally)
		}
		tally := session.PushTally[pending.Channel]
		switch pending.outcome(reply) {
		case 1:
			tally.Confirmed++
			session.SavedTokens += pending.BaselineTokens
		case -1:
			tally.Contradicted++
		}
		session.PushTally[pending.Channel] = tally
	}
	session.PendingInjections = kept
	return resolved
}

// hookRetrievalSymbols names an injected hit for matching: the title before
// its kind, and the last segment of a dotted name.
func hookRetrievalSymbols(title string) []string {
	name, _, _ := strings.Cut(title, " · ")
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	symbols := []string{name}
	if _, last, ok := strings.CutLast(name, "."); ok && last != "" {
		symbols = append(symbols, last)
	}
	return symbols
}

// hookInputStrings collects every string value in a tool input.
func hookInputStrings(value any, out []string) []string {
	switch value := value.(type) {
	case string:
		out = append(out, value)
	case map[string]any:
		for _, item := range value {
			out = hookInputStrings(item, out)
		}
	case []any:
		for _, item := range value {
			out = hookInputStrings(item, out)
		}
	}
	return out
}

// containsWord reports whether word occurs in s without an identifier
// character on either side, so "a.go" does not match "data.go".
func containsWord(s, word string) bool {
	if word == "" {
		return false
	}
	for offset := 0; ; {
		i := strings.Index(s[offset:], word)
		if i < 0 {
			return false
		}
		start, end := offset+i, offset+i+len(word)
		before, _ := utf8.DecodeLastRuneInString(s[:start])
		after, _ := utf8.DecodeRuneInString(s[end:])
		if (start == 0 || !isIdentRune(before)) && (end == len(s) || !isIdentRune(after)) {
			return true
		}
		offset = start + 1
	}
}

func isIdentRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}
