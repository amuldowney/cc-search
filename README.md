# cc-search

A fast local CLI for searching Claude Code and pi conversation transcripts.
Built for agents to pad context, refresh session memory, and dig up concepts
from earlier conversations without re-reading whole transcripts.

Both transcript stores are indexed by default: `~/.claude/projects` and
`~/.pi/agent/sessions`, each walked recursively so every project is covered.
`--transcripts DIR` overrides both with a single root.

Design: [docs/2026-07-22-transcript-search-design.md](docs/2026-07-22-transcript-search-design.md).

## Build

FTS5 is not compiled into `go-sqlite3` by default, so the `sqlite_fts5` build
tag is required — use the Makefile and it is never forgotten.

```bash
make test
make build            # ./cc-search
make install          # $HOME/.local/bin/cc-search
```

## Agent API and clients

`cc-search serve` exposes the same search operations through a versioned HTTP
API bound to loopback. The port is configurable, and the service synchronizes
transcripts before data operations:

```bash
cc-search serve --port 8765
curl 'http://127.0.0.1:8765/v1/search?pattern=authentication&limit=5'
curl http://127.0.0.1:8765/openapi.json
```

The OpenAPI document is [internal/server/openapi.json](internal/server/openapi.json)
and is also available as `openapi/cc-search.json`. The checked-in agent clients
are generated from it:

```bash
make generate-clients
make test-clients
```

- Python: `clients/python/cc_search_client`
- TypeScript: `clients/typescript/src`

Both clients use structured responses and typed HTTP errors. They are intended
for short agent scripts that compose search, selection, and context reads; use
the CLI for a one-off lookup. The HTTP service is local-only and has no remote
authentication boundary.

## Use

Every run syncs the index before querying, so the data is always current. The
first run builds the whole index; later runs only reindex transcripts whose
mtime or size changed.

```bash
# memory refresh
cc-search last 10
cc-search last --hours 2 --session 62de7038-83f9-4ca9-8b2b-165ab6c70d47
cc-search last 30 --session 62de7038-83f9-4ca9-8b2b-165ab6c70d47 --type user

# concept research
cc-search search "grayscale waveform"
cc-search search "display.cpp" --limit 5 --window-hours 48
cc-search search "retry loop" --include-current  # include this pi session
cc-search search "no such module: fts5" --all   # include tool calls + output
cc-search search "battery dispatch reserve" --any  # any term, not all
cc-search search '("caddy" OR "pihole") NOT "proxy"' --raw   # boolean logic

# read a hit in context (id, or any unique prefix, from a search result)
cc-search read 36b182bb --before 3 --after 3

# browse and resume an earlier session
cc-search sessions "deploy system" --limit 10
cc-search sessions --cwd /home/andrew/Projects/pi-remote

# inspect Pi subagent work
cc-search activities --session SESSION_ID
cc-search activities --status failed
cc-search activity ACTIVITY_ID --full

# search several hits and expand their surrounding windows
cc-search context "artifact-first deploy" --hits 3 --before 3 --after 8 --all

# recover an exact historical tool invocation (and its output)
cc-search commands "no such table" --tool Bash --full
cc-search commands "docker compose" --session SESSION_ID

# index and installation diagnostics
cc-search info
cc-search doctor

# index maintenance (rarely needed — sync is automatic)
cc-search rebuild
cc-search rebuild --session 62de7038-83f9-4ca9-8b2b-165ab6c70d47
```

Concurrent processes using the same index serialize opening and synchronization
through a path-specific lifecycle lock. Waiting is bounded; a timeout names the
index and wait duration. The lock uses advisory Unix file locking and is
supported on Unix targets (Linux, macOS, BSD, and illumos); no Windows lock
backend is provided. Transient SQLite busy/locked errors and
environmental I/O errors are returned without replacing the existing index; only
confirmed corruption or an incompatible schema is rebuilt automatically.

Terms are stemmed with porter and prefix matched, so `cache` finds `caching`
without matching anything inside `display.cpp` or `192.168.1.112`. Terms are
ANDed. When a multi-word query matches nothing, the search retries
with any term rather than reporting the topic was never discussed, and says so
with `"relaxed": true` plus a note on stderr — those hits are related, not
exact. `--any` asks for that behaviour up front.

When running inside pi, `search` excludes the current session by default because
its contents are already in the caller's context. `--include-current` restores
it, and an explicit `--session ID` always selects that session. `last` and
`read` are unchanged, so they can still recover the current conversation.

`--raw` hands the pattern to FTS5 verbatim, so `OR`, `NOT`, `NEAR`, parentheses
and `"quoted phrases"` all work. Punctuation must then be quoted yourself —
bare `display.cpp` is a syntax error in that mode, `"display.cpp"` is fine. A
raw query is never relaxed, since it says exactly what was meant.

`read` takes a message id from a search result — any unique prefix works — and
returns it with its neighbours from the same session, oldest first. That is how
you recover *why* something was decided rather than just the sentence that
decided it.

- `sessions` returns compact resumable-session rows with the working directory, visibility, parent session, last message, and message count. A query matches session prose; `--cwd` filters by exact working directory.
- `activities` and `activity` expose durable Pi activity records, including parent/child session links and terminal status. `activity ID --full` adds marker/linkage fields.
- `context` keeps selected search hits in relevance order, then returns one deduplicated chronological expansion. Each expanded message has `hitIds`, and the top-level `query` preserves why the context was collected.
- `commands` searches full content but returns normalized tool name, structured arguments, source session/message id, and timestamp. `--full` or `--include-output` pairs the invocation with its recorded result.
- `info` reports schema and indexed-source counts, executable identity, current session, and health. `doctor` adds named checks and reports `ok`.

Output is capped at 60,000 characters by default (`--budget N`, `0` disables),
and the cap *shapes* the result set rather than just chopping its tail:
previews shrink so every result still fits, down to a 40-character floor, and
only then are results dropped. Under `--full` the opposite is right — bodies
were asked for whole, so they are emitted whole until the budget runs out.

Every response reports what it cost:

```json
"budget": {"limit": 60000, "spent": 414, "dropped": 0, "shrunk": false}
```

The search pattern is positional and must come before the flags — putting a
flag there is rejected rather than searched for.

Output is one line of JSON on stdout; warnings and errors go to stderr, so
`cc-search ... | jq` is always safe. Exit 0 (including for zero results), 1 on
error, 2 on a usage mistake.

```json
{"results":[{"id":"...","sessionId":"...","timestamp":"2026-07-23T04:01:04Z",
"type":"assistant","preview":"Done. To recap: …","charCount":342}],
"total":1,"truncated":false,"relaxed":false,
"budget":{"limit":60000,"spent":342,"dropped":0,"shrunk":false}}
```

Compact output contains a single-line `preview` field capped at 100 characters
(`--preview-length N`). `--full` replaces it with the complete `content` field;
the two fields are never emitted together. Full content preserves newlines, and
`--preview-length N` only affects compact output. `truncated` is true when
`--limit` cut results off.

Paths default to `~/.claude/projects/-home-andrew-Projects` and
`~/.claude/search-index.db`, overridable with `--transcripts` and `--index`.

## What gets indexed

One row per transcript record that has a `uuid`, a `timestamp`, and extractable
content. Assistant/user messages contribute their text and thinking blocks,
tool calls are rendered as `[tool: Name] {args}`, and tool results are indexed
in full — so searches hit command output and file contents too. Metadata
records (`mode`, `last-prompt`, `file-history-*`, hook attachments) are skipped.

**Prose vs content.** Each message is indexed twice: `content` (everything,
including tool arguments and command output) and `prose` (only what was
actually said). **Queries read prose by default** — matching, previews and
`--full` all use it, and messages that only made a tool call drop out
entirely. Without this a query like "caddy proxy" is dominated by
`[tool: Edit] {...}` messages whose arguments happen to contain the words.

`--all` switches to the full content, which is what you want when hunting a
command, a path, an error string, or something that only ever appeared in
command output.

**Subagent activities.** Pi activity markers are ingested separately from
messages. Attached child sessions are linked through their `activityId`,
`parentActivityId`, `parentSessionId`, and `childSessionId`; parent Agent
start/response rows and child messages expose those fields in results. The index recognizes
`pi:activity-started`, `pi:activity-linked`, and `pi:activity-terminal` records
without treating them as searchable messages. An attached session header also
provides a fallback link when markers are incomplete. Legacy transcripts without
these markers remain ordinary session messages.

**Schema version.** The index records a version; an index written by an older
build is discarded and rebuilt on open rather than migrated.

## Measured on the real corpus

86 sessions, 20,210 messages:

| | |
|---|---|
| Full rebuild | 6.5s |
| Query (`last`, `search`) | 0.15–0.32s including the incremental sync |
| Index size | 40 MB |

The index is larger than the design's 1 MB estimate because tool results — file
dumps, command output — dominate the corpus. It is fully derived from the
transcripts and can be deleted at any time; schema/obvious database corruption
is detected on open and rebuilt automatically. The full integrity scan is
available through `cc-search doctor` rather than running before every query.
