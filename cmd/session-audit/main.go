// The session-audit command reports what the Claude Code sessions of a
// repository cost: API rounds, the input each round read, and the tool
// results that filled it. docs/session-audit.md says how to read it.
package main

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/h0rn3t/Graft/internal/sessionaudit"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("session-audit", flag.ContinueOnError)
	flags.SetOutput(stderr)
	projects := flags.String("projects", "", "Claude Code's projects `directory` (default ~/.claude/projects, or projects under $CLAUDE_CONFIG_DIR)")
	day := flags.String("since", "", "leave out conversations last written before this `day`, as 2026-11-01")
	asJSON := flags.Bool("json", false, "print the reports as JSON")
	flags.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "Usage: session-audit [--since day] [--projects directory] [--json] [repository ...]") // Nowhere to report a failed diagnostic.
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	text, err := report(*projects, *day, flags.Args(), *asJSON)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "session-audit: %v\n", err) // Nowhere to report a failed diagnostic.
		return 2
	}
	if _, err := io.WriteString(stdout, text); err != nil {
		return 2
	}
	return 0
}

// report audits each repository, the current directory when none is named,
// and renders the reports.
func report(projects, day string, repos []string, asJSON bool) (string, error) {
	var since time.Time
	if day != "" {
		var err error
		if since, err = time.ParseInLocation(time.DateOnly, day, time.Local); err != nil {
			return "", fmt.Errorf("--since: %w", err)
		}
	}
	if projects == "" {
		config := os.Getenv("CLAUDE_CONFIG_DIR")
		if config == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", fmt.Errorf("find Claude Code's projects directory: %w", err)
			}
			config = filepath.Join(home, ".claude")
		}
		projects = filepath.Join(config, "projects")
	}
	if len(repos) == 0 {
		repos = []string{"."}
	}
	reports := make([]sessionaudit.Report, 0, len(repos))
	var text strings.Builder
	for _, repo := range repos {
		path, err := filepath.Abs(repo)
		if err != nil {
			return "", err
		}
		audited, err := sessionaudit.Audit(projects, path, since)
		if err != nil {
			return "", err
		}
		reports = append(reports, audited)
		text.WriteString(audited.Human())
	}
	if !asJSON {
		return text.String(), nil
	}
	data, err := json.Marshal(reports, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return "", err
	}
	return string(data) + "\n", nil
}
