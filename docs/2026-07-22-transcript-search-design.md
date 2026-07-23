# Claude Code Transcript Search CLI — Design

**Date:** 2026-07-22  
**Status:** Approved  

## Overview

A fast, local CLI tool for searching and retrieving messages from Claude Code conversation transcripts. Optimized for agents to pad context windows, refresh session memory, and research lost concepts across previous conversations.

## Problem

Agents and users need quick access to previous conversation context without re-reading full transcripts or making API calls. Current workflows require manual transcript navigation. Need: deterministic, fast queries with compact output sized for agent context windows.

## Scope

- Query local JSONL transcripts in `~/.claude/projects/-home-andrew-Projects/`
- Build one-time SQLite index with FTS5 for fast search
- Two primary use cases: memory refresh (last N messages) and concept research (fuzzy search)
- Special handling for "recap" messages — multi-turn summaries that are particularly valuable
- Both interactive CLI and agent-subprocess modes

Out of scope: token counting (use message/time windows only), API integration, cloud storage, analytics.

## Data Structure

**SQLite Schema:**

```sql
CREATE TABLE messages (
  id TEXT PRIMARY KEY,           -- unique message ID from JSONL
  sessionId TEXT,                -- session UUID from ~/.claude/projects/-home-andrew-Projects/
  timestamp INTEGER,             -- unix milliseconds
  type TEXT,                     -- user|assistant|system|attachment|...
  content TEXT,                  -- full message content
  charCount INTEGER              -- content length for sizing
);

CREATE VIRTUAL TABLE messages_fts USING fts5(
  content,
  content=messages,
  content_rowid=rowid
);

CREATE INDEX idx_session_time ON messages(sessionId, timestamp DESC);
```

**Index Location:** `~/.claude/search-index.db`

**Recap Detection:** Assistant message where content contains "recap" (case-insensitive). Flagged in results with `"isRecap": true`.

## Query Modes

### 1. Memory Refresh

Retrieve recent messages from a session to refresh context or pad a prompt.

**CLI:**
```bash
cc-search last 10 [--session SESSION_ID]       # last 10 messages
cc-search last --hours 2 [--session SESSION_ID] # last 2 hours
```

**Output:** Compact JSON with timestamp, type, preview, charCount.

### 2. Concept Research

Fuzzy search for a topic across all or filtered transcripts.

**CLI:**
```bash
cc-search search "pattern" \
  [--limit N]              # max N results
  [--window-messages M]    # stop after scanning M messages
  [--window-hours H]       # stop after H hours of history
  [--session SESSION_ID]   # filter to specific session
  [--type user|assistant]  # filter by message type
  [--prefer-recaps]        # boost recap messages to top
  [--recaps-only]          # return only recap messages
```

**Output:** Ranked results with context, recap flag, charCount.

## Output Format

**Default (agent-friendly JSON):**

```json
{
  "results": [
    {
      "id": "msg_01ABC123XYZ",
      "sessionId": "fd97266a-4254-40ee-a0e5-f2c3a0a073af",
      "timestamp": "2026-07-22T21:00:00Z",
      "type": "assistant",
      "isRecap": true,
      "preview": "Done. To recap: the index rebuild completed successfully...",
      "charCount": 342
    },
    {
      "id": "msg_02DEF456UVW",
      "sessionId": "fd97266a-4254-40ee-a0e5-f2c3a0a073af",
      "timestamp": "2026-07-22T20:55:30Z",
      "type": "user",
      "isRecap": false,
      "preview": "rebuild the index and commit the changes",
      "charCount": 58
    }
  ],
  "total": 2,
  "recapCount": 1,
  "truncated": false
}
```

**Preview Length:** Default 100 chars; configurable via `--preview-length N`.

**Full Content:** Pass `--full` to include complete `content` field (for agents that need it).

## Index Management

**Build:**
- On first run, scan all JSONL files in `~/.claude/projects/-home-andrew-Projects/` and build index.
- Identify sessions by filename (UUID format).
- Parse each JSONL line as a JSON object; extract fields: `uuid` (messageId), `timestamp`, `type`, `message.content[0].text` (or aggregate multi-part messages).

**Incremental Updates:**
- On each run, check modification times of JSONL files.
- If a file is newer than the index, re-scan and update affected messages.
- Rebuilding a single session is ~100ms; full rebuild from scratch ~1-2s.

**Manual Rebuild:**
```bash
cc-search rebuild              # full rebuild
cc-search rebuild --session ID # rebuild single session
```

## Implementation Stack

- **Language:** Go (compiled binary, ~10MB, no runtime required)
- **Database:** sqlite3 with Go bindings (`github.com/mattn/go-sqlite3`)
- **FTS:** SQLite FTS5 (built-in, no external dependency)
- **CLI:** Cobra or simple `flag` package

## Sizing & Performance

**Index Size:** ~50 bytes per message + content. For 10k messages (~5MB content), expect ~1MB index.

**Query Performance:**
- Last N messages: <10ms (indexed range query)
- Fuzzy search: 10–100ms depending on result set and window size
- Index rebuild: 1–2s (full) or 10–50ms (incremental per session)

**Output Size (Compact Mode):** ~500 bytes per result (JSON overhead + preview). 10 results ≈ 5KB.

## Error Handling

- Missing JSONL files: skip silently, log warning
- Corrupt JSON line: skip, log warning, continue
- Index corruption: detect on open, auto-rebuild
- Empty results: return empty array, `total: 0`, `truncated: false`

## Testing

**Unit Tests:**
- Index build correctness (known inputs → expected schema)
- Search accuracy (known query → expected ranked results)
- Recap detection (various "recap" spellings and contexts)
- Time window filtering
- Message type filtering

**Integration Tests:**
- Subprocess calls (agent → CLI → JSON parse)
- Real transcript parsing
- Incremental index updates

## Future Considerations (out of scope)

- Token counting via external API (deferred — use message/time windows for now)
- Multi-project search (currently single `~/.claude/projects/-home-andrew-Projects/`)
- Persistence of query history or bookmarks
- Streaming large result sets
