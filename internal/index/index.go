// Package index stores transcript messages in a SQLite database with an FTS5
// index and answers memory-refresh and concept-research queries against it.
package index

import (
	"bufio"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"

	"github.com/andrewmuldowney/cc-search/internal/transcript"
)

// schemaVersion is bumped whenever the tables change. An index written by a
// different version is discarded rather than migrated — it is all derived data.
const schemaVersion = 6

var errSchemaMismatch = errors.New("incompatible index schema")

const schema = `
CREATE TABLE IF NOT EXISTS messages (
  id           TEXT PRIMARY KEY,
  entryId      TEXT NOT NULL,
  sourcePath   TEXT NOT NULL,
  sessionId    TEXT,
  timestamp    INTEGER,
  type         TEXT,
  content      TEXT,
  prose        TEXT,
  charCount    INTEGER,
  activityId   TEXT,
  activityRole TEXT
);

-- content is everything (tool calls, command output); prose is only what was
-- actually said, so a search can skip matches that live in tool arguments.
--
-- The porter stemmer folds inflected forms together, so "cache" finds
-- "caching" and "deploy" finds "deployed". Identifiers are unaffected: there
-- is nothing to stem in display.cpp or 192.168.1.112, and measurement over the
-- real corpus showed their hit counts unchanged.
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
  content,
  prose,
  content=messages,
  content_rowid=rowid,
  tokenize='porter unicode61'
);

CREATE INDEX IF NOT EXISTS idx_session_time ON messages(sessionId, timestamp DESC);
CREATE INDEX IF NOT EXISTS idx_source_path ON messages(sourcePath);

-- Session headers are metadata, not searchable messages. Attached sessions are
-- retained here so child activity transcripts can be linked back to parents.
CREATE TABLE IF NOT EXISTS sessions (
  sourcePath       TEXT PRIMARY KEY,
  sessionId        TEXT NOT NULL,
  parentSession    TEXT,
  cwd              TEXT,
  visibility       TEXT
);
CREATE INDEX IF NOT EXISTS idx_sessions_id ON sessions(sessionId);

-- Durable activity markers live in the parent session. The child transcript is
-- linked by path and resolved to its session id through the sessions table.
CREATE TABLE IF NOT EXISTS activities (
  activityId        TEXT PRIMARY KEY,
  recordSourcePath  TEXT NOT NULL,
  parentSourcePath  TEXT NOT NULL,
  parentSessionId   TEXT,
  parentActivityId  TEXT,
  childSessionPath  TEXT,
  status            TEXT,
  startedAt         INTEGER,
  completedAt       INTEGER,
  kind              TEXT,
  namespace         TEXT,
  title             TEXT,
  description       TEXT,
  model             TEXT,
  effort            TEXT,
  resultSummary     TEXT,
  toolUses          INTEGER,
  startEntryId      TEXT,
  startParentId     TEXT,
  linkedEntryId     TEXT,
  terminalEntryId   TEXT,
  terminalParentId  TEXT
);
CREATE INDEX IF NOT EXISTS idx_activities_parent_session ON activities(parentSessionId);
CREATE INDEX IF NOT EXISTS idx_activities_child_path ON activities(childSessionPath);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, content, prose) VALUES (new.rowid, new.content, new.prose);
END;

CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, content, prose)
    VALUES ('delete', old.rowid, old.content, old.prose);
END;

CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, content, prose)
    VALUES ('delete', old.rowid, old.content, old.prose);
  INSERT INTO messages_fts(rowid, content, prose) VALUES (new.rowid, new.content, new.prose);
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
	sql       *sql.DB
	lifecycle *lifecycleLock
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
	Type      string
	ProseOnly bool
}

// SearchOptions selects messages matching a full-text query.
type SearchOptions struct {
	Query          string
	Limit          int
	WindowMessages int
	WindowHours    int
	SessionID      string
	// ExcludeSessionID removes one session unless SessionID explicitly selects it.
	ExcludeSessionID string
	Type             string
	ProseOnly        bool
	// Any matches messages containing any term rather than all of them.
	Any bool
	// Raw passes Query to FTS5 untouched, so it may use OR, NOT, NEAR,
	// grouping and phrases. Punctuation must then be quoted by the caller.
	Raw bool
}

// Open opens (creating if needed) the index at path and holds the lifecycle
// lock until ReleaseLifecycleLock or Close is called. Callers should release
// it after synchronization and before running normal queries.
func Open(path string) (*DB, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	lifecycle, err := acquireLifecycleLock(path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*DB, error) {
		return nil, errors.Join(err, lifecycle.Close())
	}

	db, err := open(path)
	if err == nil {
		db.lifecycle = lifecycle
		return db, nil
	}
	if !shouldRebuild(err) {
		return fail(err)
	}

	if err := removeDatabaseFiles(path); err != nil {
		return fail(err)
	}
	db, err = open(path)
	if err != nil {
		return fail(err)
	}
	db.lifecycle = lifecycle
	return db, nil
}

func removeDatabaseFiles(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal", ""} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("discard index %q: %w", path, err)
		}
	}
	return nil
}

func shouldRebuild(err error) bool {
	if errors.Is(err, errSchemaMismatch) {
		return true
	}
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code {
	case sqlite3.ErrCorrupt, sqlite3.ErrNotADB, sqlite3.ErrFormat:
		return true
	default:
		return false
	}
}

func open(path string) (*DB, error) {
	handle, err := sql.Open("sqlite3", path+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}

	var version int
	if err := handle.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		handle.Close()
		return nil, fmt.Errorf("read schema version: %w", err)
	}
	empty, err := isEmpty(handle)
	if err != nil {
		handle.Close()
		return nil, fmt.Errorf("check schema contents: %w", err)
	}
	if version != schemaVersion && !empty {
		handle.Close()
		return nil, fmt.Errorf("%w: index has schema version %d, want %d",
			errSchemaMismatch, version, schemaVersion)
	}

	if _, err := handle.Exec(schema); err != nil {
		handle.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	if _, err := handle.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		handle.Close()
		return nil, fmt.Errorf("set schema version: %w", err)
	}
	if _, err := handle.Exec(`PRAGMA quick_check`); err != nil {
		handle.Close()
		return nil, fmt.Errorf("integrity check: %w", err)
	}
	return &DB{sql: handle}, nil
}

// isEmpty reports whether the database has no tables yet, which is how a
// freshly created file is told apart from one written by another version.
func isEmpty(handle *sql.DB) (bool, error) {
	var tables int
	if err := handle.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table'`).
		Scan(&tables); err != nil {
		return false, err
	}
	return tables == 0, nil
}

// ReleaseLifecycleLock releases the cross-process lock after synchronization.
// It is safe to call more than once.
func (d *DB) ReleaseLifecycleLock() error {
	if d == nil || d.lifecycle == nil {
		return nil
	}
	err := d.lifecycle.Close()
	d.lifecycle = nil
	return err
}

// Close releases the database handle and any lifecycle lock still held.
func (d *DB) Close() error {
	if d == nil {
		return nil
	}
	return errors.Join(d.sql.Close(), d.ReleaseLifecycleLock())
}

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

	// Walk recursively: pi keeps transcripts one level down, in per-directory
	// folders under its sessions root.
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			return nil
		}
		session := strings.TrimSuffix(entry.Name(), ".jsonl")
		if sessionID != "" && !sessionMatches(session, sessionID) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			// The file vanished between listing and stat; nothing to index.
			return nil
		}
		canonicalPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if !force && !d.changed(canonicalPath, info) {
			stats.FilesSkipped++
			return nil
		}
		n, err := d.indexFile(canonicalPath, session, info)
		if err != nil {
			return err
		}
		stats.SessionsIndexed++
		stats.MessagesIndexed += n
		return nil
	})
	if err != nil {
		return stats, fmt.Errorf("read transcript dir: %w", err)
	}
	if err := d.reconcileAttachedActivities(); err != nil {
		return stats, err
	}
	return stats, nil
}

// reconcileAttachedActivities preserves the relationship even when a child
// file is present but its parent activity markers are unavailable or old.
func (d *DB) reconcileAttachedActivities() error {
	_, err := d.sql.Exec(`
		INSERT OR IGNORE INTO activities (
			activityId, recordSourcePath, parentSourcePath, childSessionPath, status)
		SELECT child.sessionId, child.sourcePath, child.parentSession, child.sourcePath, 'running'
		FROM sessions child
		WHERE child.visibility = 'attached'
		  AND child.parentSession IS NOT NULL
		  AND child.parentSession != ''
		  AND NOT EXISTS (
			SELECT 1 FROM activities activity
			WHERE activity.childSessionPath = child.sourcePath
		  )`)
	if err != nil {
		return fmt.Errorf("reconcile attached activities: %w", err)
	}
	return nil
}

// sessionMatches reports whether a filename-derived session name is the one
// asked for. pi filenames are <timestamp>_<uuid>, so also accept a match
// against the uuid suffix.
func sessionMatches(session, sessionID string) bool {
	return session == sessionID || strings.HasSuffix(session, "_"+sessionID)
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

	// Parse before starting the transaction so Pi's header session ID is
	// available when removing the previous contents of a changed file. Pi
	// filenames include a timestamp (<timestamp>_<uuid>), while its messages
	// use the bare UUID from the session header.
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	parser := new(transcript.Parser)
	messages := []transcript.Message{}
	for scanner.Scan() {
		msg, ok := parser.ParseLine(scanner.Bytes())
		if !ok {
			continue
		}
		rawSessionID := msg.SessionID
		if msg.SessionID == "" {
			msg.SessionID = session
		}
		if rawSessionID != "" && rawSessionID != session {
			// Pi message IDs are only unique within a session. Keep the
			// transcript ID recognizable while making the index key global so
			// messages from different sessions cannot overwrite each other.
			msg.ID = rawSessionID + ":" + msg.ID
		}
		messages = append(messages, msg)
	}
	if err := scanner.Err(); err != nil {
		return 0, fmt.Errorf("scan %s: %w", path, err)
	}
	linkActivityParents(messages, parser.ActivityEvents())
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM messages WHERE sourcePath = ?`, path); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE sourcePath = ?`, path); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM activities WHERE recordSourcePath = ?`, path); err != nil {
		return 0, err
	}

	sessionMeta := parser.Session()
	if sessionMeta.ID == "" {
		sessionMeta.ID = sessionName(path)
	}
	if _, err := tx.Exec(`
		INSERT INTO sessions (sourcePath, sessionId, parentSession, cwd, visibility)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(sourcePath) DO UPDATE SET
			sessionId = excluded.sessionId,
			parentSession = excluded.parentSession,
			cwd = excluded.cwd,
			visibility = excluded.visibility`,
		path, sessionMeta.ID, sessionMeta.ParentSession, sessionMeta.CWD, sessionMeta.Visibility); err != nil {
		return 0, err
	}

	activities := aggregateActivities(path, sessionMeta.ID, parser.ActivityEvents())
	for _, activity := range activities {
		if activity.ChildSessionPath != "" {
			if _, err := tx.Exec(`DELETE FROM activities WHERE childSessionPath = ? AND activityId != ?`, activity.ChildSessionPath, activity.ID); err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO activities (
				activityId, recordSourcePath, parentSourcePath, parentSessionId, parentActivityId,
				childSessionPath, status, startedAt, completedAt, kind, namespace,
				title, description, model, effort, resultSummary, toolUses,
				startEntryId, startParentId, linkedEntryId, terminalEntryId, terminalParentId)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(activityId) DO UPDATE SET
				recordSourcePath = excluded.recordSourcePath,
				parentSourcePath = excluded.parentSourcePath,
				parentSessionId = excluded.parentSessionId,
				parentActivityId = excluded.parentActivityId,
				childSessionPath = excluded.childSessionPath,
				status = excluded.status,
				startedAt = excluded.startedAt,
				completedAt = excluded.completedAt,
				kind = excluded.kind,
				namespace = excluded.namespace,
				title = excluded.title,
				description = excluded.description,
				model = excluded.model,
				effort = excluded.effort,
				resultSummary = excluded.resultSummary,
				toolUses = excluded.toolUses,
				startEntryId = excluded.startEntryId,
				startParentId = excluded.startParentId,
				linkedEntryId = excluded.linkedEntryId,
				terminalEntryId = excluded.terminalEntryId,
				terminalParentId = excluded.terminalParentId`,
			activity.ID, activity.RecordSourcePath, activity.ParentSourcePath, activity.ParentSessionID, activity.ParentActivityID,
			activity.ChildSessionPath, activity.Status, activity.StartedAt, activity.CompletedAt,
			activity.Kind, activity.Namespace, activity.Title, activity.Description, activity.Model,
			activity.Effort, activity.ResultSummary, activity.ToolUses, activity.StartEntryID,
			activity.StartParentID, activity.LinkedEntryID, activity.TerminalEntryID, activity.TerminalParentID); err != nil {
			return 0, err
		}
	}

	insert, err := tx.Prepare(`
		INSERT INTO messages (id, entryId, sourcePath, sessionId, timestamp, type, content, prose, charCount, activityId, activityRole)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			entryId      = excluded.entryId,
			sourcePath   = excluded.sourcePath,
			sessionId    = excluded.sessionId,
			timestamp    = excluded.timestamp,
			type         = excluded.type,
			content      = excluded.content,
			prose        = excluded.prose,
			charCount    = excluded.charCount,
			activityId   = excluded.activityId,
			activityRole = excluded.activityRole`)
	if err != nil {
		return 0, err
	}
	defer insert.Close()

	for _, msg := range messages {
		if _, err := insert.Exec(msg.ID, msg.EntryID, path, msg.SessionID, msg.Timestamp, msg.Type,
			msg.Content, msg.Prose, msg.CharCount, msg.ActivityID, msg.ActivityRole); err != nil {
			return 0, err
		}
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
	return len(messages), nil
}

func sessionName(path string) string {
	return strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
}

type indexedActivity struct {
	ID               string
	RecordSourcePath string
	ParentSourcePath string
	ParentSessionID  string
	ParentActivityID string
	ChildSessionPath string
	Status           string
	StartedAt        int64
	CompletedAt      int64
	Kind             string
	Namespace        string
	Title            string
	Description      string
	Model            string
	Effort           string
	ResultSummary    string
	ToolUses         int
	StartEntryID     string
	StartParentID    string
	LinkedEntryID    string
	TerminalEntryID  string
	TerminalParentID string
}

func aggregateActivities(path, parentSessionID string, events []transcript.ActivityEvent) []indexedActivity {
	byID := make(map[string]*indexedActivity)
	for _, event := range events {
		activity := byID[event.ActivityID]
		if activity == nil {
			activity = &indexedActivity{ID: event.ActivityID, RecordSourcePath: path, ParentSourcePath: path, ParentSessionID: parentSessionID}
			byID[event.ActivityID] = activity
		}
		if event.ParentActivityID != "" {
			activity.ParentActivityID = event.ParentActivityID
		}
		switch event.Kind {
		case "started":
			activity.StartEntryID = event.EntryID
			activity.StartParentID = event.ParentID
			activity.Status = event.Status
			activity.StartedAt = event.StartedAt
			activity.Kind = event.ActivityKind
			activity.Namespace = event.Namespace
			activity.Title = event.Title
			activity.Description = event.Description
			activity.Model = event.Model
			activity.Effort = event.Effort
		case "linked":
			activity.LinkedEntryID = event.EntryID
			if event.SessionFile != "" {
				if childPath, err := filepath.Abs(event.SessionFile); err == nil {
					activity.ChildSessionPath = childPath
				} else {
					activity.ChildSessionPath = event.SessionFile
				}
			}
		case "terminal":
			activity.TerminalEntryID = event.EntryID
			activity.TerminalParentID = event.ParentID
			activity.Status = event.Status
			activity.CompletedAt = event.CompletedAt
			activity.ResultSummary = event.ResultSummary
			activity.ToolUses = event.ToolUses
		}
	}

	out := make([]indexedActivity, 0, len(byID))
	for _, activity := range byID {
		out = append(out, *activity)
	}
	slices.SortFunc(out, func(a, b indexedActivity) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func linkActivityParents(messages []transcript.Message, events []transcript.ActivityEvent) {
	byEntry := make(map[string]int, len(messages))
	for i := range messages {
		byEntry[messages[i].EntryID] = i
	}
	for _, event := range events {
		idx, ok := byEntry[event.ParentID]
		if !ok || event.ActivityID == "" {
			continue
		}
		role := event.Kind
		if role == "started" {
			role = "start"
		}
		if messages[idx].ActivityID == "" || messages[idx].ActivityRole == "" {
			messages[idx].ActivityID = event.ActivityID
			messages[idx].ActivityRole = role
		}
	}
}

func selectColumns(alias string) string {
	return strings.Join([]string{
		alias + ".id",
		alias + ".entryId",
		alias + ".sessionId",
		alias + ".timestamp",
		alias + ".type",
		alias + ".content",
		alias + ".prose",
		alias + ".charCount",
		"COALESCE(NULLIF(" + alias + ".activityId, ''), childActivity.activityId, '')",
		"COALESCE(NULLIF(" + alias + ".activityRole, ''), CASE WHEN childActivity.activityId IS NOT NULL THEN 'child' ELSE '' END)",
		"COALESCE(NULLIF(directActivity.parentActivityId, ''), childActivity.parentActivityId, '')",
		"COALESCE(NULLIF(directActivity.parentSessionId, ''), directParentSession.sessionId, NULLIF(childActivity.parentSessionId, ''), childParentSession.sessionId, '')",
		"COALESCE(directChildSession.sessionId, childSession.sessionId, '')",
	}, ", ")
}

func activityJoins(alias string) string {
	return " LEFT JOIN activities childActivity ON childActivity.childSessionPath = " + alias + ".sourcePath" +
		" LEFT JOIN activities directActivity ON directActivity.activityId = " + alias + ".activityId" +
		" LEFT JOIN sessions childSession ON childSession.sourcePath = childActivity.childSessionPath" +
		" LEFT JOIN sessions directChildSession ON directChildSession.sourcePath = directActivity.childSessionPath" +
		" LEFT JOIN sessions childParentSession ON childParentSession.sourcePath = childActivity.parentSourcePath" +
		" LEFT JOIN sessions directParentSession ON directParentSession.sourcePath = directActivity.parentSourcePath"
}

// Last returns the most recent messages, newest first.
func (d *DB) Last(opts LastOptions) ([]transcript.Message, error) {
	query := `SELECT ` + selectColumns("m") + ` FROM messages m` + activityJoins("m") + ` WHERE 1 = 1`
	var args []any

	if opts.SessionID != "" {
		query += ` AND m.sessionId = ?`
		args = append(args, opts.SessionID)
	}
	if opts.Type != "" {
		query += ` AND m.type = ?`
		args = append(args, opts.Type)
	}
	if opts.ProseOnly {
		query += ` AND m.prose != ''`
	}
	if opts.Hours > 0 {
		query += ` AND m.timestamp >= ?`
		args = append(args, since(opts.Hours))
	}
	query += ` ORDER BY m.timestamp DESC`
	if opts.N > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.N)
	}

	return d.collect(query, args...)
}

// Search returns messages matching opts.Query, most relevant first.
func (d *DB) Search(opts SearchOptions) ([]transcript.Message, error) {
	match := ftsQuery(opts.Query, opts.Any)
	if opts.Raw {
		match = strings.TrimSpace(opts.Query)
	}
	if match == "" {
		return nil, nil
	}
	if opts.ProseOnly {
		match = "{prose} : (" + match + ")"
	}

	// The window is the slice of recent history to search within: the newest
	// WindowMessages messages and/or everything inside WindowHours.
	window := `SELECT rowid FROM messages WHERE 1 = 1`
	var args []any
	if opts.SessionID != "" {
		window += ` AND sessionId = ?`
		args = append(args, opts.SessionID)
	} else if opts.ExcludeSessionID != "" {
		window += ` AND sessionId != ?`
		args = append(args, opts.ExcludeSessionID)
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
		SELECT ` + selectColumns("m") + `
		FROM messages_fts f
		JOIN messages m ON m.rowid = f.rowid` + activityJoins("m") + `
		WHERE f.messages_fts MATCH ?
		  AND m.rowid IN (` + window + `)`
	args = append([]any{match}, args...)

	if opts.Type != "" {
		query += ` AND m.type = ?`
		args = append(args, opts.Type)
	}
	query += ` ORDER BY bm25(f.messages_fts), m.timestamp DESC`

	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	msgs, err := d.collect(query, args...)
	if err != nil && opts.Raw {
		// The caller wrote this expression, so a failure here is theirs.
		return nil, fmt.Errorf("%w: %w", ErrBadQuery, err)
	}
	return msgs, err
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
		if err := rows.Scan(&m.ID, &m.EntryID, &m.SessionID, &m.Timestamp, &m.Type,
			&m.Content, &m.Prose, &m.CharCount, &m.ActivityID, &m.ActivityRole,
			&m.ParentActivityID, &m.ParentSessionID, &m.ChildSessionID); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func since(hours int) int64 {
	return time.Now().Add(-time.Duration(hours) * time.Hour).UnixMilli()
}

// ftsQuery turns a user pattern into an FTS5 MATCH expression. Each whitespace
// separated term becomes a quoted prefix term, so punctuation-heavy patterns
// like "display.cpp" are matched literally instead of parsed as FTS syntax.
// Terms are ANDed unless any is set.
func ftsQuery(pattern string, any bool) string {
	var terms []string
	for _, field := range strings.Fields(pattern) {
		escaped := strings.ReplaceAll(field, `"`, `""`)
		terms = append(terms, `"`+escaped+`"*`)
	}
	join := " "
	if any {
		join = " OR "
	}
	return strings.Join(terms, join)
}

// TermCount reports how many terms a pattern will search for.
func TermCount(pattern string) int { return len(strings.Fields(pattern)) }

// ErrNoSuchID and ErrAmbiguousID report why a message address did not resolve.
var (
	ErrNoSuchID    = errors.New("no message with that id")
	ErrAmbiguousID = errors.New("ambiguous id prefix")
)

// ErrBadQuery reports that a raw query is not valid FTS5 syntax.
var ErrBadQuery = errors.New("invalid query")

// Around returns the message addressed by id together with its neighbours in
// the same session, oldest first. id may be a unique prefix. When proseOnly is
// set the neighbours are limited to messages that said something, but the
// addressed message is always included.
func (d *DB) Around(id string, before, after int, proseOnly bool) ([]transcript.Message, error) {
	target, rowid, err := d.resolve(id)
	if err != nil {
		return nil, err
	}

	filter := ""
	if proseOnly {
		filter = ` AND m.prose != ''`
	}

	// Ties on timestamp are broken by rowid, which follows transcript order.
	earlier, err := d.collect(`SELECT `+selectColumns("m")+` FROM messages m`+activityJoins("m")+`
		WHERE m.sessionId = ?
		  AND (m.timestamp < ? OR (m.timestamp = ? AND m.rowid < ?))`+filter+`
		ORDER BY m.timestamp DESC, m.rowid DESC LIMIT ?`,
		target.SessionID, target.Timestamp, target.Timestamp, rowid, before)
	if err != nil {
		return nil, err
	}
	slices.Reverse(earlier)

	later, err := d.collect(`SELECT `+selectColumns("m")+` FROM messages m`+activityJoins("m")+`
		WHERE m.sessionId = ?
		  AND (m.timestamp > ? OR (m.timestamp = ? AND m.rowid > ?))`+filter+`
		ORDER BY m.timestamp ASC, m.rowid ASC LIMIT ?`,
		target.SessionID, target.Timestamp, target.Timestamp, rowid, after)
	if err != nil {
		return nil, err
	}

	out := make([]transcript.Message, 0, len(earlier)+1+len(later))
	out = append(out, earlier...)
	out = append(out, target)
	return append(out, later...), nil
}

// resolve turns an exact id or a unique prefix into one message.
func (d *DB) resolve(id string) (transcript.Message, int64, error) {
	var rowid int64
	msgs, err := d.collect(`SELECT `+selectColumns("m")+` FROM messages m`+activityJoins("m")+` WHERE m.id = ?`, id)
	if err != nil {
		return transcript.Message{}, 0, err
	}
	if len(msgs) == 0 {
		// Fall back to prefix matching, fetching two rows to spot ambiguity.
		msgs, err = d.collect(`SELECT `+selectColumns("m")+` FROM messages m`+activityJoins("m")+`
			WHERE m.id LIKE ? ESCAPE '\' ORDER BY m.id LIMIT 2`, escapeLike(id)+"%")
		if err != nil {
			return transcript.Message{}, 0, err
		}
		switch len(msgs) {
		case 0:
			return transcript.Message{}, 0, fmt.Errorf("%w: %q", ErrNoSuchID, id)
		case 1:
		default:
			return transcript.Message{}, 0, fmt.Errorf("%w: %q matches at least %q and %q",
				ErrAmbiguousID, id, msgs[0].ID, msgs[1].ID)
		}
	}
	if err := d.sql.QueryRow(`SELECT rowid FROM messages WHERE id = ?`, msgs[0].ID).
		Scan(&rowid); err != nil {
		return transcript.Message{}, 0, err
	}
	return msgs[0], rowid, nil
}

// escapeLike neutralises LIKE wildcards so an id prefix matches literally.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
