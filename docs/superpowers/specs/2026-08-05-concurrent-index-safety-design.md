# Concurrent index safety

**Date:** 2026-08-05  
**Status:** Approved

## Problem

`cc-search` synchronizes its derived SQLite index before every query. Eleven Pi subagents starting together therefore opened and synchronized the same index concurrently. Calls failed with `database is locked`, `no such table: messages`, and `create schema: disk I/O error`.

The destructive failure is in `index.Open`: every open error is treated as proof that the index is damaged or outdated, so the shared database path is removed while other processes may still hold open handles. A transient `SQLITE_BUSY`/`SQLITE_LOCKED` can consequently turn ordinary contention into index replacement and inconsistent handles.

## Requirements

1. Concurrent CLI processes using one index must complete successfully and return valid JSON.
2. Opening and incremental synchronization of a shared index must be serialized across processes.
3. Transient lock/busy errors must never trigger index deletion.
4. Proven corruption and incompatible schema versions must retain automatic discard-and-rebuild behavior.
5. Lock waiting must be bounded and return an actionable error rather than hang forever.
6. Existing command output and single-process behavior must remain unchanged.

## Design

### Index lifecycle lock

Acquire an advisory lock derived from the index path before opening or synchronizing the database. Hold it through schema validation and all configured transcript synchronization, then release it before executing the read query. The `rebuild` command uses the same lock.

The lock is cross-process, path-specific, bounded by a timeout, and represented by a separate lock file so replacing the derived database does not invalidate the lock identity.

### Safe recovery classification

`index.Open` may discard and recreate an index only when the error establishes that the database is corrupt or has an incompatible schema. SQLite busy/locked errors and environmental failures—including permissions and I/O errors—are returned without unlinking the database.

### SQLite behavior

Keep SQLite's busy timeout as defense in depth. WAL may be enabled if tests demonstrate it is needed for safe overlap between a post-sync reader and the next process's sync, but WAL alone is not the primary synchronization mechanism and should not broaden scope unnecessarily.

## Error handling

- Lock timeout: fail with the index path and wait duration.
- Busy/locked open: return the underlying error; preserve the index.
- Proven corruption or schema mismatch: close handles, remove database plus SQLite sidecars as appropriate, then recreate.
- Lock release failures: report them without hiding a prior operation error.

## Tests

Follow strict red-green TDD.

1. A process-level regression test launches at least eleven concurrent CLI searches against one fresh index and transcript root, then asserts every process exits zero with valid JSON.
2. An index-layer test forces a transient lock/busy open error and asserts the original database is not removed or replaced.
3. Existing corruption and old-schema recovery tests continue to prove legitimate rebuilds.
4. Run `make test`, `make vet`, and a focused race reproduction using the built binary.

## Scope exclusions

- No daemon or persistent indexing service.
- No per-process permanent indexes or merge layer.
- No changes to transcript parsing, search semantics, or JSON output.
- No silent infinite retries.
