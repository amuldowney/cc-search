# cc-search: Remove recap support and duplicate output

**Date:** 2026-08-14
**Status:** Approved

## Goal

Simplify cc-search's public and internal model:

- Recap messages remain ordinary searchable messages, but the special recap
  concept is removed.
- A response never includes both a compact preview and full content.

## Design

### Recap removal

Remove recap-specific behavior from every layer:

- CLI flags `--prefer-recaps` and `--recaps-only`.
- JSON fields `isRecap` and `recapCount`.
- Transcript parsing and `Message` state used only for recap detection.
- SQLite `isRecap` storage, filtering, and ordering.
- Related tests, README examples, and current behavior documentation.

The derived index schema version is bumped. Existing indexes are discarded and
rebuilt automatically rather than migrated.

Messages containing the word "recap" are not excluded; they are searched and
returned like any other message.

### Mutually exclusive output

Normal output emits `preview` only. `--full` emits `content` only.

- Compact previews remain single-line and length-limited.
- Full content is emitted without transformation, including newlines.
- `--preview-length` continues to control compact output.
- Character budgets continue to measure the body that is actually emitted.

The remaining metadata (`id`, `sessionId`, `timestamp`, `type`, and
`charCount`) is unchanged. `truncated`, `relaxed`, and budget accounting remain
unchanged.

## Data flow

Transcript parsing produces ordinary messages with content/prose. The index
stores and queries those fields without recap metadata. Output formatting picks
exactly one body representation based on `Full`, and JSON `omitempty` prevents
the unused body field from appearing.

## Error handling

No new errors are introduced. Existing usage errors for removed flags are
retained through the standard flag parser. An old index is treated as an
incompatible derived index and rebuilt automatically.

## Verification

- Add/update output tests proving compact responses omit `content` and full
  responses omit `preview`.
- Add/update parser, index, and CLI tests proving recap-like text has no special
  fields, flags, filtering, or ordering behavior.
- Update documentation and run the complete Go test suite plus build.
