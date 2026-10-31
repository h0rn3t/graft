package main

import (
	jsonv2 "encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Prompt-time retrieval is pushed per repository only while sessions that
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
	pendingRetrievalLimit   = 16
)

// hookPendingRetrieval is one prompt-time injection waiting for the end of
// the turn to show whether the agent used it.
type hookPendingRetrieval struct {
	Files            []string `json:"files"`
	Symbols          []string `json:"symbols"`
	BaselineTokens   int      `json:"baselineTokens,omitzero"`
	ReadsAtInjection int      `json:"readsAtInjection"`
	Touched          bool     `json:"touched,omitzero"`
}

// pushLedger is a repository's record of whether pushed retrieval paid off.
// It holds session ids and outcomes only, never prompts, paths or source.
type pushLedger struct {
	Version  int           `json:"version"`
	Sessions []pushOutcome `json:"sessions,omitempty"` // first-scored order, oldest first
	Seen     []string      `json:"seen,omitempty"`     // sessions that already submitted a prompt
	NewSince int           `json:"newSince,omitzero"`  // new sessions since support went negative
	Audit    []string      `json:"audit,omitempty"`    // re-audit sessions, pushed whatever the support
}

type pushOutcome struct {
	ID      string `json:"id"`
	Outcome int    `json:"outcome"` // +1 confirmed, -1 contradicted
}

// Agent hosts whose prompt hook pushes retrieval; each keeps its own ledger.
const (
	hookHostClaude = "claude"
	hookHostCodex  = "codex"
)

// hookHost names the agent behind a prompt or stop hook from its transcript:
// Codex writes rollout-*.jsonl, and every other host that reaches these hooks
// is Claude Code.
func hookHost(input hookInput) string {
	if strings.HasPrefix(filepath.Base(input.string("transcript_path")), "rollout-") {
		return hookHostCodex
	}
	return hookHostClaude
}

func pushLedgerPath(root, host string) string {
	if host != hookHostCodex {
		host = hookHostClaude
	}
	return filepath.Join(hookCacheDir(root), "push-ledger-"+host+".json")
}

// readPushLedger returns the repository's ledger; a missing or malformed one
// reads as empty, which pushes.
func readPushLedger(root, host string) pushLedger {
	var ledger pushLedger
	data, err := os.ReadFile(pushLedgerPath(root, host))
	if err != nil || jsonv2.Unmarshal(data, &ledger) != nil {
		return pushLedger{Version: 1}
	}
	ledger.Version = 1
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
		return writeHookJSONAtomic(path, ledger)
	})
}

// support is q = Σ γ^(n−k)·(c_k − λ·x_k) over the scored sessions.
func (ledger pushLedger) support() float64 {
	q := 0.0
	for _, session := range ledger.Sessions {
		q *= pushDecay
		if session.Outcome > 0 {
			q++
		} else {
			q -= pushContradictionWeight
		}
	}
	return q
}

// sessionsUntilReaudit is how many new sessions must start before the next
// re-audit session, counting that one.
func (ledger pushLedger) sessionsUntilReaudit() int {
	return pushReauditEvery - ledger.NewSince%pushReauditEvery
}

// hookPushAllowed reports whether session id may receive prompt-time
// retrieval, counting the session as new on its first prompt. Without the
// ledger lock it pushes.
func hookPushAllowed(root, host, id string) bool {
	allowed := true
	err := updatePushLedger(root, host, func(ledger *pushLedger) bool {
		negative := ledger.support() < 0
		changed := false
		if !slices.Contains(ledger.Seen, id) {
			ledger.Seen = keepLast(append(ledger.Seen, id), pushLedgerSeen)
			changed = true
			if negative {
				ledger.NewSince++
				if ledger.NewSince%pushReauditEvery == 0 {
					ledger.Audit = keepLast(append(ledger.Audit, id), pushLedgerAudits)
				}
			}
		}
		if !negative && (ledger.NewSince != 0 || len(ledger.Audit) != 0) {
			ledger.NewSince, ledger.Audit = 0, nil
			changed = true
		}
		allowed = !negative || slices.Contains(ledger.Audit, id)
		return changed
	})
	return err != nil || allowed
}

// recordPushOutcome sets session id's single outcome in the ledger from its
// resolved injections. Only neutral ones record nothing.
func recordPushOutcome(root, host, id string, confirmed, contradicted int) {
	outcome := 0
	switch {
	case contradicted > confirmed:
		outcome = -1
	case confirmed > 0:
		outcome = 1
	}
	if outcome == 0 {
		return
	}
	_ = updatePushLedger(root, host, func(ledger *pushLedger) bool {
		i := slices.IndexFunc(ledger.Sessions, func(session pushOutcome) bool { return session.ID == id })
		if i < 0 {
			ledger.Sessions = keepLast(append(ledger.Sessions, pushOutcome{ID: id, Outcome: outcome}), pushLedgerSessions)
			return true
		}
		if ledger.Sessions[i].Outcome == outcome {
			return false
		}
		ledger.Sessions[i].Outcome = outcome
		return true
	})
}

// keepLast drops the oldest elements of s beyond the newest n.
func keepLast[T any](s []T, n int) []T {
	if extra := len(s) - n; extra > 0 {
		return s[extra:]
	}
	return s
}

// markHookRetrievalTouches marks the pending injections a tool call reached:
// any call naming an injected file, and a graft call naming an injected
// symbol.
func markHookRetrievalTouches(session *sessionState, graftCall bool, input any) {
	values := hookInputStrings(input, nil)
	for i := range session.PendingRetrievals {
		pending := &session.PendingRetrievals[i]
		if pending.Touched {
			continue
		}
		for _, value := range values {
			value = filepath.ToSlash(value)
			if slices.ContainsFunc(pending.Files, func(file string) bool { return containsWord(value, file) }) ||
				graftCall && slices.ContainsFunc(pending.Symbols, func(symbol string) bool { return containsWord(value, symbol) }) {
				pending.Touched = true
				break
			}
		}
	}
}

// resolveHookRetrievals settles every pending injection at the end of the
// main agent's turn, crediting the baseline of the confirmed ones.
func resolveHookRetrievals(session *sessionState, reply string) {
	reads := session.GraftReads + session.SourceReads
	for _, pending := range session.PendingRetrievals {
		named := slices.ContainsFunc(pending.Symbols, func(symbol string) bool { return containsWord(reply, symbol) }) ||
			slices.ContainsFunc(pending.Files, func(file string) bool { return containsWord(reply, filepath.Base(file)) })
		switch {
		case pending.Touched || named:
			session.RetrievalsConfirmed++
			session.SavedTokens += pending.BaselineTokens
		case reads > pending.ReadsAtInjection:
			session.RetrievalsContradicted++
		}
	}
	session.PendingRetrievals = nil
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
