# Current architecture

`cc-search` is a local, derived-data search service for Claude Code and pi
transcripts. This document describes the implementation currently in the
repository; the dated design document beside it is the original proposal.

## Data flow

```text
Claude Code JSONL ─┐
                   ├─ transcript parser ─ SQLite messages + FTS5 ─ CLI / HTTP API
pi session JSONL ──┘                         │
                                             ├─ sessions
                                             └─ pi activities
```

The default roots are `~/.claude/projects` and `~/.pi/agent/sessions`. A CLI
invocation opens the derived index, synchronizes each root, releases its
lifecycle lock, performs the read, and closes the database. The server performs
an initial sync and repeats the same synchronization before data operations.
Missing roots are warnings so a machine using only one agent still works.

## Transcript normalization

`internal/transcript` walks JSONL files recursively and normalizes records with
an ID, timestamp, type, and extractable content into `Message` values. Text,
thinking blocks, tool calls, and tool results are retained. Each message has:

- `Content`: everything searchable, including tool arguments and results
- `Prose`: text that was actually said, used by default for agent-friendly
  searches
- source/session/timestamp metadata
- pi activity linkage when present

Metadata records are not indexed as messages. pi activity markers are parsed
into durable activity rows and related child sessions are linked through the
session table.

## SQLite index

`internal/index` uses SQLite with FTS5, compiled through the mandatory
`sqlite_fts5` Go build tag. The schema contains:

- `messages` and the external-content `messages_fts` table
- `sessions` for compact session browsing and parent/child links
- `activities` for durable pi subagent lifecycle records
- `files` for incremental mtime/size synchronization

FTS5 uses `porter unicode61`. Normal queries are literal, stemmed, prefix
matched, and ANDed. `--any` changes the term join; `--raw` passes a boolean FTS5
expression through after the caller opts into the syntax.

The index is derived entirely from transcript files. Schema version mismatches
are handled safely: an older index is rebuilt, while a binary older than the
index refuses to destroy data written by a newer binary. Confirmed corruption
is rebuilt; transient SQLite busy/locked and environmental I/O errors are
returned. A path-specific advisory lifecycle lock serializes open, sync,
rebuild, and close operations across processes.

## Output contract

`internal/output` renders one JSON object per CLI response and applies the
character budget. Preview mode shares the budget across results and shortens
previews before dropping results. Full mode emits complete bodies in result
order until the budget is exhausted. The accounting fields are always present:

```json
{"limit":60000,"spent":414,"dropped":0,"shrunk":false}
```

The default search space is `prose`; `--all` switches reads and searches to
`content`. Search excludes the current pi session when pi exposes
`PI_SESSION_ID` or `PI_SESSION_FILE`; an explicit `--session` wins.

The `context` operation keeps search hits in relevance order, expands their
neighbor windows chronologically, deduplicates overlaps, and preserves hit IDs
on expanded messages. The `commands` operation parses tool calls and can pair
them with their tool results.

## HTTP API

`internal/server` embeds `internal/server/openapi.json`. The server is
loopback-only by construction, accepts only GET requests for reads and the
specified rebuild operation, and serializes requests with a mutex so a query
cannot race a transcript replacement or rebuild. The same index methods back
the CLI and API, so their search and output semantics remain aligned.

The generated Python and TypeScript clients are checked in for small agent
scripts. `scripts/generate-clients.mjs` validates the OpenAPI paths and rewrites
the generated clients from the checked-in document.
