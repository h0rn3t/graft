package hosts

import (
	"slices"
	"strings"

	"github.com/h0rn3t/Graft/internal/jsonjs"
)

// Legacy shim names: entries written before hooks ran the binary directly
// start graft through one of these Node scripts.
const (
	legacyHooksShim      = "graft-hooks.cjs"
	legacyStatuslineShim = "graft-statusline.cjs"
)

// IsGraftHookCommand reports whether a host hook command runs graft's hook
// runtime: directly, as `<graft> _hook <sub>`, or through a legacy shim. The
// executable is not matched by name, since it may be a renamed dev or test
// binary.
func IsGraftHookCommand(command string) bool {
	return strings.Contains(command, legacyHooksShim) || runsEntryPoint(command, "_hook")
}

func isGraftStatuslineCommand(command string) bool {
	return strings.Contains(command, legacyStatuslineShim) || runsEntryPoint(command, "_statusline")
}

// runsEntryPoint reports whether the first argument after the executable in
// command is entry. The executable is a bare word or a path quoted the way
// shellPath quotes it.
func runsEntryPoint(command, entry string) bool {
	end := strings.IndexAny(command, " \t")
	if command != "" && (command[0] == '"' || command[0] == '\'') {
		end = -1
		// A single-quoted path escapes its own quotes as '\'', so the closing
		// quote is the first one followed by whitespace.
		for i := 1; i+1 < len(command); i++ {
			if command[i] == command[0] && (command[i+1] == ' ' || command[i+1] == '\t') {
				end = i + 1
				break
			}
		}
	}
	if end < 0 {
		return false
	}
	args := strings.Fields(command[end:])
	return len(args) > 0 && args[0] == entry
}

// isGraftEntry reports whether a hooks-config entry is one graft installed:
// a command of its own (Cursor) or of any handler under hooks (Claude Code,
// Codex) runs graft's hook runtime.
func isGraftEntry(entry jsonjs.Value) bool {
	object, ok := jsonjs.AsObject(entry)
	if !ok {
		return false
	}
	handlers, _ := jsonjs.AsArray(mustGet(object, "hooks"))
	return isGraftHandler(object) || slices.ContainsFunc(handlers, isGraftHandler)
}

// isGraftHandler reports whether a hook object's own command runs graft's
// hook runtime.
func isGraftHandler(handler jsonjs.Value) bool {
	object, ok := jsonjs.AsObject(handler)
	if !ok {
		return false
	}
	command, ok := mustGet(object, "command").(string)
	return ok && IsGraftHookCommand(command)
}

// withoutGraftHooks returns entries with graft's hooks taken out. An entry
// whose own command runs graft (Cursor) is dropped. In a handler block
// (Claude Code, Codex) only graft's handlers go: the matcher and the other
// handlers stay in their order, and the block is dropped only when no
// handler is left. A trimmed block is a copy: callers merge into shallow
// clones whose nested blocks the original settings still share.
func withoutGraftHooks(entries []jsonjs.Value) []jsonjs.Value {
	kept := make([]jsonjs.Value, 0, len(entries))
	for _, entry := range entries {
		if !isGraftEntry(entry) {
			kept = append(kept, entry)
			continue
		}
		object, _ := jsonjs.AsObject(entry)
		if isGraftHandler(object) {
			continue
		}
		handlers, _ := jsonjs.AsArray(mustGet(object, "hooks"))
		rest := slices.DeleteFunc(slices.Clone(handlers), isGraftHandler)
		if len(rest) == 0 {
			continue
		}
		trimmed := object.Clone()
		trimmed.Set("hooks", rest)
		kept = append(kept, trimmed)
	}
	return kept
}
