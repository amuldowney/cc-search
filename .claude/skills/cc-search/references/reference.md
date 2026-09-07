# cc-search reference

## Commands

```text
cc-search last [N] [flags]        newest messages (default N=10)
cc-search search PATTERN [flags]  full-text search
cc-search read ID [flags]         a message plus same-session neighbors
cc-search sessions [QUERY]        browse sessions to resume
cc-search activities [flags]      inspect pi activity records
cc-search activity ID [flags]    inspect one activity
cc-search context PATTERN         search hits plus expanded context
cc-search commands PATTERN        retrieve exact historical tool calls
cc-search info                    index and installation details
cc-search doctor                  index health checks
cc-search rebuild                 discard and rebuild derived data
cc-search serve                   loopback OpenAPI HTTP API
```

Patterns and IDs are positional and must come before flags. Multi-word patterns
must be quoted. A unique ID prefix is accepted by `read` and `activity`.

## CLI flags

### Shared data/output flags

These are available on commands that open the index:

| Flag | Effect |
|---|---|
| `--index PATH` | derived index database (default `~/.claude/search-index.db`) |
| `--transcripts DIR` | replace both default transcript roots with one root |
| `--session ID` | restrict to a session where the command supports it |
| `--all` | search/render full content, including tool calls and output |
| `--full` | emit full content or full activity/command details |
| `--preview-length N` | compact preview length, default 100 characters |
| `--budget N` | output character cap, default 60000; `0` disables it |

### `last`

```text
--hours H       only messages from the last H hours
--type TYPE     restrict to user, assistant, system, ...
```

`last` defaults to 10 messages. It returns newest messages first unless the
command is `read`, which returns its context oldest first.

### `search`

```text
--limit N             maximum results; 0 means no limit
--window-messages M   search only the newest M messages
--window-hours H      search only the last H hours
--session ID          restrict to one session
--include-current     include the current pi session
--type TYPE           restrict by message type
--any                 match any term instead of every term
--raw                 pass an FTS5 boolean expression through
```

A current pi session is excluded by default when pi exposes `PI_SESSION_ID` or
`PI_SESSION_FILE`. An explicit `--session` takes precedence.

### `read`

```text
--before N     messages before the target (default 5)
--after N      messages after the target (default 5)
```

The target is always included, and neighbors never cross a session boundary.
By default only prose is rendered; `--all` includes tool-only records.

### `sessions`

```text
QUERY          optional prose query
--limit N      maximum rows, default 20
--cwd PATH     exact working-directory filter
--any          match any query term
```

Rows include session ID, working directory, visibility, parent session, last
message preview, last timestamp, and message count.

### `activities` and `activity`

```text
activities:
  --session ID     parent or child session filter
  --status STATUS   running, completed, failed, ...
  --limit N         maximum rows, default 20
  --full            include diagnostic linkage fields

activity ID:
  --full            include diagnostic linkage fields
```

Activity rows are pi subagent lifecycle records. The index links parent
activity markers with child session transcripts when the marker information is
available.

### `context`

```text
--hits N             search hits to expand, default 3
--before N           messages before each hit, default 3
--after N            messages after each hit, default 8
--session ID         restrict search to one session
--type TYPE          restrict by message type
--include-current    include current pi session
--any                match any term
--raw                use an FTS5 boolean expression
```

The response contains the selected hits and a deduplicated chronological
expansion. Expanded rows include the IDs of the hits that caused them to be
selected.

### `commands`

```text
--tool NAME          restrict to one tool, for example Bash
--session ID         restrict to one session
--limit N            maximum rows, default 20; `0` means unlimited
--full               include paired tool output and full details
--include-output     include paired tool output
--output             alias for `--include-output`
```

This command searches full message content because command text commonly lives
in tool arguments or tool results. It returns a normalized tool name,
structured arguments, source message/session IDs, timestamp, and optional
paired output.

### `serve`

```text
--host HOST       localhost or a loopback address only; default 127.0.0.1
--port N          TCP port; default 8765, `0` chooses an available port
--index PATH      derived index path
--transcripts DIR transcript root override
```

The API has no authentication and must remain loopback-only.

## Output

Data commands emit one JSON object on stdout. Warnings and errors go to stderr.
`help`, `serve`, and `rebuild` also print human-readable status text where
appropriate. A typical compact search response is:

```json
{
  "results": [
    {
      "id": "message-id",
      "sessionId": "session-id",
      "timestamp": "2026-08-25T00:00:01Z",
      "type": "assistant",
      "preview": "A compact one-line preview…",
      "charCount": 342
    }
  ],
  "total": 1,
  "truncated": false,
  "relaxed": false,
  "budget": {"limit": 60000, "spent": 42, "dropped": 0, "shrunk": false}
}
```

`--full` replaces `preview` with `content`; compact responses never emit both.
`charCount` remains the full content length. `truncated` is true when `--limit`
or the output budget prevents all selected results from being emitted.

The budget is applied to rendered output:

- preview mode shortens previews to fit more results, down to a 40-character
  floor, before dropping results
- full mode emits complete bodies in rank order until the budget is exhausted
- `budget.dropped` counts dropped messages and `budget.shrunk` reports shortened
  previews

## Query semantics

- Default queries are literal and punctuation-safe. Terms are whitespace
  separated, Porter-stemmed, prefix-matched, and ANDed.
- If a multi-term AND query returns no results, search retries with any term and
  sets `relaxed: true`. This is a related-results fallback, not proof that all
  terms matched.
- `--any` requests any-term matching without setting `relaxed`.
- `--raw` passes the expression to FTS5, enabling `OR`, `NOT`, `NEAR`, grouping,
  phrases, and column filters. Quote punctuation such as `display.cpp` and
  `192.168.1.112`. Raw queries are never relaxed, and `--raw --any` is rejected.
- `--all` switches from `prose` to `content`; it is the right choice for a path,
  command, error, file dump, or tool-only message.

## Sources and index maintenance

Default transcript roots are:

```text
~/.claude/projects
~/.pi/agent/sessions
```

Both are walked recursively. `--transcripts DIR` replaces both with one root.
The index defaults to `~/.claude/search-index.db`; it is fully derived and safe
to delete.

Every operation compares transcript mtime and size with the `files` table and
re-reads only changed files. A changed transcript is deleted and reinserted,
so incremental sync does not duplicate messages. `rebuild` is useful when you
want to force this process or rebuild one session:

```bash
cc-search rebuild
cc-search rebuild --session SESSION_ID
```

`cc-search info` reports indexed source and count information. `cc-search
doctor` performs the SQLite integrity and schema checks. Lifecycle operations
use a path-specific advisory lock, so concurrent CLI processes are safe on Unix
systems. The unsupported-platform fallback does not claim a lifecycle lock.

## Troubleshooting

| Symptom | Action |
|---|---|
| `no such module: fts5` | Rebuild with `make build`; the `sqlite_fts5` tag is mandatory. |
| No matches for a command or error | Add `--all`; it may only be in tool output. |
| `relaxed: true` | The exact AND query had no match; narrow or improve the query. |
| Missing one agent's sessions | Run `cc-search info`; check both default roots or pass `--transcripts`. |
| Stale or unhealthy index | Run `cc-search doctor`, then `cc-search rebuild`. |
| Concurrent access failure | Wait for the other process; the lock timeout names the index. |

## Build

```bash
make test
make vet
make build
make deploy
```

The `sqlite_fts5` build tag is mandatory for every Go build and test. The
OpenAPI document at `openapi/cc-search.json` remains available from the
loopback service at `/openapi.json` for HTTP clients that need it.
