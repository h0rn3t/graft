package main

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	jsonv2 "encoding/json/v2"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A micro-skill is a one-line fact about a repository learned from the
// outcomes of earlier shell commands (TokenEconomizer's free envfacts layer):
// a candidate until three sessions confirm it, then validated until its
// decayed support turns negative, injected only while the failure it
// prevents keeps occurring or being prevented.
const (
	microskillPromote       = 3
	microskillDecay         = 0.8
	microskillContradiction = 5.0
	microskillFireGate      = 0.40
	microskillFireMin       = 4
	microskillSessionFacts  = 4
	microskillSessionTokens = 150
	microskillStoreLimit    = 64
	microskillSessionLimit  = 32
	microskillEvidenceLimit = 8
)

const (
	microskillCandidate = "candidate"
	microskillValidated = "validated"
	microskillRetired   = "retired"

	microskillFormRepair = "form_repair"
	microskillEnvValue   = "env_value"
)

const microskillsHeader = "[graft] Repository facts learned from earlier sessions here, confirmed by later ones:"

// microskillLint refuses a fact that would read as advice to skip work.
var microskillLint = regexp.MustCompile(`(?i)\b(skip|no need|don'?t|do not|stop|done|completed?|finish(?:ed)?|verify|verification)\b`)

type microskillStore struct {
	Version int          `json:"version"`
	Facts   []microskill `json:"facts"`
}

type microskill struct {
	ID          string               `json:"id"`
	Family      string               `json:"family"`
	Intent      string               `json:"intent"`
	Failing     string               `json:"failing,omitempty"`
	Working     string               `json:"working,omitempty"`
	ErrorClass  string               `json:"errorClass,omitempty"`
	Variable    string               `json:"variable,omitempty"`
	Value       string               `json:"value,omitempty"`
	Instruction string               `json:"instruction"`
	Scope       string               `json:"scope"`
	Anchors     map[string]string    `json:"anchors,omitempty"`
	Status      string               `json:"status"`
	Revalidate  bool                 `json:"revalidate,omitzero"`
	DemotedAt   string               `json:"demotedAt,omitempty"`
	Sessions    []microskillSession  `json:"sessions,omitempty"` // first-scored order, oldest first
	Evidence    []microskillEvidence `json:"evidence,omitempty"`
	Metrics     microskillMetrics    `json:"metrics"`
	UpdatedAt   string               `json:"updatedAt"`
}

// microskillSession is one session's verdict on a fact; a later turn of the
// same session replaces it.
type microskillSession struct {
	ID             string `json:"id"`
	At             string `json:"at"`
	Verdict        int    `json:"verdict,omitzero"` // +1 confirmed, -1 contradicted
	Exercised      bool   `json:"exercised,omitzero"`
	Fired          bool   `json:"fired,omitzero"`
	Injected       bool   `json:"injected,omitzero"`
	Prevented      bool   `json:"prevented,omitzero"`
	InjectedTokens int    `json:"injectedTokens,omitzero"`
	FailureTokens  int    `json:"failureTokens,omitzero"`
}

type microskillEvidence struct {
	Session string `json:"session"`
	At      string `json:"at"`
	Kind    string `json:"kind"` // repair, confirm, contradict, exercise
	Form    string `json:"form"`
	Error   string `json:"error,omitempty"`
}

type microskillMetrics struct {
	Exercised        int `json:"exercised"`
	Fired            int `json:"fired"`
	Confirmed        int `json:"confirmed"`
	Contradicted     int `json:"contradicted"`
	InjectedSessions int `json:"injectedSessions"`
	InjectedTokens   int `json:"injectedTokens"`
	FailureTokens    int `json:"failureTokens"` // mean cost of the failure in a session where it occurred
	AvoidedTokens    int `json:"avoidedTokens"` // prevented failures × FailureTokens, an estimate
}

func microskillsDisabled() bool {
	return os.Getenv("GRAFT_NO_MICROSKILLS") != ""
}

func microskillsPath(root string) string {
	return filepath.Join(hookCacheDir(root), "microskills.json")
}

// readMicroskills returns the repository's facts with their anchors checked
// against the files on disk; a missing or malformed store reads as empty.
func readMicroskills(root string) []microskill {
	var store microskillStore
	data, err := os.ReadFile(microskillsPath(root))
	if err != nil || jsonv2.Unmarshal(data, &store) != nil {
		return nil
	}
	return refreshMicroskillAnchors(root, store.Facts, time.Now().UTC().Format(time.RFC3339))
}

// updateMicroskills applies change to the facts under the store's lock and
// writes them back, keeping at most microskillStoreLimit.
func updateMicroskills(root string, change func([]microskill) []microskill) error {
	path := microskillsPath(root)
	return withHookStateLock(path, func() error {
		facts := change(readMicroskills(root))
		for len(facts) > microskillStoreLimit {
			i := microskillEvictee(facts)
			facts = slices.Delete(facts, i, i+1)
		}
		return writeHookJSONAtomic(path, microskillStore{Version: 1, Facts: facts})
	})
}

// microskillEvictee is the fact a full store drops: the least recently
// updated retired one, else the least recently updated.
func microskillEvictee(facts []microskill) int {
	best := -1
	for i, fact := range facts {
		retired, bestRetired := fact.Status == microskillRetired, best >= 0 && facts[best].Status == microskillRetired
		if best < 0 || retired && !bestRetired || retired == bestRetired && fact.UpdatedAt < facts[best].UpdatedAt {
			best = i
		}
	}
	return best
}

// microskillAnchors hashes the manifests and lockfiles in a scope directory.
func microskillAnchors(root, scope string) map[string]string {
	anchors := make(map[string]string)
	for _, name := range slices.Concat(microskillManifests, microskillLockfiles) {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(scope), name))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(data)
		anchors[name] = hex.EncodeToString(sum[:])
	}
	return anchors
}

// refreshMicroskillAnchors applies anchor changes: a validated fact becomes a
// candidate to be confirmed once more, and a retired one is dropped so that
// its repair can be learned afresh.
func refreshMicroskillAnchors(root string, facts []microskill, now string) []microskill {
	kept := facts[:0]
	for _, fact := range facts {
		if anchors := microskillAnchors(root, fact.Scope); !maps.Equal(anchors, fact.Anchors) && fact.reanchor(anchors, now) {
			continue
		}
		kept = append(kept, fact)
	}
	return kept
}

// reanchor records new anchors and reports whether the fact is to be dropped.
func (fact *microskill) reanchor(anchors map[string]string, now string) bool {
	fact.Anchors, fact.UpdatedAt = anchors, now
	switch fact.Status {
	case microskillRetired:
		return true
	case microskillValidated:
		fact.Status, fact.Revalidate, fact.DemotedAt = microskillCandidate, true, now
	}
	return false
}

// microskillErrorClass compares failures without their numbers.
var microskillDigits = regexp.MustCompile(`\d+`)

func microskillErrorClass(line string) string {
	return strings.ToLower(microskillDigits.ReplaceAllString(line, "N"))
}

func microskillID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(sum[:6])
}

// microskillCandidates proposes facts from one session's observations: a
// failed call followed by a later working call of the same intent, agent and
// scope is a form repair when the forms differ, and an env value when the
// later call set a variable the error names.
func microskillCandidates(observations []shellObservation) []microskill {
	var facts []microskill
	for i, failed := range observations {
		if !failed.Failed {
			continue
		}
		j := slices.IndexFunc(observations[i+1:], func(later shellObservation) bool {
			return !later.Failed && later.Agent == failed.Agent && later.Scope == failed.Scope && later.Intent == failed.Intent
		})
		if j < 0 {
			continue
		}
		working := observations[i+1+j]
		fact := microskill{Intent: failed.Intent, Scope: failed.Scope, ErrorClass: microskillErrorClass(failed.Error)}
		switch {
		case working.Form != failed.Form:
			fact.Family, fact.Failing, fact.Working = microskillFormRepair, failed.Form, working.Form
			fact.ID = microskillID(fact.Family, fact.Scope, fact.Intent, fact.Failing, fact.Working)
			fact.Instruction = "`" + fact.Failing + "` fails here with `" + clip(failed.Error, 80) + "`; `" + fact.Working + "` works."
		default:
			names := slices.Sorted(maps.Keys(working.Env))
			k := slices.IndexFunc(names, func(name string) bool {
				_, before := failed.Env[name]
				return !before && containsWord(failed.Error, name)
			})
			if k < 0 {
				continue
			}
			fact.Family, fact.Failing, fact.Variable, fact.Value = microskillEnvValue, failed.Form, names[k], working.Env[names[k]]
			fact.ID = microskillID(fact.Family, fact.Scope, fact.Intent, fact.Variable)
			fact.Instruction = "`" + fact.Failing + "` needs `" + fact.Variable + "` set here"
			if fact.Value != "" {
				fact.Instruction += ": `" + fact.Variable + "=" + fact.Value + "` works"
			}
			fact.Instruction += "."
		}
		if microskillLint.MatchString(fact.Instruction) ||
			slices.ContainsFunc(facts, func(other microskill) bool { return other.ID == fact.ID }) {
			continue
		}
		facts = append(facts, fact)
	}
	return facts
}

// microskillJudgment is what one session's observations show about a fact.
type microskillJudgment struct {
	exercised, failingFailed, failingTried, workingWorked, contradicted bool
	failureTokens                                                       int
	form, error                                                         string
}

func (fact microskill) judge(observations []shellObservation) microskillJudgment {
	var judged microskillJudgment
	for _, observation := range observations {
		if observation.Scope != fact.Scope || observation.Intent != fact.Intent {
			continue
		}
		judged.exercised = true
		if judged.form == "" {
			judged.form = observation.Form
		}
		classMatches := observation.Failed && microskillErrorClass(observation.Error) == fact.ErrorClass
		var failing, working bool
		switch fact.Family {
		case microskillFormRepair:
			failing, working = observation.Form == fact.Failing, observation.Form == fact.Working
			judged.contradicted = judged.contradicted || failing && !observation.Failed || working && classMatches
			judged.failingTried = judged.failingTried || failing
		case microskillEnvValue:
			value, set := observation.Env[fact.Variable]
			set = set && (fact.Value == "" || value == fact.Value)
			needs := observation.Failed && containsWord(observation.Error, fact.Variable)
			failing, working = needs && !set, set
			judged.contradicted = judged.contradicted || needs && set
			judged.failingTried = judged.failingTried || failing
		}
		if failing && classMatches {
			judged.failingFailed = true
			judged.failureTokens += observation.Tokens
			judged.form, judged.error = observation.Form, observation.Error
		}
		judged.workingWorked = judged.workingWorked || working && !observation.Failed
	}
	return judged
}

// record sets session id's verdict on the fact from its observations, and
// reports whether anything changed.
func (fact *microskill) record(id, now string, observations []shellObservation, injected bool, injectedTokens int) bool {
	judged := fact.judge(observations)
	if !judged.exercised && !injected {
		return false
	}
	entry := microskillSession{
		ID:             id,
		Exercised:      judged.exercised,
		Injected:       injected,
		InjectedTokens: injectedTokens,
		FailureTokens:  judged.failureTokens,
		Prevented:      injected && judged.workingWorked && !judged.failingTried,
	}
	entry.Fired = judged.failingFailed || entry.Prevented
	kind := "exercise"
	switch {
	case judged.contradicted:
		entry.Verdict, kind = -1, "contradict"
	case judged.failingFailed && judged.workingWorked:
		entry.Verdict, kind = 1, "repair"
	case entry.Prevented:
		entry.Verdict, kind = 1, "confirm"
	}
	i := slices.IndexFunc(fact.Sessions, func(session microskillSession) bool { return session.ID == id })
	if i >= 0 {
		entry.At = fact.Sessions[i].At
		if fact.Sessions[i] == entry {
			return false
		}
		if fact.Sessions[i].Verdict != entry.Verdict {
			entry.At = now
		}
		fact.Sessions[i] = entry
	} else {
		entry.At = now
		fact.Sessions = keepLast(append(fact.Sessions, entry), microskillSessionLimit)
	}
	if judged.exercised {
		evidence := microskillEvidence{Session: id, At: now, Kind: kind, Form: judged.form, Error: judged.error}
		fact.Evidence = slices.DeleteFunc(fact.Evidence, func(item microskillEvidence) bool { return item.Session == id })
		fact.Evidence = keepLast(append(fact.Evidence, evidence), microskillEvidenceLimit)
	}
	fact.settle()
	fact.UpdatedAt = now
	return true
}

// support is Σ γ^(n−k)·(c_k − λ·x_k) over the fact's scored sessions.
func (fact microskill) support() float64 {
	q := 0.0
	for _, session := range fact.Sessions {
		switch {
		case session.Verdict > 0:
			q = q*microskillDecay + 1
		case session.Verdict < 0:
			q = q*microskillDecay - microskillContradiction
		}
	}
	return q
}

// settle moves the fact through its lifecycle and derives its metrics.
func (fact *microskill) settle() {
	var scored []int
	confirms := 0
	metrics := microskillMetrics{}
	failures, prevented := 0, 0
	for _, session := range fact.Sessions {
		if session.Verdict != 0 {
			scored = append(scored, session.Verdict)
		}
		if session.Verdict > 0 && (!fact.Revalidate || session.At > fact.DemotedAt) {
			confirms++
		}
		metrics.Exercised += b2i(session.Exercised)
		metrics.Fired += b2i(session.Fired)
		metrics.Confirmed += b2i(session.Verdict > 0)
		metrics.Contradicted += b2i(session.Verdict < 0)
		metrics.InjectedSessions += b2i(session.Injected)
		metrics.InjectedTokens += session.InjectedTokens
		prevented += b2i(session.Prevented)
		if session.FailureTokens > 0 {
			metrics.FailureTokens += session.FailureTokens
			failures++
		}
	}
	if failures > 0 {
		metrics.FailureTokens /= failures
	}
	metrics.AvoidedTokens = prevented * metrics.FailureTokens
	fact.Metrics = metrics

	switch fact.Status {
	case microskillCandidate:
		need := microskillPromote
		if fact.Revalidate {
			need = 1
		}
		if confirms >= need && fact.support() >= 0 {
			fact.Status, fact.Revalidate, fact.DemotedAt = microskillValidated, false, ""
		}
	case microskillValidated:
		lastTwo := len(scored) >= 2 && scored[len(scored)-1] < 0 && scored[len(scored)-2] < 0
		if fact.support() < 0 || lastTwo {
			fact.Status = microskillRetired
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// injectable reports whether a fact may reach an agent: validated, and
// firing in at least microskillFireGate of the sessions that exercised it.
func (fact microskill) injectable() bool {
	return fact.Status == microskillValidated && fact.Metrics.Exercised >= microskillFireMin &&
		float64(fact.Metrics.Fired) >= microskillFireGate*float64(fact.Metrics.Exercised)
}

func (fact microskill) line() string {
	if fact.Scope == "." {
		return "- " + fact.Instruction
	}
	return "- In `" + fact.Scope + "/`: " + fact.Instruction
}

// lastConfirmed is when a session last confirmed the fact.
func (fact microskill) lastConfirmed() string {
	last := ""
	for _, session := range fact.Sessions {
		if session.Verdict > 0 {
			last = max(last, session.At)
		}
	}
	return last
}

// foldMicroskills folds session id's observations into the repository's
// facts: new candidates from its repairs, and its verdict on every fact it
// exercised or received.
func foldMicroskills(root, id string) {
	if microskillsDisabled() {
		return
	}
	session := readHookSession(root, id)
	if len(session.ShellObservations) == 0 && len(session.MicroskillsInjected) == 0 {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_ = updateMicroskills(root, func(facts []microskill) []microskill {
		for _, candidate := range microskillCandidates(session.ShellObservations) {
			if slices.ContainsFunc(facts, func(fact microskill) bool { return fact.ID == candidate.ID }) {
				continue
			}
			candidate.Status, candidate.Anchors, candidate.UpdatedAt = microskillCandidate, microskillAnchors(root, candidate.Scope), now
			facts = append(facts, candidate)
		}
		for i := range facts {
			fact := &facts[i]
			if fact.Status == microskillRetired {
				continue
			}
			injected := slices.Contains(session.MicroskillsInjected, fact.ID)
			tokens := 0
			if injected {
				tokens = hookContextTokens(fact.line())
			}
			fact.record(id, now, session.ShellObservations, injected, tokens)
		}
		return facts
	})
}

// pickMicroskills chooses the facts to inject: injectable, in scope, not
// already injected, one per scope and intent, by support, within the
// session's fact and token budget.
func pickMicroskills(facts []microskill, inScope func(scope string) bool, injected []string, spent int) []microskill {
	taken := make(map[string]bool)
	for _, fact := range facts {
		if slices.Contains(injected, fact.ID) {
			taken[fact.Scope+"\x00"+fact.Intent] = true
		}
	}
	var eligible []microskill
	for _, fact := range facts {
		if fact.injectable() && inScope(fact.Scope) && !slices.Contains(injected, fact.ID) && !taken[fact.Scope+"\x00"+fact.Intent] {
			eligible = append(eligible, fact)
		}
	}
	slices.SortStableFunc(eligible, func(a, b microskill) int {
		return cmp.Or(cmp.Compare(b.support(), a.support()), strings.Compare(b.lastConfirmed(), a.lastConfirmed()))
	})
	var chosen []microskill
	tokens := spent
	if spent == 0 {
		tokens = hookContextTokens(microskillsHeader)
	}
	for _, fact := range eligible {
		key := fact.Scope + "\x00" + fact.Intent
		cost := hookContextTokens(fact.line())
		if len(injected)+len(chosen) >= microskillSessionFacts || taken[key] || tokens+cost > microskillSessionTokens {
			continue
		}
		taken[key] = true
		chosen = append(chosen, fact)
		tokens += cost
	}
	return chosen
}

// renderMicroskills is the facts block an agent receives.
func renderMicroskills(facts []microskill) string {
	lines := []string{microskillsHeader}
	for _, fact := range facts {
		lines = append(lines, fact.line())
	}
	return strings.Join(lines, "\n")
}

// claimMicroskills picks the facts for a session and records them in it as
// injected and pending resolution, charging their tokens. It returns the
// block to emit, "" when nothing is picked.
func claimMicroskills(session *sessionState, facts []microskill, inScope func(scope string) bool) string {
	chosen := pickMicroskills(facts, inScope, session.MicroskillsInjected, session.MicroskillTokens)
	if len(chosen) == 0 {
		return ""
	}
	text := renderMicroskills(chosen)
	for _, fact := range chosen {
		session.MicroskillsInjected = append(session.MicroskillsInjected, fact.ID)
		addPendingInjection(session, hookPendingInjection{Channel: pushMicroskills, Fact: fact.ID})
	}
	session.MicroskillTokens += hookContextTokens(text)
	return text
}

// hookMicroskillsForSession is the facts block for a starting session: facts
// scoped to the repository root or to a directory holding cwd, when the
// channel pushes. It records the injection and returns "" without one.
func hookMicroskillsForSession(root, host, id, cwd string) string {
	if microskillsDisabled() {
		return ""
	}
	facts := readMicroskills(root)
	rel, ok := relativeUnder(root, cmp.Or(cwd, root))
	inScope := func(scope string) bool {
		return scope == "." || ok && scopeHolds(scope, filepath.ToSlash(rel))
	}
	if !slices.ContainsFunc(facts, func(fact microskill) bool { return fact.injectable() && inScope(fact.Scope) }) ||
		!hookPushAllowed(root, host, id, pushMicroskills) {
		return ""
	}
	text := ""
	_ = updateHookSession(root, id, func(session *sessionState) bool {
		text = claimMicroskills(session, facts, inScope)
		return text != ""
	})
	return text
}

// scopeHolds reports whether the repository-relative path lies in scope.
func scopeHolds(scope, path string) bool {
	return scope == "." || path == scope || strings.HasPrefix(path, scope+"/")
}

// recordShellObservation is every host's single entry point for a shell
// call: it keeps the observation, once per call, and settles pending
// micro-skill injections of the same agent that the call decides.
func recordShellObservation(session *sessionState, observation shellObservation, facts []microskill) {
	if observation.Call != "" && slices.ContainsFunc(session.ShellObservations, func(seen shellObservation) bool {
		return seen.Call == observation.Call
	}) {
		return
	}
	session.ShellObservations = keepLast(append(session.ShellObservations, observation), shellObservationLimit)
	for i := range session.PendingInjections {
		pending := &session.PendingInjections[i]
		if pending.Channel != pushMicroskills || pending.Agent != observation.Agent || pending.First != "" {
			continue
		}
		k := slices.IndexFunc(facts, func(fact microskill) bool { return fact.ID == pending.Fact })
		if k >= 0 {
			pending.First = facts[k].decides(observation)
		}
	}
}

// decides is what one call of a fact's intent shows about an injection of
// it: "confirmed", "contradicted", or "" when it is another intent or scope.
func (fact microskill) decides(observation shellObservation) string {
	if observation.Scope != fact.Scope || observation.Intent != fact.Intent {
		return ""
	}
	judged := fact.judge([]shellObservation{observation})
	switch {
	case judged.contradicted || judged.failingTried:
		return microskillContradicted
	case judged.workingWorked:
		return microskillConfirmed
	}
	return ""
}

const (
	microskillConfirmed    = "confirmed"
	microskillContradicted = "contradicted"
)
