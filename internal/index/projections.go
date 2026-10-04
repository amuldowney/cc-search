package index

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/amuldowney/cc-search/internal/transcript"
)

const projectionSchema = `
-- Read models are maintained transactionally with their source messages. The
-- 514-rune prefix keeps a non-whitespace rune after the supported 512-rune
-- preview boundary, so whitespace normalization preserves ellipsis behavior.
CREATE TABLE IF NOT EXISTS message_projections (
  messageId        TEXT PRIMARY KEY,
  contentPrefix    TEXT NOT NULL,
  prosePrefix      TEXT NOT NULL,
  contentCharCount INTEGER NOT NULL,
  proseCharCount   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS source_projections (
  sourcePath          TEXT PRIMARY KEY,
  sessionId           TEXT NOT NULL,
  messageCount        INTEGER NOT NULL,
  latestTimestamp     INTEGER NOT NULL,
  latestMessageId     TEXT NOT NULL,
  latestContentPrefix TEXT NOT NULL,
  latestProsePrefix   TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS tool_invocations (
  invocationId INTEGER PRIMARY KEY,
  messageId    TEXT NOT NULL,
  sessionId    TEXT NOT NULL,
  toolId       TEXT NOT NULL,
  toolName     TEXT NOT NULL,
  arguments    TEXT NOT NULL,
  timestamp    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tool_results (
  resultId     INTEGER PRIMARY KEY,
  messageId    TEXT NOT NULL,
  sessionId    TEXT NOT NULL,
  toolId       TEXT NOT NULL,
  toolName     TEXT NOT NULL,
  content      TEXT NOT NULL,
  isError      INTEGER NOT NULL,
  timestamp    INTEGER NOT NULL,
  invocationId INTEGER
);
CREATE INDEX IF NOT EXISTS idx_timestamp ON messages(timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_type_time ON messages(type, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_source_time ON messages(sourcePath, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_tool_invocations_session_tool ON tool_invocations(sessionId, toolId);
CREATE INDEX IF NOT EXISTS idx_tool_invocations_message ON tool_invocations(messageId);
CREATE INDEX IF NOT EXISTS idx_tool_invocations_session_time ON tool_invocations(sessionId, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_tool_results_invocation ON tool_results(invocationId);
CREATE INDEX IF NOT EXISTS idx_tool_results_session_tool ON tool_results(sessionId, toolId);
CREATE INDEX IF NOT EXISTS idx_tool_results_message ON tool_results(messageId);
`

const projectionPrefixRunes = 514
const maxCompactPreviewLength = 512

func cachedPrefix(body string) string {
	flat := strings.Join(strings.Fields(body), " ")
	runes := []rune(flat)
	if len(runes) > projectionPrefixRunes {
		runes = runes[:projectionPrefixRunes]
	}
	return string(runes)
}

func insertMessageProjection(tx *sql.Tx, msg transcript.Message) error {
	_, err := tx.Exec(`INSERT INTO message_projections
		(messageId, contentPrefix, prosePrefix, contentCharCount, proseCharCount)
		VALUES (?, ?, ?, ?, ?)`, msg.ID, cachedPrefix(msg.Content), cachedPrefix(msg.Prose), len(msg.Content), len(msg.Prose))
	return err
}

func insertToolRecords(tx *sql.Tx, msg transcript.Message) error {
	for _, call := range msg.ToolCalls {
		if _, err := tx.Exec(`INSERT INTO tool_invocations
			(messageId, sessionId, toolId, toolName, arguments, timestamp)
			VALUES (?, ?, ?, ?, ?, ?)`, msg.ID, msg.SessionID, call.ID, call.Name, call.Arguments, msg.Timestamp); err != nil {
			return err
		}
	}
	for _, result := range msg.ToolResults {
		isError := 0
		if result.IsError {
			isError = 1
		}
		if _, err := tx.Exec(`INSERT INTO tool_results
			(messageId, sessionId, toolId, toolName, content, isError, timestamp)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, msg.ID, msg.SessionID, result.ID, result.Name, result.Content, isError, msg.Timestamp); err != nil {
			return err
		}
	}
	return nil
}

func refreshSourceProjection(tx *sql.Tx, sourcePath string) error {
	var sessionID string
	if err := tx.QueryRow(`SELECT sessionId FROM sessions WHERE sourcePath = ?`, sourcePath).Scan(&sessionID); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(`SELECT count(*) FROM messages WHERE sourcePath = ?`, sourcePath).Scan(&count); err != nil {
		return err
	}
	var timestamp int64
	var messageID, contentPrefix, prosePrefix string
	err := tx.QueryRow(`SELECT m.timestamp, m.id, p.contentPrefix, p.prosePrefix
		FROM messages m JOIN message_projections p ON p.messageId = m.id
		WHERE m.sourcePath = ? ORDER BY m.timestamp DESC, m.rowid DESC LIMIT 1`, sourcePath).
		Scan(&timestamp, &messageID, &contentPrefix, &prosePrefix)
	if errors.Is(err, sql.ErrNoRows) {
		timestamp, messageID, contentPrefix, prosePrefix = 0, "", "", ""
	} else if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO source_projections
		(sourcePath, sessionId, messageCount, latestTimestamp, latestMessageId, latestContentPrefix, latestProsePrefix)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(sourcePath) DO UPDATE SET sessionId=excluded.sessionId,
		messageCount=excluded.messageCount, latestTimestamp=excluded.latestTimestamp,
		latestMessageId=excluded.latestMessageId, latestContentPrefix=excluded.latestContentPrefix,
		latestProsePrefix=excluded.latestProsePrefix`, sourcePath, sessionID, count, timestamp, messageID, contentPrefix, prosePrefix)
	return err
}

type indexedToolCall struct {
	id                      int64
	messageID, toolID, name string
	timestamp, rowID        int64
}

type indexedToolResult struct {
	id                      int64
	messageID, toolID, name string
	timestamp, rowID        int64
}

// rebuildToolLinks pairs durable IDs within a session and uses a bounded
// nearest-prior same-session fallback for legacy calls/results without IDs.
func rebuildToolLinks(tx *sql.Tx, sessionID string) error {
	if _, err := tx.Exec(`UPDATE tool_results SET invocationId = NULL WHERE sessionId = ?`, sessionID); err != nil {
		return err
	}
	calls := []indexedToolCall{}
	rows, err := tx.Query(`SELECT i.invocationId, i.messageId, i.toolId, i.toolName, i.timestamp, m.rowid
		FROM tool_invocations i JOIN messages m ON m.id = i.messageId
		WHERE i.sessionId = ? ORDER BY i.timestamp, m.rowid, i.invocationId`, sessionID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var call indexedToolCall
		if err := rows.Scan(&call.id, &call.messageID, &call.toolID, &call.name, &call.timestamp, &call.rowID); err != nil {
			rows.Close()
			return err
		}
		calls = append(calls, call)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	results := []indexedToolResult{}
	rows, err = tx.Query(`SELECT r.resultId, r.messageId, r.toolId, r.toolName, r.timestamp, m.rowid
		FROM tool_results r JOIN messages m ON m.id = r.messageId
		WHERE r.sessionId = ? ORDER BY r.timestamp, m.rowid, r.resultId`, sessionID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var result indexedToolResult
		if err := rows.Scan(&result.id, &result.messageID, &result.toolID, &result.name, &result.timestamp, &result.rowID); err != nil {
			rows.Close()
			return err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	fallbackUsed := make(map[int64]bool)
	for _, result := range results {
		var match *indexedToolCall
		if result.toolID != "" {
			for i := len(calls) - 1; i >= 0; i-- {
				call := &calls[i]
				if call.timestamp < result.timestamp || call.timestamp == result.timestamp && call.rowID <= result.rowID {
					if call.toolID == result.toolID {
						match = call
						break
					}
				}
			}
		} else {
			lookedAt := 0
			for i := len(calls) - 1; i >= 0 && lookedAt < 64; i-- {
				call := &calls[i]
				if call.timestamp > result.timestamp || call.timestamp == result.timestamp && call.rowID > result.rowID {
					continue
				}
				if call.toolID != "" {
					continue
				}
				lookedAt++
				if fallbackUsed[call.id] || result.name != "" && call.name != "" && !strings.EqualFold(result.name, call.name) {
					continue
				}
				match = call
				fallbackUsed[call.id] = true
				break
			}
		}
		if match != nil {
			if _, err := tx.Exec(`UPDATE tool_results SET invocationId = ? WHERE resultId = ?`, match.id, result.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// migrateReadProjections adds the version 8 read model transactionally to an
// existing v7 index. It derives projections and tool relationships solely from
// indexed rows, leaving the external-content FTS table and message rowids as-is.
func migrateReadProjections(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		return nil
	}
	if version != 7 {
		return fmt.Errorf("read projection migration requires schema version 7, got %d", version)
	}
	if _, err := tx.Exec(projectionSchema); err != nil {
		return fmt.Errorf("create read projection schema: %w", err)
	}

	rows, err := tx.Query(`SELECT id, sourcePath, COALESCE(sessionId, ''), timestamp,
		COALESCE(content, ''), COALESCE(prose, ''), COALESCE(charCount, 0),
		COALESCE(toolCalls, ''), COALESCE(toolResults, '') FROM messages ORDER BY rowid`)
	if err != nil {
		return err
	}
	sources := map[string]struct{}{}
	sourceRows, err := tx.Query(`SELECT sourcePath FROM sessions`)
	if err != nil {
		return err
	}
	for sourceRows.Next() {
		var source string
		if err := sourceRows.Scan(&source); err != nil {
			sourceRows.Close()
			return err
		}
		sources[source] = struct{}{}
	}
	if err := sourceRows.Err(); err != nil {
		sourceRows.Close()
		return err
	}
	if err := sourceRows.Close(); err != nil {
		return err
	}
	sessions := map[string]struct{}{}
	for rows.Next() {
		var msg transcript.Message
		var sourcePath, callsJSON, resultsJSON string
		if err := rows.Scan(&msg.ID, &sourcePath, &msg.SessionID, &msg.Timestamp,
			&msg.Content, &msg.Prose, &msg.CharCount, &callsJSON, &resultsJSON); err != nil {
			rows.Close()
			return err
		}
		msg.ProseCharCount = len(msg.Prose)
		if err := json.Unmarshal([]byte(callsJSON), &msg.ToolCalls); err != nil {
			msg.ToolCalls = nil
		}
		if err := json.Unmarshal([]byte(resultsJSON), &msg.ToolResults); err != nil {
			msg.ToolResults = nil
		}
		if err := insertMessageProjection(tx, msg); err != nil {
			rows.Close()
			return err
		}
		if err := insertToolRecords(tx, msg); err != nil {
			rows.Close()
			return err
		}
		sources[sourcePath] = struct{}{}
		sessions[msg.SessionID] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, source := range readDistinct(sources) {
		if err := refreshSourceProjection(tx, source); err != nil {
			return err
		}
	}
	for _, session := range readDistinct(sessions) {
		if session != "" {
			if err := rebuildToolLinks(tx, session); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`PRAGMA user_version = 8`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit read projection migration: %w", err)
	}
	return nil
}

func readDistinct(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for item := range set {
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}
