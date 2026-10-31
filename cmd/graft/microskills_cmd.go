package main

import (
	"encoding/json/jsontext"
	jsonv2 "encoding/json/v2"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/h0rn3t/Graft/internal/jsonjs"
)

// runMicroskills prints the repository's learned facts: the validated ones,
// and with --all the candidates and retired ones too.
func runMicroskills(opts callersOptions, stdout, stderr io.Writer) int {
	root := opts.root
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			writeDiagnostic(stderr, "✗ failed to resolve working directory: %v\n", err)
			return 1
		}
		root = nearestGraftRoot(cwd, opts.contextDir)
	}
	var facts []microskill
	for _, fact := range readMicroskills(root) {
		if opts.all || fact.Status == microskillValidated {
			facts = append(facts, fact)
		}
	}
	if opts.jsonOutput {
		data, err := jsonv2.Marshal(microskillStore{Version: 1, Facts: facts}, jsontext.WithIndent("  "))
		if err != nil {
			writeDiagnostic(stderr, "✗ %v\n", err)
			return 1
		}
		if _, err := fmt.Fprintf(stdout, "%s\n", data); err != nil {
			return 1
		}
		return 0
	}
	if len(facts) == 0 {
		_, err := io.WriteString(stdout, "graft microskills: none learned yet — facts form when agent sessions here fail at a command and repair it.\n")
		if err != nil {
			return 1
		}
		return 0
	}
	lines := []string{fmt.Sprintf("graft microskills — %d facts", len(facts))}
	for _, fact := range facts {
		firing := "fires —"
		if exercised := fact.Metrics.Exercised; exercised > 0 {
			firing = fmt.Sprintf("fires %d/%d", fact.Metrics.Fired, exercised)
		}
		lines = append(lines,
			fmt.Sprintf("  %s  %-9s  scope %s  %s  support %s  injected %d sessions (~%d tokens)  avoided ~%d tokens",
				fact.ID, fact.Status, fact.Scope, firing, jsonjs.FormatNumber(fact.support()),
				fact.Metrics.InjectedSessions, fact.Metrics.InjectedTokens, fact.Metrics.AvoidedTokens),
			"    "+fact.Instruction)
		for _, evidence := range fact.Evidence {
			line := fmt.Sprintf("      %s %-10s session %s  %s", evidence.At, evidence.Kind, evidence.Session, evidence.Form)
			if evidence.Error != "" {
				line += "  — " + evidence.Error
			}
			lines = append(lines, line)
		}
	}
	if _, err := fmt.Fprintln(stdout, strings.Join(lines, "\n")); err != nil {
		return 1
	}
	return 0
}
