package main

import (
	"cmp"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// shellObservationLimit bounds the observations a session keeps, newest last.
const shellObservationLimit = 128

// shellObservation is one shell call reduced to what micro-skills compare. It
// holds no output beyond one masked error line.
type shellObservation struct {
	Call   string            `json:"call,omitempty"` // tool_use id, to record a call once
	Agent  string            `json:"agent,omitempty"`
	Intent string            `json:"intent"`
	Form   string            `json:"form"`
	Scope  string            `json:"scope"`
	Failed bool              `json:"failed,omitzero"`
	Error  string            `json:"error,omitempty"`
	Env    map[string]string `json:"env,omitempty"`   // assignments the call set; "" for a value not kept
	Tokens int               `json:"tokens,omitzero"` // what the failed call cost: command and output
}

// pendingShellCall is a Bash call seen in a transcript whose result has not
// arrived yet.
type pendingShellCall struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
}

// shellCommand is a shell command reduced to its intent and form.
type shellCommand struct {
	intent string            // program basename plus its bare subcommand
	form   string            // program as typed, subcommand and flags; other arguments "…"
	dir    string            // where leading cd commands move, as typed
	env    map[string]string // assignments set before the program
}

var (
	shellSubcommandPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)
	shellAssignmentPattern = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)
	// An error line names what went wrong in words a tool uses for it; a bare
	// non-zero exit (grep without a match) has none.
	shellErrorPattern = regexp.MustCompile(`(?i)\b(errors?|fatal|not found|cannot|can't|couldn't|denied|unknown (flag|option|command|subcommand)|no such|missing|failed|not set|undefined|not recognized|invalid)\b`)
	// Claude Code writes "Exit code 1", Codex "Exit code: 1" or "Process
	// exited with code 1" (the forms TokenEconomizer reads).
	shellExitLine    = regexp.MustCompile(`(?i)^\s*(?:exit code|process exited with code)[:\s]+-?\d+\s*$`)
	shellExitPattern = regexp.MustCompile(`(?im)^\s*(?:exit code|process exited with code)[:\s]+(-?\d+)`)
	// Values of these are never stored: their names say they are secrets.
	shellSecretName    = regexp.MustCompile(`(?i)KEY|TOKEN|SECRET|PASS|AUTH|CREDENTIAL`)
	shellAssignmentArg = regexp.MustCompile(`\b([A-Za-z_][A-Za-z0-9_]*)=('[^']*'|"[^"]*"|[^\s'"]+)`)
	shellTokenLike     = regexp.MustCompile(`[A-Za-z0-9_\-]{32,}`)
)

// parseShellCommand finds the command's program after leading cd, export,
// env and assignments; segments after the first program are ignored. A
// command with no program has no intent.
func parseShellCommand(command string) shellCommand {
	parsed := shellCommand{env: map[string]string{}}
	for _, segment := range shellSegments(command) {
		words := shellWords(segment)
		switch {
		case len(words) == 0:
			continue
		case words[0] == "cd":
			if len(words) > 1 {
				if strings.HasPrefix(words[1], "/") {
					parsed.dir = words[1]
				} else {
					parsed.dir = path.Join(parsed.dir, words[1])
				}
			}
			continue
		case words[0] == "export":
			words = words[1:]
		}
		if len(words) > 0 && words[0] == "env" {
			words = words[1:]
		}
		for len(words) > 0 {
			match := shellAssignmentPattern.FindStringSubmatch(words[0])
			if match == nil {
				break
			}
			parsed.env[match[1]] = match[2]
			words = words[1:]
		}
		if len(words) == 0 {
			continue
		}
		parsed.intent, parsed.form = path.Base(filepath.ToSlash(words[0])), words[0]
		rest := words[1:]
		if len(rest) > 0 && shellSubcommandPattern.MatchString(rest[0]) {
			parsed.intent += " " + rest[0]
			parsed.form += " " + rest[0]
			rest = rest[1:]
		}
		for _, word := range rest {
			switch {
			case strings.HasPrefix(word, "-"):
				if name, _, ok := strings.Cut(word, "="); ok {
					word = name + "=…"
				}
				parsed.form += " " + word
			case !strings.HasSuffix(parsed.form, " …"):
				parsed.form += " …"
			}
		}
		return parsed
	}
	return shellCommand{}
}

// shellSegments splits a command at &&, ||, ;, | and newlines outside quotes.
func shellSegments(command string) []string {
	var segments []string
	var quote byte
	start := 0
	for i := 0; i < len(command); i++ {
		c := command[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '\\':
			i++
		case c == ';' || c == '\n' || c == '|' || c == '&' && i+1 < len(command) && command[i+1] == '&':
			segments = append(segments, command[start:i])
			if (c == '|' || c == '&') && i+1 < len(command) && command[i+1] == c {
				i++
			}
			start = i + 1
		}
	}
	return append(segments, command[start:])
}

// shellWords splits one segment into words at blanks outside quotes, without
// the quotes.
func shellWords(segment string) []string {
	var words []string
	var word strings.Builder
	var quote byte
	inWord := false
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				word.WriteByte(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == '\\' && i+1 < len(segment):
			i++
			word.WriteByte(segment[i])
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, word.String())
	}
	return words
}

// shellErrorLine returns the first line of a result that names an error. skip
// is true for a result that is no command outcome at all: another hook's
// denial, or the user declining the call.
func shellErrorLine(text string) (line string, skip bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "PreToolUse:") || strings.Contains(trimmed, "hook error:") ||
		strings.Contains(trimmed, "doesn't want to proceed") {
		return "", true
	}
	for line := range strings.Lines(trimmed) {
		line = strings.TrimSpace(line)
		if line != "" && !shellExitLine.MatchString(line) && shellErrorPattern.MatchString(line) {
			return line, false
		}
	}
	return "", false
}

// maskShellText makes paths under root relative and the home directory "~",
// and drops the values of secret-named assignments and token-like strings.
func maskShellText(text, root string) string {
	for _, prefix := range []string{root, filepath.ToSlash(root)} {
		if prefix != "" && prefix != "/" {
			text = strings.ReplaceAll(text, prefix+"/", "")
			text = strings.ReplaceAll(text, prefix+string(filepath.Separator), "")
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && home != "/" {
		text = strings.ReplaceAll(text, home, "~")
	}
	text = shellAssignmentArg.ReplaceAllStringFunc(text, func(assignment string) string {
		name, _, _ := strings.Cut(assignment, "=")
		if shellSecretName.MatchString(name) {
			return name + "=…"
		}
		return assignment
	})
	return shellTokenLike.ReplaceAllString(text, "…")
}

// shellResponseOutcome reads a tool hook's shell response: its text and
// whether the call failed. A numeric exit code decides first, then an error
// flag, then an exit line in the text.
func shellResponseOutcome(response any) (text string, failed bool) {
	fields, ok := response.(map[string]any)
	if !ok {
		text = shellResultText(response)
		return text, shellTextExit(text)
	}
	for _, key := range []string{"content", "output", "aggregated_output", "formatted_output"} {
		if text = shellResultText(fields[key]); text != "" {
			break
		}
	}
	if text == "" {
		var parts []string
		for _, key := range []string{"stdout", "stderr"} {
			if part := shellResultText(fields[key]); part != "" {
				parts = append(parts, part)
			}
		}
		text = strings.Join(parts, "\n")
	}
	metadata, _ := fields["metadata"].(map[string]any)
	for _, code := range []any{fields["exit_code"], fields["exitCode"], fields["returncode"], fields["status_code"], metadata["exit"], metadata["exit_code"]} {
		if code, ok := code.(float64); ok {
			return text, code != 0
		}
	}
	for _, key := range []string{"isError", "is_error"} {
		if flag, ok := fields[key].(bool); ok {
			return text, flag
		}
	}
	return text, shellTextExit(text)
}

// shellTextExit reports whether text carries a non-zero exit line among its
// first 400 bytes or its last line.
func shellTextExit(text string) bool {
	head := text[:min(len(text), 400)]
	match := shellExitPattern.FindStringSubmatch(head)
	if match == nil {
		_, last, _ := strings.CutLast(strings.TrimRight(text, "\n"), "\n")
		match = shellExitPattern.FindStringSubmatch(last)
	}
	if match == nil {
		line, skip := shellErrorLine(text)
		return line != "" && !skip
	}
	code, err := strconv.Atoi(match[1])
	return err == nil && code != 0
}

// shellResultText is a tool result's text: a string, or the text blocks of a
// content array.
func shellResultText(content any) string {
	switch content := content.(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, block := range content {
			if block, ok := block.(map[string]any); ok {
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// microskillManifests mark the directory a fact applies to; with the
// lockfiles they are the anchors whose change makes a fact be checked again.
var (
	microskillManifests = []string{"go.mod", "package.json", "pyproject.toml", "requirements.txt", "Cargo.toml", "pom.xml",
		"build.gradle", "build.gradle.kts", "Makefile", "CMakeLists.txt", "Gemfile", "composer.json"}
	microskillLockfiles = []string{"go.sum", "package-lock.json", "pnpm-lock.yaml", "yarn.lock", "poetry.lock", "uv.lock", "Cargo.lock"}
)

// shellScope is the repository-relative directory of the nearest build
// manifest at or above cwd, "." when there is none. ok is false when cwd is
// outside root.
func shellScope(root, cwd string) (string, bool) {
	if cwd == "" {
		return ".", true
	}
	rel, ok := relativeUnder(root, cwd)
	if !ok {
		return "", false
	}
	for dir := rel; dir != "."; dir = filepath.Dir(dir) {
		for _, name := range microskillManifests {
			if _, err := os.Stat(filepath.Join(root, dir, name)); err == nil {
				return filepath.ToSlash(dir), true
			}
		}
	}
	return ".", true
}

// relativeUnder is dir relative to root, also when one of them is reached
// through a symlink; ok is false when dir is outside root.
func relativeUnder(root, dir string) (string, bool) {
	outside := func(rel string, err error) bool {
		return err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	rel, err := filepath.Rel(root, dir)
	if !outside(rel, err) {
		return rel, true
	}
	realRoot, rootErr := filepath.EvalSymlinks(root)
	realDir, dirErr := filepath.EvalSymlinks(dir)
	if rootErr != nil || dirErr != nil {
		return "", false
	}
	rel, err = filepath.Rel(realRoot, realDir)
	return rel, !outside(rel, err)
}

// newShellObservation reduces one shell call and its result. ok is false
// when the call says nothing about the environment: no program, another
// hook's denial, a failure without an error line, or a directory outside
// the repository.
func newShellObservation(root, agent, call, command, cwd, text string, failed bool) (shellObservation, bool) {
	parsed := parseShellCommand(command)
	if parsed.intent == "" {
		return shellObservation{}, false
	}
	line, skip := shellErrorLine(text)
	if skip || failed && line == "" {
		return shellObservation{}, false
	}
	dir := cmp.Or(cwd, root)
	if parsed.dir != "" {
		if filepath.IsAbs(filepath.FromSlash(parsed.dir)) {
			dir = filepath.FromSlash(parsed.dir)
		} else {
			dir = filepath.Join(dir, filepath.FromSlash(parsed.dir))
		}
	}
	scope, ok := shellScope(root, dir)
	if !ok {
		return shellObservation{}, false
	}
	observation := shellObservation{
		Call:   call,
		Agent:  agent,
		Intent: parsed.intent,
		Form:   clip(maskShellText(parsed.form, root), 200),
		Scope:  scope,
		Failed: failed,
	}
	if failed {
		observation.Error = clip(maskShellText(line, root), 160)
		observation.Tokens = hookContextTokens(command + text)
	}
	for name, value := range parsed.env {
		if observation.Env == nil {
			observation.Env = make(map[string]string)
		}
		// A value that looks secret, or that cannot be shown on one line, is
		// recorded only as set.
		if shellSecretName.MatchString(name) || len(value) > 64 || strings.ContainsAny(value, " \t\n") || shellTokenLike.MatchString(value) {
			value = ""
		}
		observation.Env[name] = maskShellText(value, root)
	}
	return observation, true
}
