// Package sessionaudit measures real Claude Code sessions from their
// transcripts: how many API rounds they took, how much input each round
// read, and which tool results filled it.
//
// Two weights come out of it: what one round costs and how long a tool
// result is carried. They say what a change to graft's answers is worth in
// sessions as people run them, which a benchmark of a few rounds does not
// show.
package sessionaudit

import (
	"cmp"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/h0rn3t/Graft/internal/savings"
)

// A Report holds what the sessions of one repository cost.
type Report struct {
	// Repo is the repository's path as Claude Code saw it.
	Repo string `json:"repo"`
	// Main counts the conversations a person started, Subagents the ones
	// those started. A subagent has a context of its own.
	Main      Group `json:"main"`
	Subagents Group `json:"subagents"`
}

// A Group sums conversations of one sort.
type Group struct {
	Conversations int `json:"conversations"`
	// Rounds counts the API responses; each read the whole context again.
	Rounds      int `json:"rounds"`
	Compactions int `json:"compactions"`

	// Tokens, as the API billed them over all rounds:
	Input      int `json:"inputTokens"` // read outside the prompt cache
	CacheWrite int `json:"cacheWriteTokens"`
	CacheRead  int `json:"cacheReadTokens"`
	Output     int `json:"outputTokens"`

	// Tools holds the tool calls by kind: "graft", "search" (grep, find and
	// the like), "view" (a file read), "edit", "shell" and "other".
	Tools map[string]Tool `json:"tools,omitempty"`
}

// A Tool sums the calls of one kind.
type Tool struct {
	Calls int `json:"calls"`
	// Chars is the length of the results, in UTF-16 units.
	Chars int `json:"resultChars,omitzero"`
	// CharRounds is, summed over the results, the length of one times the
	// rounds that read it again before a compaction or the session's end.
	CharRounds int `json:"resultCharRounds,omitzero"`
}

// Audit reads the transcripts Claude Code keeps for repo under projects, its
// projects directory, leaving out those last written before since.
func Audit(projects, repo string, since time.Time) (Report, error) {
	report := Report{Repo: repo}
	root, err := os.OpenRoot(projects)
	if err != nil {
		return report, fmt.Errorf("open Claude Code's projects directory: %w", err)
	}
	defer func() { _ = root.Close() }() // The read-only root has no buffered state to flush.

	// Claude Code names a project's directory after its path, with every
	// character that is not a letter or a digit turned into a dash.
	project := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, repo)
	if err := report.Main.readAll(root, project+"/*.jsonl", since); err != nil {
		return report, err
	}
	err = report.Subagents.readAll(root, project+"/*/subagents/*.jsonl", since)
	return report, err
}

// readAll adds the transcripts that match pattern and were written at or
// after since.
func (g *Group) readAll(root *os.Root, pattern string, since time.Time) error {
	names, err := fs.Glob(root.FS(), pattern)
	if err != nil {
		return err
	}
	for _, name := range names {
		info, err := root.Stat(name)
		if err != nil || info.ModTime().Before(since) {
			continue // A transcript removed since the listing counts as an old one.
		}
		transcript, err := root.Open(name)
		if err != nil {
			return fmt.Errorf("read transcript: %w", err)
		}
		g.read(transcript)
		_ = transcript.Close() // Read-only: nothing is lost with the error.
	}
	return nil
}

type transcriptLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		ID    string `json:"id"`
		Usage *struct {
			Input      int `json:"input_tokens"`
			CacheWrite int `json:"cache_creation_input_tokens"`
			CacheRead  int `json:"cache_read_input_tokens"`
			Output     int `json:"output_tokens"`
		} `json:"usage"`
		Content jsontext.Value `json:"content"`
	} `json:"message"`
}

// A contentBlock is one block of a message: a tool call, a tool result or text.
type contentBlock struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Input     jsontext.Value `json:"input"`
	ToolUseID string         `json:"tool_use_id"`
	Text      string         `json:"text"`
	Content   jsontext.Value `json:"content"`
}

// lenient reads what Claude Code wrote even where strict JSON would refuse it:
// a tool's output can hold bytes that are not UTF-8.
var lenient = json.JoinOptions(jsontext.AllowInvalidUTF8(true), jsontext.AllowDuplicateNames(true))

// read adds one conversation. It stops at the first line that is not whole
// JSON, since Claude Code may still be writing the transcript, and passes
// over a line whose fields are not of the types read here.
func (g *Group) read(transcript io.Reader) {
	g.Conversations++
	type result struct {
		kind         string
		chars, round int
	}
	var (
		dec     = jsontext.NewDecoder(transcript, lenient)
		rounds  = 0                         // rounds of this conversation so far
		counted = make(map[string]struct{}) // message IDs: a response of several blocks takes several lines
		kinds   = make(map[string]string)   // tool call ID → kind
		carried []result                    // results in the context since the last compaction
	)
	settle := func() {
		for _, r := range carried {
			tool := g.Tools[r.kind]
			tool.CharRounds += r.chars * (rounds - r.round)
			g.Tools[r.kind] = tool
		}
		carried = carried[:0]
	}
	for {
		raw, err := dec.ReadValue()
		if err != nil {
			break
		}
		var line transcriptLine
		if json.Unmarshal(raw, &line, lenient) != nil {
			continue
		}
		message := line.Message
		var blocks []contentBlock
		if message.Content.Kind() == '[' {
			_ = json.Unmarshal(message.Content, &blocks, lenient) // Blocks of another shape are not tool calls.
		}
		switch {
		case line.Type == "system" && line.Subtype == "compact_boundary":
			g.Compactions++
			settle()
		case line.Type == "assistant":
			if _, seen := counted[message.ID]; !seen && message.Usage != nil {
				counted[message.ID] = struct{}{}
				rounds++
				g.Rounds++
				g.Input += message.Usage.Input
				g.CacheWrite += message.Usage.CacheWrite
				g.CacheRead += message.Usage.CacheRead
				g.Output += message.Usage.Output
			}
			for _, block := range blocks {
				if block.Type != "tool_use" {
					continue
				}
				if g.Tools == nil {
					g.Tools = make(map[string]Tool)
				}
				kind := toolKind(block.Name, block.Input)
				kinds[block.ID] = kind
				tool := g.Tools[kind]
				tool.Calls++
				g.Tools[kind] = tool
			}
		case line.Type == "user":
			for _, block := range blocks {
				kind, called := kinds[block.ToolUseID]
				if block.Type != "tool_result" || !called {
					continue
				}
				chars := resultLength(block.Content)
				tool := g.Tools[kind]
				tool.Chars += chars
				g.Tools[kind] = tool
				carried = append(carried, result{kind: kind, chars: chars, round: rounds})
			}
		}
	}
	settle()
}

// resultLength measures a tool result, which is a string or a list of text blocks.
func resultLength(content jsontext.Value) int {
	var text string
	if json.Unmarshal(content, &text, lenient) == nil {
		return savings.Length(text)
	}
	var blocks []contentBlock
	_ = json.Unmarshal(content, &blocks, lenient) // A result of another shape, an image say, has no text to count.
	chars := 0
	for _, block := range blocks {
		chars += savings.Length(block.Text)
	}
	return chars
}

// graftQueries are the graft commands that answer a question about the code.
var graftQueries = []string{"ask", "read", "skeleton", "callers", "blast", "grep", "map", "path", "show"}

// toolKind sorts a tool call by the work it does. A shell command is sorted
// by the first program it runs, past a leading `cd dir &&`, so a command
// that batches several programs counts once, as the first.
func toolKind(name string, input jsontext.Value) string {
	switch {
	case strings.HasPrefix(name, "mcp__graft__"):
		return "graft"
	case name == "Grep", name == "Glob":
		return "search"
	case name == "Read":
		return "view"
	case name == "Edit", name == "Write", name == "MultiEdit", name == "NotebookEdit":
		return "edit"
	case name != "Bash":
		return "other"
	}
	var shell struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &shell, lenient) // A command that is not text sorts as plain shell work.
	words := strings.Fields(shell.Command)
	if len(words) >= 3 && words[0] == "cd" && (words[2] == "&&" || words[2] == ";") {
		words = words[3:]
	}
	if len(words) == 0 {
		return "shell"
	}
	args := words[1:]
	switch path.Base(words[0]) {
	case "grep", "egrep", "fgrep", "rg", "ugrep", "ag", "ack", "find", "fd", "bfs":
		return "search"
	case "git":
		if len(args) > 0 && args[0] == "grep" {
			return "search"
		}
	case "graft":
		if len(args) > 0 && slices.Contains(graftQueries, args[0]) {
			return "graft"
		}
	case "cat", "head", "tail", "nl":
		writes := func(arg string) bool { return strings.HasPrefix(arg, ">") || strings.HasPrefix(arg, "<<") }
		if !slices.ContainsFunc(args, writes) {
			return "view"
		}
	case "sed":
		if len(args) > 0 && args[0] == "-n" {
			return "view"
		}
	}
	return "shell"
}

// RoundTokens is the input an API round reads on average, cached or not:
// what a change that saves one round is worth.
func (g Group) RoundTokens() int {
	if g.Rounds == 0 {
		return 0
	}
	return (g.Input + g.CacheWrite + g.CacheRead) / g.Rounds
}

// CarriedRounds is how many later rounds read a character of tool result
// again, on average: what a longer answer costs beyond its own length.
func (g Group) CarriedRounds() float64 {
	chars, charRounds := 0, 0
	for _, tool := range g.Tools {
		chars += tool.Chars
		charRounds += tool.CharRounds
	}
	if chars == 0 {
		return 0
	}
	return float64(charRounds) / float64(chars)
}

// Searches counts the calls graft answered and the raw searches beside them.
func (g Group) Searches() (graft, raw int) {
	return g.Tools["graft"].Calls, g.Tools["search"].Calls
}

// Human renders the report for a terminal.
func (r Report) Human() string {
	var b strings.Builder
	fmt.Fprintln(&b, r.Repo)
	if r.Main.Conversations+r.Subagents.Conversations == 0 {
		b.WriteString("  no conversations in the period\n")
		return b.String()
	}
	r.Main.write(&b, "main")
	r.Subagents.write(&b, "subagents")
	return b.String()
}

func (g Group) write(b *strings.Builder, name string) {
	if g.Conversations == 0 {
		return
	}
	count := func(n int, noun string) string {
		if n == 1 {
			return "1 " + noun
		}
		return savings.Group(n) + " " + noun + "s"
	}
	read := g.Input + g.CacheWrite + g.CacheRead
	fmt.Fprintf(b, "  %s: %s, %s, %s\n", name, count(g.Conversations, "conversation"), count(g.Rounds, "round"), count(g.Compactions, "compaction"))
	if g.Rounds == 0 {
		return
	}
	fmt.Fprintf(b, "    a round reads %s tokens (%s from the cache, %s written to it) and writes %s\n",
		savings.Group(g.RoundTokens()), savings.Group(g.CacheRead/g.Rounds), savings.Group(g.CacheWrite/g.Rounds), savings.Group(g.Output/g.Rounds))
	if graft, raw := g.Searches(); graft+raw > 0 {
		fmt.Fprintf(b, "    graft answered %s of %s searches (%d%%)\n", savings.Group(graft), savings.Group(graft+raw), 100*graft/(graft+raw))
	}
	kinds := slices.SortedFunc(func(yield func(string) bool) {
		for kind := range g.Tools {
			if !yield(kind) {
				return
			}
		}
	}, func(a, b string) int {
		return cmp.Or(cmp.Compare(g.Tools[b].CharRounds, g.Tools[a].CharRounds), cmp.Compare(a, b))
	})
	fmt.Fprintf(b, "    %-8s %9s %13s %15s %15s\n", "results", "calls", "chars", "rounds carried", "share of input")
	for _, kind := range kinds {
		tool := g.Tools[kind]
		carried := 0.0
		if tool.Chars > 0 {
			carried = float64(tool.CharRounds) / float64(tool.Chars)
		}
		fmt.Fprintf(b, "    %-8s %9s %13s %15.1f %14.1f%%\n", kind, savings.Group(tool.Calls), savings.Group(tool.Chars),
			carried, 100*float64(savings.Tokens(tool.CharRounds))/float64(max(read, 1)))
	}
	fmt.Fprintf(b, "    a saved round is worth %s tokens; 1,000 tokens of tool result are read again by %.0f rounds, as much as %.0f%% of a round\n",
		savings.Group(g.RoundTokens()), g.CarriedRounds(), 100*1000*g.CarriedRounds()/float64(max(g.RoundTokens(), 1)))
}
