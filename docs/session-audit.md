# Session audit

`session-audit` reads the transcripts Claude Code keeps for a repository and reports what its sessions cost: how many API rounds they took, how much input each round read, and which tool results filled that input. It makes no model call and writes nothing.

Use it before deciding whether a change to graft's answers is worth building. A benchmark run takes about three rounds over a 13,000-token prompt. A real session takes dozens of rounds over a context of 100,000 to 300,000 tokens, so the same change weighs differently there.

## Run it

From a repository:

```sh
go run ./cmd/session-audit --since 2026-11-01
```

Name other repositories to audit them, each by the path Claude Code was started in:

```sh
go run ./cmd/session-audit --since 2026-11-01 ~/owngo ~/shershen/golang-skills .
```

`--json` prints the same numbers as JSON. `--projects` names Claude Code's projects directory when it is not `~/.claude/projects`. `--since` leaves out conversations last written before that day.

## Read it

```text
/home/coder/owngo
  main: 48 conversations, 5,487 rounds, 26 compactions
    a round reads 196,216 tokens (193,737 from the cache, 2,477 written to it) and writes 689
    graft answered 235 of 929 searches (25%)
    results      calls         chars  rounds carried  share of input
    view           954     3,357,292            87.0            6.8%
    shell        3,194     3,169,036            82.7            6.1%
    search         694     1,323,684            86.3            2.7%
    graft          235       785,310            92.2            1.7%
    a saved round is worth 196,216 tokens; 1,000 tokens of tool result are read again by 85 rounds, as much as 44% of a round
```

- **main** and **subagents** are counted apart, because a subagent has a context of its own.
- **A round** is one API response. It reads the whole context again, nearly all of it from the prompt cache.
- **graft answered N of M searches** compares graft calls (MCP tools and the query commands of the CLI) with raw searches (`grep`, `rg`, `find`, the Grep and Glob tools).
- **results** lists the tool results by kind:
  - `view` is a file read: the Read tool, `cat`, `sed -n`, `head`, `tail`.
  - `search` is a raw search.
  - `graft` is a graft call.
  - `shell` is every other shell command.
- **rounds carried** is how many later rounds read a result again before a compaction or the end of the session.
- **share of input** is what those repeated reads make of all the input the sessions read.

## The two weights

The last line gives the two numbers that price a change:

- **A saved round** is worth the input one round reads.
- **A token of tool result** is paid once for every round that carries it.

A change that adds tokens to an answer pays for itself only if it saves a round often enough. In the example, 1,000 more tokens cost 44% of a round, so the change must save a round in more than 44 of 100 calls. A change that trims 1,000 tokens may cost an extra round in up to 44 of 100 calls and still pay.

Measured on 2026-11-16 over two weeks of sessions in three repositories:

| Repository | Input per round | Rounds a result is carried | 1,000 result tokens, as a share of a round |
|---|---|---|---|
| owngo | 196,216 | 85 | 44% |
| golang-skills | 247,196 | 54 | 22% |
| Graft | 277,290 | 80 | 29% |

In a three-round benchmark run a round reads about 13,000 tokens and a result is carried about two rounds, so the same 1,000 tokens cost about 15% of a round. Real sessions therefore punish a long answer up to three times harder than the benchmark shows, and a saved round is worth fifteen to twenty times more tokens.

## What it does not measure

- **Kinds are approximate.** A shell command counts by the first program it runs, past a leading `cd dir &&`. A command that batches `sed -n …; grep …` counts once, as a view.
- **Characters, not tokens.** Result lengths are UTF-16 units, turned into tokens at about four characters a token for the share of input.
- **Not everything in the context.** System prompt, tool schemas, memory files, the model's own text and its reasoning are in the input per round but not in the results table.
- **The period is by file time.** A conversation counts when its transcript was last written on or after `--since`, and then counts whole.
