# Codex 0.166.0 PostToolUse payloads

Captured on 2026-11-01 from Codex CLI 0.166.0, model `gpt-6.1-sol`, in an
empty temporary directory with an isolated `CODEX_HOME`. The normal ChatGPT
login flow was completed. The stdin recorder was reviewed and trusted through
the interactive `/hooks` UI. Neither hook trust nor sandboxing was bypassed.
The user's existing Codex configuration and credentials were not copied.

The launch command was:

```sh
codex --no-daemon --sandbox read-only --ask-for-approval on-request --cd <temporary-directory>
```

The temporary `hooks.json` registered a synchronous command hook for
`PostToolUse`, without a matcher and with a 10-second timeout. Its Python
recorder read `sys.stdin.buffer` and wrote those bytes unchanged to a new
private file. Each fixture is a real hook input, not a rollout event or a
handwritten approximation. `capture-manifest.json` records the SHA-256 of
each original stdin file and the exit status shown in the Codex TUI.

| Fixture | Process exit status | Hook response |
| --- | --- | --- |
| `success.json` | 0 | `GRAFT_CODEX_CAPTURE_OK` |
| `failure-error-line.json` | 7 | `error: GRAFT_CODEX_CAPTURE_FAILURE` |
| `failure-without-error-line.json` | 7 | `GRAFT_CODEX_CAPTURE_FAILURE` |

All three inputs identify the shell tool as `Bash`. Their `tool_response`
is a plain string containing command output only. The input contains no
numeric exit status or error flag, and the response contains no exit-status
header or trailer. The failing input with an error line therefore exercises
the outcome reader's final text fallback. The failure without an error line
is indistinguishable from a successful call at this boundary: the reader
returns `failed=false`, as the fail-open design specifies. These captures do
not establish the response shape of every Codex shell execution path.

The commands and response strings are unchanged. The JSON was pretty-printed,
and only these fields were normalized:

- `cwd`: `/capture/repo`.
- `transcript_path`: `/capture/codex-home/sessions/rollout-capture.jsonl`.
- `session_id` and `turn_id`: fixed UUID-shaped values, preserving which
  commands shared a session and turn.
- `tool_use_id`: `call_capture_1`, `call_capture_2`, or `call_capture_3`.

The manifest's process exit statuses are corroborating capture metadata;
they were not inserted into the hook payloads.

See the [official hook trust documentation](https://learn.chatgpt.com/docs/hooks#review-and-trust-hooks).
