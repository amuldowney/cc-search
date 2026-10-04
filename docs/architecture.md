# Current architecture

`cc-search` is a local, derived-data search service for Claude Code, pi, and
Codex transcripts. This document describes the implementation currently in the
repository; the dated design document beside it is the original proposal.

## Data flow

```text
Claude Code JSONL ─┐
pi session JSONL ───┼─ transcript parser ─ SQLite messages + FTS5 ─ CLI / HTTP API
Codex rollout JSONL ┘                         │
                                              ├─ sessions
                                              └─ pi activities
```

The default roots are `~/.claude/projects`, `~/.pi/agent/sessions`,
`$CODEX_HOME/sessions`, and `$CODEX_HOME/archived_sessions`, where
`CODEX_HOME` defaults to `~/.codex`. Codex's plain `.jsonl` and compressed
`.jsonl.zst` rollouts are both supported. Normal CLI/API reads open an existing
read-only WAL snapshot without acquiring the writer lifecycle lock. They request
background refresh when stale, coalesced at most once per 30 seconds. CLI children
have null stdio and a detached session; HTTP owns a single-flight worker. A busy
writer makes opportunistic refresh skip, not queue. `--refresh`, API
`refresh=true`, and the explicit refresh commands wait for synchronization.
Bootstrap, schema upgrades, and new root profiles wait before serving results.
Missing roots are ignored normally and reported by CLI `doctor`.

## Transcript normalization

`internal/transcript` walks JSONL files recursively and normalizes Claude Code,
pi, and Codex records with an ID, timestamp, type, and extractable content into
`Message` values. Text, thinking blocks, tool calls, and tool results are
retained. Each message has:

- `Content`: everything searchable, including tool arguments and results
- `Prose`: text that was actually said, used by default for agent-friendly
  searches
- source/session/timestamp metadata
- pi activity linkage when present

Metadata records are not indexed as messages. Codex rollout metadata such as
`session_meta`, `turn_context`, and token accounting is skipped; its canonical
`response_item` messages and tool calls/results are normalized, with duplicate
`event_msg` mirrors suppressed. Pi activity markers are parsed into durable
activity rows and related child sessions are linked through the session table.

## SQLite index

`internal/index` uses SQLite with FTS5, compiled through the mandatory
`sqlite_fts5` Go build tag. The schema contains:

- `messages` and the external-content `messages_fts` table
- `sessions` for compact session browsing and parent/child links
- `activities` for durable pi subagent lifecycle records
- `files` for incremental mtime/size synchronization
- `message_projections` for normalized preview prefixes and original byte counts
- `source_projections` for per-transcript summaries/counts
- `tool_invocations` / `tool_results` for indexed, session-scoped pairing
- `refresh_state` for per-root-set freshness and retry throttling

Unrestricted FTS searches filter matched rows directly; only message-count
windows construct a history subquery. Session FTS executes once rather than per
session. Global/type/source timestamp indexes serve recent-history queries.

FTS5 uses `porter unicode61`. Normal queries are literal, stemmed, prefix
matched, and ANDed. `--any` changes the term join; `--raw` passes a boolean FTS5
expression through after the caller opts into the syntax.

The index is derived entirely from transcript files. Schema v7 upgrades to v8
additively, preserving messages/FTS rowids while backfilling read projections.
Older incompatible schemas require reindexing; a binary older than the index
refuses to destroy data written by a newer binary. Ordinary read-only opens do
not write schema metadata, remove database files, or recover corruption.
Transient SQLite busy/locked and environmental I/O errors are returned.

A path-specific advisory lock serializes writers across processes, not readers.
Per-root-set freshness records last attempt and successful completion separately;
failed refreshes preserve lastSuccess and expose only a sanitized error category
in diagnostics. File replacements remain atomic transactions. The interval is
opportunistic, not a freshness bound: an idle index can be much older until the
next refresh finishes. Explicit refresh provides the strong freshness path.

## Output contract

`internal/output` renders one JSON object per CLI response and applies the
character budget. Preview mode shares the budget across results and shortens
previews before dropping results. Full mode emits complete bodies in result
order until the budget is exhausted. The accounting fields are always present:

```json
{"limit":60000,"spent":414,"dropped":0,"shrunk":false}
```

The default search space is `prose`; `--all` switches reads and searches to
`content`. Missing transcript roots stay silent here and are surfaced by
`doctor`. Search excludes the current pi session when pi exposes
`PI_SESSION_ID` or `PI_SESSION_FILE`; an explicit `--session` wins.

The `context` operation keeps search hits in relevance order, expands their
neighbor windows chronologically, deduplicates overlaps, and preserves hit IDs
on expanded messages. The `commands` operation uses index-time tool call/result links and loads output
only when requested.

## HTTP API

`internal/server` embeds `internal/server/openapi.json`. The server is
loopback-only by construction, accepts GET requests for reads and POST for
refresh/rebuild operations. Each request owns a reader connection; a mutex only
protects worker scheduling/shutdown, never filesystem indexing or queries. The
same index methods back the CLI and API, so search/output semantics stay aligned.
Search defaults to 20 hits; explicit limit 0 remains unlimited.

The OpenAPI document is kept in `internal/server/openapi.json` and exposed at
`/openapi.json`; agents normally use the CLI, while composed workflows can use
plain HTTP against the loopback service.
