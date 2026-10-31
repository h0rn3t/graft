package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/h0rn3t/Graft/internal/jsonjs"
	"github.com/h0rn3t/Graft/internal/savings"
)

func runStats(opts callersOptions, stdout, stderr io.Writer) int {
	root := opts.root
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			writeDiagnostic(stderr, "✗ failed to resolve working directory: %v\n", err)
			return 1
		}
		root = nearestGraftRoot(cwd, opts.contextDir)
		if root != cwd {
			writeDiagnostic(stderr, "[graft] no graft/ here — answering from %s/graft\n", root)
		}
	}
	id, data, found := savings.LatestSession(root)
	if !found {
		var err error
		if opts.jsonOutput {
			_, err = io.WriteString(stdout, "null\n")
		} else {
			_, err = io.WriteString(stdout, "graft stats: no session recorded yet — use graft in an agent session, then look again.\n")
		}
		if err != nil {
			return 1
		}
		return 0
	}
	raw := data
	if !json.Valid(data) || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		data = []byte(emptySessionJSON)
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return 1
	}
	if opts.jsonOutput {
		// { id, ...session }: a parse failure or a literal null reads as the
		// empty session, as the TypeScript readSession does.
		session, err := jsonjs.Parse(raw)
		if err != nil || session == nil {
			session, _ = jsonjs.Parse([]byte(emptySessionJSON))
		}
		merged := jsonjs.NewObject()
		merged.Set("id", id)
		spread := jsonjs.Spread(session, true)
		for _, key := range spread.Keys() {
			value, _ := spread.Get(key)
			merged.Set(key, value)
		}
		state, support, untilReaudit := statsPushState(root, statsSessionHost(values), id)
		push := jsonjs.NewObject()
		push.Set("state", state)
		push.Set("support", support)
		push.Set("untilReaudit", float64(untilReaudit))
		merged.Set("push", push)
		if _, err := fmt.Fprintln(stdout, jsonjs.Stringify(merged, 2)); err != nil {
			return 1
		}
		return 0
	}
	readNumber := func(key string) float64 {
		var value float64
		_ = json.Unmarshal(values[key], &value)
		return value
	}
	graft, source, saved, injected := readNumber("graftReads"), readNumber("sourceReads"), readNumber("savedTokens"), readNumber("injectedTokens")
	mix := "no retrieval yet"
	if total := graft + source; total != 0 {
		mix = fmt.Sprintf("%.0f%% graft", math.Round(graft/total*100))
	}
	lines := []string{
		"graft stats — session " + id,
		fmt.Sprintf("  graft reads:   %g", graft),
		fmt.Sprintf("  source reads:  %g   (Read / Grep / Glob)", source),
		"  mix:           " + mix,
		"  tokens saved:  ~" + groupedNumber(saved),
		"  injected:      ~" + groupedNumber(injected) + "   (context graft's hooks added)",
		"  net saved:     ~" + groupedNumber(saved-injected),
	}
	if usd, ok := savings.DollarsSaved(saved-injected, readNumber("inputCostMicros"), readNumber("inputTokensBilled")); ok && saved > injected {
		lines = append(lines, "  value saved:   ~"+savings.FormatDollars(usd))
	}
	state, support, untilReaudit := statsPushState(root, statsSessionHost(values), id)
	switch state {
	case "off":
		lines = append(lines, fmt.Sprintf("  prompt push:   off (support %s, re-audit in %d new sessions)", jsonjs.FormatNumber(support), untilReaudit))
	default:
		lines = append(lines, fmt.Sprintf("  prompt push:   %s (support %s)", state, jsonjs.FormatNumber(support)))
	}
	var lastQuery string
	if json.Unmarshal(values["lastQuery"], &lastQuery) == nil && lastQuery != "" {
		lines = append(lines, "  last query:    "+lastQuery)
	}
	if _, err := fmt.Fprintln(stdout, strings.Join(lines, "\n")); err != nil {
		return 1
	}
	return 0
}

// statsSessionHost is the agent host a session recorded, Claude Code when it
// recorded none.
func statsSessionHost(values map[string]json.RawMessage) string {
	var host string
	if json.Unmarshal(values["host"], &host) != nil || host == "" {
		return hookHostClaude
	}
	return host
}

// statsPushState reports whether prompt retrieval is pushed for session id:
// "on", "off", or "re-audit" for a session pushed while support is negative,
// with the support rounded for display and, when off, the new sessions left
// until the next re-audit.
func statsPushState(root, host, id string) (state string, support float64, untilReaudit int) {
	ledger := readPushLedger(root, host)
	support = math.Round(ledger.support()*1000) / 1000
	switch {
	case ledger.support() >= 0:
		return "on", support, 0
	case slices.Contains(ledger.Audit, id):
		return "re-audit", support, 0
	default:
		return "off", support, ledger.sessionsUntilReaudit()
	}
}

// emptySessionJSON is the state of a session nothing has been recorded for.
const emptySessionJSON = `{"lastQuery":null,"perAgentQuery":{},"graftReads":0,"sourceReads":0,"savedTokens":0,"injectedPointers":[],"nudges":0}`

func groupedNumber(value float64) string {
	plain := strconv.FormatFloat(math.Abs(value), 'f', 0, 64)
	for i := len(plain) - 3; i > 0; i -= 3 {
		plain = plain[:i] + "," + plain[i:]
	}
	if value <= -0.5 {
		return "-" + plain
	}
	return plain
}
