# cc-search

A fast local CLI for searching Claude Code conversation transcripts. Built for
agents to pad context, refresh session memory, and dig up concepts from earlier
conversations without re-reading whole transcripts.

Design: [docs/2026-07-22-transcript-search-design.md](docs/2026-07-22-transcript-search-design.md).

## Build

FTS5 is not compiled into `go-sqlite3` by default, so the `sqlite_fts5` build
tag is required — use the Makefile and it is never forgotten.

```bash
make test
make build            # ./cc-search
make install          # $HOME/.local/bin/cc-search
```

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
cc-search search "firmware rollout" --prefer-recaps
cc-search search "why did the proxy break" --prose   # ignore tool args/output
cc-search search "caddy proxy" --recaps-only --full

# index maintenance (rarely needed — sync is automatic)
cc-search rebuild
cc-search rebuild --session 62de7038-83f9-4ca9-8b2b-165ab6c70d47
```

The search pattern is positional and must come before the flags — putting a
flag there is rejected rather than searched for.

Output is one line of JSON on stdout; warnings and errors go to stderr, so
`cc-search ... | jq` is always safe. Exit 0 (including for zero results), 1 on
error, 2 on a usage mistake.

```json
{"results":[{"id":"...","sessionId":"...","timestamp":"2026-07-23T04:01:04Z",
"type":"assistant","isRecap":true,"preview":"Done. To recap: …","charCount":342}],
"total":1,"recapCount":1,"truncated":false}
```

Previews are collapsed to a single line and capped at 100 characters
(`--preview-length N`); `--full` adds the complete `content` field.
`truncated` is true when `--limit` cut results off.

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
actually said). `--prose` searches the second — without it, a query like
"caddy proxy" is dominated by `[tool: Edit] {...}` messages whose arguments
happen to contain the words. Use `--prose` for "what did we decide", and plain
search when hunting a command, path, or piece of output.

**Recaps.** An assistant message whose *prose* mentions "recap" is flagged
`isRecap`. Tool-call arguments are deliberately excluded — otherwise running
`cc-search --recaps-only` would flag its own command line as a recap.

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
transcripts and can be deleted at any time; a corrupt index is detected on open
and rebuilt automatically.
