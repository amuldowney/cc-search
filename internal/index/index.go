// Package index stores transcript messages in a SQLite database with an FTS5
// index and answers memory-refresh and concept-research queries against it.
package index

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/andrewmuldowney/cc-search/internal/transcript"
)

const schema = `
CREATE TABLE IF NOT EXISTS messages (
  id        TEXT PRIMARY KEY,
  sessionId TEXT,
  timestamp INTEGER,
  type      TEXT,
  content   TEXT,
  charCount INTEGER,
  isRecap   INTEGER NOT NULL DEFAULT 0
);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
  content,
  content=messages,
  content_rowid=rowid
);

CREATE INDEX IF NOT EXISTS idx_session_time ON messages(sessionId, timestamp DESC);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;

CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
  INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;

-- Tracks the mtime/size of each indexed transcript so a rescan only touches
-- files that actually changed.
CREATE TABLE IF NOT EXISTS files (
  path  TEXT PRIMARY KEY,
  mtime INTEGER,
  size  INTEGER
);
`

// DB is an open index database.
type DB struct {
	sql *sql.DB
}

// SyncStats reports what a Sync changed.
type SyncStats struct {
	SessionsIndexed int
	MessagesIndexed int
	FilesSkipped    int
}

// LastOptions selects the most recent messages.
type LastOptions struct {
	N         int
	Hours     int
	SessionID string
}

// SearchOptions selects messages matching a full-text query.
type SearchOptions struct {
	Query          string
	Limit          int
	WindowMessages int
	WindowHours    int
	SessionID      string
	Type           string
	PreferRecaps   bool
	RecapsOnly     bool
}

// Open opens (creating if needed) the index at path.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := open(path)
	if err == nil {
		return db, nil
	}

	// A damaged index is disposable — everything in it is derived from the
	// transcripts, so throw it away and start clean rather than failing.
	if rmErr := os.Remove(path); rmErr != nil {
		return nil, err
	}
	return open(path)
}

func open(path string) (*DB, error) {
	handle, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	if _, err := handle.Exec(schema); err != nil {
		handle.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if _, err := handle.Exec(`PRAGMA quick_check`); err != nil {
		handle.Close()
		return nil, fmt.Errorf("integrity check: %w", err)
	}
	return &DB{sql: handle}, nil
}

// Close releases the database handle.
func (d *DB) Close() error { return d.sql.Close() }

// Sync indexes any transcript file in dir that changed since the last sync.
func (d *DB) Sync(dir string) (SyncStats, error) {
	return d.sync(dir, "", false)
}

// Rebuild discards the index and rebuilds it from dir. When sessionID is
// non-empty only that session is rebuilt.
func (d *DB) Rebuild(dir, sessionID string) (SyncStats, error) {
	return d.sync(dir, sessionID, true)
}

func (d *DB) sync(dir, sessionID string, force bool) (SyncStats, error) {
	var stats SyncStats

	entries, err := os.ReadDir(dir)
	if err != nil {
		return stats, fmt.Errorf("read transcript dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		session := strings.TrimSuffix(entry.Name(), ".jsonl")
		if sessionID != "" && session != sessionID {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			// The file vanished between listing and stat; nothing to index.
			continue
		}
		if !force && !d.changed(path, info) {
			stats.FilesSkipped++
			continue
		}
		n, err := d.indexFile(path, session, info)
		if err != nil {
			return stats, err
		}
		stats.SessionsIndexed++
		stats.MessagesIndexed += n
	}
	return stats, nil
}

// changed reports whether path differs from what the files table recorded.
func (d *DB) changed(path string, info os.FileInfo) bool {
	var mtime, size int64
	err := d.sql.QueryRow(`SELECT mtime, size FROM files WHERE path = ?`, path).Scan(&mtime, &size)
	if err != nil {
		return true
	}
	return mtime != info.ModTime().UnixMilli() || size != info.Size()
}

// indexFile replaces every indexed message for one session with the current
// contents of its transcript.
func (d *DB) indexFile(path, session string, info os.FileInfo) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM messages WHERE sessionId = ?`, session); err != nil {
		return 0, err
	}

	insert, err := tx.Prepare(`
		INSERT INTO messages (id, sessionId, timestamp, type, content, charCount, isRecap)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			sessionId = excluded.sessionId,
			timestamp = excluded.timestamp,
			type      = excluded.type,
			content   = excluded.content,
			charCount = excluded.charCount,
			isRecap   = excluded.isRecap`)
	if err != nil {
		return 0, err
	}
	defer insert.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	count := 0
	for scanner.Scan() {
		msg, ok := transcript.ParseLine(scanner.Bytes())
		if !ok {
			continue
		}
		if msg.SessionID == "" {
			msg.SessionID = session
		}
		if _, err := insert.Exec(msg.ID, msg.SessionID, msg.Timestamp, msg.Type,
			msg.Content, msg.CharCount, msg.IsRecap); err != nil {
			return 0, err
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan %s: %w", path, err)
	}

	if _, err := tx.Exec(`
		INSERT INTO files (path, mtime, size) VALUES (?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET mtime = excluded.mtime, size = excluded.size`,
		path, info.ModTime().UnixMilli(), info.Size()); err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

const selectColumns = `id, sessionId, timestamp, type, content, charCount, isRecap`

// Last returns the most recent messages, newest first.
func (d *DB) Last(opts LastOptions) ([]transcript.Message, error) {
	query := `SELECT ` + selectColumns + ` FROM messages WHERE 1 = 1`
	var args []any

	if opts.SessionID != "" {
		query += ` AND sessionId = ?`
		args = append(args, opts.SessionID)
	}
	if opts.Hours > 0 {
		query += ` AND timestamp >= ?`
		args = append(args, since(opts.Hours))
	}
	query += ` ORDER BY timestamp DESC`
	if opts.N > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.N)
	}

	return d.collect(query, args...)
}

// Search returns messages matching opts.Query, most relevant first.
func (d *DB) Search(opts SearchOptions) ([]transcript.Message, error) {
	match := ftsQuery(opts.Query)
	if match == "" {
		return nil, nil
	}

	// The window is the slice of recent history to search within: the newest
	// WindowMessages messages and/or everything inside WindowHours.
	window := `SELECT rowid FROM messages WHERE 1 = 1`
	var args []any
	if opts.SessionID != "" {
		window += ` AND sessionId = ?`
		args = append(args, opts.SessionID)
	}
	if opts.WindowHours > 0 {
		window += ` AND timestamp >= ?`
		args = append(args, since(opts.WindowHours))
	}
	window += ` ORDER BY timestamp DESC`
	if opts.WindowMessages > 0 {
		window += ` LIMIT ?`
		args = append(args, opts.WindowMessages)
	}

	query := `
		SELECT ` + prefixed(selectColumns, "m") + `
		FROM messages_fts f
		JOIN messages m ON m.rowid = f.rowid
		WHERE f.messages_fts MATCH ?
		  AND m.rowid IN (` + window + `)`
	args = append([]any{match}, args...)

	if opts.Type != "" {
		query += ` AND m.type = ?`
		args = append(args, opts.Type)
	}
	if opts.RecapsOnly {
		query += ` AND m.isRecap = 1`
	}

	query += ` ORDER BY `
	if opts.PreferRecaps {
		query += `m.isRecap DESC, `
	}
	query += `bm25(f.messages_fts), m.timestamp DESC`

	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	return d.collect(query, args...)
}

func (d *DB) collect(query string, args ...any) ([]transcript.Message, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := []transcript.Message{}
	for rows.Next() {
		var m transcript.Message
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Timestamp, &m.Type,
			&m.Content, &m.CharCount, &m.IsRecap); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func since(hours int) int64 {
	return time.Now().Add(-time.Duration(hours) * time.Hour).UnixMilli()
}

func prefixed(columns, alias string) string {
	parts := strings.Split(columns, ", ")
	for i, p := range parts {
		parts[i] = alias + "." + p
	}
	return strings.Join(parts, ", ")
}

// ftsQuery turns a user pattern into an FTS5 MATCH expression. Each whitespace
// separated term becomes a quoted prefix term, so punctuation-heavy patterns
// like "display.cpp" are matched literally instead of parsed as FTS syntax.
func ftsQuery(pattern string) string {
	var terms []string
	for _, field := range strings.Fields(pattern) {
		escaped := strings.ReplaceAll(field, `"`, `""`)
		terms = append(terms, `"`+escaped+`"*`)
	}
	return strings.Join(terms, " ")
}
