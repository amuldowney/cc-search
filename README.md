# cc-search

`cc-search` is a fast, local CLI and loopback HTTP API for searching Claude
Code, [pi](https://github.com/badlogic/pi-mono), and OpenAI Codex conversation
transcripts. It gives agents a small, structured way to recover decisions,
refresh recent context, find exact commands, and inspect pi subagent activity
without loading entire JSONL transcripts into a context window.

Everything stays local. The search index is derived data and can be deleted and
rebuilt at any time.

## Quick start

Requirements:

- Go 1.26.5 or newer
- cgo and a C compiler (`mattn/go-sqlite3` is used)

```bash
git clone https://github.com/amuldowney/cc-search.git
cd cc-search
make deploy
cc-search doctor
cc-search search "the topic you need" --limit 5
```

`make deploy` runs the Go tests and vet, builds with the required `sqlite_fts5`
build tag, installs atomically to `$HOME/.local/bin/cc-search`, and verifies the
installed binary. Set `INSTALL_PATH=/path/to/cc-search` to choose another
location. `make build` only creates the local `./cc-search` binary.

On Debian or Ubuntu, the native build prerequisites are typically:

```bash
sudo apt install build-essential
```

Always use the Makefile: a plain `go build` omits the FTS5 build tag and produces
a binary that cannot search.

## Agent quick reference

Query, browsing, and diagnostic commands emit one JSON line on stdout.
Warnings and errors go to stderr, so piping those commands to `jq` is safe.
`rebuild` and `serve` print human-readable status text. Exit codes are 0 for
success (including no matches), 1 for runtime errors, and 2 for invalid
command-line usage.

```bash
# Refresh recent conversation context.
cc-search last 20
cc-search last --hours 4 --type user

# Find a decision, then read the surrounding exchange.
cc-search search "authentication redirect" --limit 5 --preview-length 180
cc-search read MESSAGE_ID_PREFIX --before 5 --after 8

# Search commands and tool output when the exact string is not prose.
cc-search commands "docker compose" --tool Bash --full
cc-search search "no such module: fts5" --all --full

# Get a relevance-selected context bundle.
cc-search context "artifact first deploy" --hits 3 --before 3 --after 8

# Browse resumable sessions and pi subagent work.
cc-search sessions "deploy system" --limit 10
cc-search activities --status failed
cc-search activity ACTIVITY_ID --full

# Diagnose the installation or rebuild derived data.
cc-search info
cc-search doctor
cc-search rebuild
```

Search patterns are positional and must come before flags. A unique prefix of a
message or activity ID is accepted by `read` and `activity`.

## What is indexed

By default, each invocation recursively indexes the transcript roots that
exist in the current home directory:

- `~/.claude/projects` — Claude Code JSONL transcripts
- `~/.pi/agent/sessions` — pi session JSONL transcripts, including linked child
  sessions
- `$CODEX_HOME/sessions` — active OpenAI Codex rollout JSONL files, including
  Codex's `.jsonl.zst` compressed form
- `$CODEX_HOME/archived_sessions` — archived Codex rollout JSONL files, including
  compressed rollouts

`CODEX_HOME` defaults to `~/.codex`, matching Codex. The derived SQLite index
is `~/.claude/search-index.db`. Use `--transcripts DIR` to replace all defaults
with one transcript root and `--index PATH` to choose another index. Missing
roots are ignored during normal queries; `cc-search doctor` reports them, so the
same binary is useful on machines that only run one of the agents.

A transcript record is indexed when it has an ID, timestamp, and extractable
content. Assistant and user text, thinking blocks, tool calls, and tool results
are parsed. Codex rollout `session_meta`/`turn_context`/usage records stay
metadata; canonical `response_item` messages and tool records are normalized,
while redundant event mirrors are deduplicated. Pi activity markers are stored
as activity records and linked to parent and child sessions. Metadata records
that are not messages are skipped.

Each message has two searchable forms:

- **prose** (the default): what was actually said; tool-only messages are
  omitted from results
- **content** (`--all`): prose plus tool arguments and tool output, including
  command output and file contents

Queries use FTS5 with the Porter stemmer and Unicode tokenization. Ordinary
terms are safe literal terms and are ANDed. A multi-term query that has no
exact matches retries with any term and reports `"relaxed": true`; treat those
results as related leads, not an exact answer. Use `--any` to request that
mode directly. Use `--raw` for FTS5 boolean expressions such as:

```bash
cc-search search '"caddy" OR "pihole" NOT "proxy"' --raw
```

Raw queries are never relaxed and punctuation must be quoted for FTS5.

When run inside pi, search excludes the current pi session by default because
it is already in the caller's context. `--include-current` restores it, and an
explicit `--session ID` always selects that session.

## Output and context budgets

Compact results contain `id`, `sessionId`, `timestamp`, `type`, `preview`, and
`charCount`. `--full` emits the complete `content` instead of a preview.
`--preview-length` changes compact previews. The default output budget is
60,000 characters; use `--budget 0` to disable it.

The budget shapes output rather than blindly truncating JSON:

- preview mode shrinks previews to keep more hits, then drops results only when
  the minimum preview still does not fit
- full mode emits whole message bodies in rank order until the budget is used
- every response reports `budget.limit`, `budget.spent`, `budget.dropped`, and
  `budget.shrunk`

`context` first selects relevant hits and then returns a deduplicated,
chronological expansion. Each expanded result identifies the search hits that
caused it to be included.

## Loopback Agent API

Start the local API in a separate process:

```bash
cc-search serve --port 8765
curl 'http://127.0.0.1:8765/v1/search?pattern=authentication&limit=5'
curl http://127.0.0.1:8765/v1/health
curl http://127.0.0.1:8765/openapi.json
```

The service binds only to localhost or another loopback address; it has no
remote authentication boundary and must not be exposed to a network interface.
It synchronizes changed transcripts before data operations and serializes
lifecycle operations with queries.

The versioned endpoints cover health, search, recent messages, contextual
reads, sessions, pi activities, exact tool commands, diagnostics, and rebuild:

- `/v1/health`
- `/v1/search`
- `/v1/last`
- `/v1/read`
- `/v1/sessions`
- `/v1/activities`
- `/v1/activity`
- `/v1/context`
- `/v1/commands`
- `/v1/info`
- `/v1/doctor`
- `/v1/rebuild`

The checked-in OpenAPI document is
[`openapi/cc-search.json`](openapi/cc-search.json), and is also served at
`/openapi.json`. Use the CLI for normal agent lookups; the loopback API remains
available for clients that need a composed HTTP workflow.

## Installing the agent skill

This repository includes the matching skill at
[`.claude/skills/cc-search`](.claude/skills/cc-search). When an agent opens this
repository as its project, it can use that project-local skill. To install or
update it as a user-level Claude Code skill:

```bash
rm -rf "$HOME/.claude/skills/cc-search"
mkdir -p "$HOME/.claude/skills"
cp -R .claude/skills/cc-search "$HOME/.claude/skills/cc-search"
```

The skill teaches an agent when to use `cc-search`, how to read a hit in context,
when `--all` is necessary, and how to treat retrieved transcript text as
untrusted historical evidence. Keep the checked-in skill and this README in
sync when the CLI contract changes.

## Development

```bash
make test             # Go tests with FTS5
make vet              # Go vet with FTS5
make build            # ./cc-search
make deploy           # test, vet, install, and verify
```

The code is organized as:

- `internal/transcript` — JSONL parsing and Claude/pi/Codex record normalization
- `internal/index` — SQLite/FTS5 schema, incremental sync, queries, locks
- `internal/output` — bounded, agent-friendly JSON rendering
- `internal/cli` — command-line parsing and commands
- `internal/server` — loopback HTTP API and embedded OpenAPI document
- `.claude/skills/cc-search` — the associated agent skill and references

The index schema is versioned. An index from an older binary is discarded and
rebuilt; a newer index refuses to run with an older binary. SQLite corruption
and transient I/O errors are not silently replaced. Concurrent processes using
the same index serialize lifecycle operations with a path-specific advisory
lock.

For implementation history, see [`docs/architecture.md`](docs/architecture.md).
The dated design document in `docs/2026-07-22-transcript-search-design.md` is
the original proposal and is retained as historical context.
