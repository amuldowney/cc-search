// Package index stores transcript messages in a SQLite database with an FTS5
// index and answers memory-refresh and concept-research queries against it.
package index

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"

	"github.com/amuldowney/cc-search/internal/transcript"
)

// schemaVersion is bumped whenever the tables change. An index written by a
// different version is discarded rather than migrated — it is all derived data.
const schemaVersion = 7

// SchemaVersion is the current derived-index schema version.
const SchemaVersion = schemaVersion

var (
	errSchemaMismatch = errors.New("incompatible index schema")
	errNewerSchema    = errors.New("index schema is newer than this binary")
)

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
  activityRole TEXT,
  toolCalls    TEXT,
  toolResults  TEXT
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
	path      string
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
		db.path = path
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
	db.path = path
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
		if version > schemaVersion {
			return nil, fmt.Errorf("%w: index has schema version %d, but this binary supports %d; deploy a newer cc-search",
				errNewerSchema, version, schemaVersion)
		}
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
	// Full integrity checking is intentionally not done here. This function
	// runs before every CLI query, and quick_check scans the entire database
	// (including the large FTS tables). `doctor` calls DB.Check explicitly.
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

// WithLifecycleLock runs fn while holding the cross-process lifecycle lock.
// Reuse Open's initial lock when it is still held; otherwise acquire the
// path-specific lock for the duration of the callback.
func (d *DB) WithLifecycleLock(fn func() error) (err error) {
	if d.lifecycle != nil {
		return fn()
	}
	lifecycle, err := acquireLifecycleLock(d.path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, lifecycle.Close()) }()
	return fn()
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
		INSERT INTO messages (id, entryId, sourcePath, sessionId, timestamp, type, content, prose, charCount, activityId, activityRole, toolCalls, toolResults)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			activityRole = excluded.activityRole,
			toolCalls    = excluded.toolCalls,
			toolResults  = excluded.toolResults`)
	if err != nil {
		return 0, err
	}
	defer insert.Close()

	for _, msg := range messages {
		calls, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return 0, err
		}
		results, err := json.Marshal(msg.ToolResults)
		if err != nil {
			return 0, err
		}
		if _, err := insert.Exec(msg.ID, msg.EntryID, path, msg.SessionID, msg.Timestamp, msg.Type,
			msg.Content, msg.Prose, msg.CharCount, msg.ActivityID, msg.ActivityRole,
			string(calls), string(results)); err != nil {
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
		case "task-notification":
			if activity.StartedAt == 0 {
				activity.StartedAt = event.StartedAt
			}
			activity.Status = event.Status
			if event.CompletedAt != 0 {
				activity.CompletedAt = event.CompletedAt
			}
			if event.Title != "" {
				activity.Title = event.Title
			}
			if event.Description != "" {
				activity.Description = event.Description
			}
			activity.Kind = event.ActivityKind
			activity.Namespace = event.Namespace
			activity.ResultSummary = event.ResultSummary
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
		alias + ".toolCalls",
		alias + ".toolResults",
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
		var calls, results string
		if err := rows.Scan(&m.ID, &m.EntryID, &m.SessionID, &m.Timestamp, &m.Type,
			&m.Content, &m.Prose, &m.CharCount, &calls, &results, &m.ActivityID, &m.ActivityRole,
			&m.ParentActivityID, &m.ParentSessionID, &m.ChildSessionID); err != nil {
			return nil, err
		}
		if calls != "" {
			_ = json.Unmarshal([]byte(calls), &m.ToolCalls)
		}
		if results != "" {
			_ = json.Unmarshal([]byte(results), &m.ToolResults)
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

// SessionOptions selects session summaries. Query matches indexed prose and
// CWD is an exact working-directory filter.
type SessionOptions struct {
	Query            string
	Limit            int
	CWD              string
	Any              bool
	ExcludeSessionID string
}

// SessionSummary is the compact row returned by the sessions command.
type SessionSummary struct {
	SessionID       string
	CWD             string
	Visibility      string
	ParentSessionID string
	LastTimestamp   int64
	LastPreview     string
	MessageCount    int
}

// Sessions returns one summary per indexed transcript session, newest first.
func (d *DB) Sessions(opts SessionOptions) ([]SessionSummary, error) {
	query := `
		SELECT COALESCE(s.sessionId, ''), COALESCE(s.cwd, ''), COALESCE(s.visibility, ''),
		       COALESCE(parent.sessionId, NULLIF(s.parentSession, ''), ''),
		       COALESCE(MAX(m.timestamp), 0),
		       COALESCE((SELECT CASE WHEN latest.prose != '' THEN latest.prose ELSE latest.content END
						 FROM messages latest WHERE latest.sourcePath = s.sourcePath
						 ORDER BY latest.timestamp DESC, latest.rowid DESC LIMIT 1), ''),
		       COUNT(m.id)
		FROM sessions s
		LEFT JOIN sessions parent ON parent.sourcePath = s.parentSession
		LEFT JOIN messages m ON m.sourcePath = s.sourcePath
		WHERE 1 = 1`
	args := []any{}
	if opts.CWD != "" {
		query += ` AND s.cwd = ?`
		args = append(args, opts.CWD)
	}
	if opts.ExcludeSessionID != "" {
		query += ` AND s.sessionId != ?`
		args = append(args, opts.ExcludeSessionID)
	}
	if opts.Query != "" {
		match := "{prose} : (" + ftsQuery(opts.Query, opts.Any) + ")"
		query += ` AND EXISTS (
			SELECT 1 FROM messages_fts f
			JOIN messages matched ON matched.rowid = f.rowid
			WHERE matched.sourcePath = s.sourcePath AND f.messages_fts MATCH ?)`
		args = append(args, match)
	}
	query += ` GROUP BY s.sourcePath, s.sessionId, s.cwd, s.visibility, s.parentSession, parent.sessionId
		ORDER BY MAX(m.timestamp) DESC, s.sessionId`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}

	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SessionSummary{}
	for rows.Next() {
		var summary SessionSummary
		if err := rows.Scan(&summary.SessionID, &summary.CWD, &summary.Visibility,
			&summary.ParentSessionID, &summary.LastTimestamp, &summary.LastPreview,
			&summary.MessageCount); err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, rows.Err()
}

// ActivityOptions selects recorded Pi subagent activities.
type ActivityOptions struct {
	SessionID string
	Status    string
	Limit     int
}

// ActivityRecord is the normalized activity metadata stored in the index.
type ActivityRecord struct {
	ActivityID       string
	Status           string
	Title            string
	Description      string
	Model            string
	Effort           string
	ToolUses         int
	StartedAt        int64
	CompletedAt      int64
	ParentSessionID  string
	ChildSessionID   string
	ParentActivityID string
	Kind             string
	Namespace        string
	ResultSummary    string
	StartEntryID     string
	StartParentID    string
	LinkedEntryID    string
	TerminalEntryID  string
	TerminalParentID string
}

const activityColumns = `COALESCE(a.activityId, ''), COALESCE(a.status, ''), COALESCE(a.title, ''),
	COALESCE(a.description, ''), COALESCE(a.model, ''), COALESCE(a.effort, ''),
	COALESCE(a.toolUses, 0), COALESCE(a.startedAt, 0), COALESCE(a.completedAt, 0),
	COALESCE(NULLIF(a.parentSessionId, ''), parent.sessionId, NULLIF(a.parentSourcePath, ''), ''),
	COALESCE(child.sessionId, ''), COALESCE(a.parentActivityId, ''), COALESCE(a.kind, ''),
	COALESCE(a.namespace, ''), COALESCE(a.resultSummary, ''), COALESCE(a.startEntryId, ''),
	COALESCE(a.startParentId, ''), COALESCE(a.linkedEntryId, ''),
	COALESCE(a.terminalEntryId, ''), COALESCE(a.terminalParentId, '')`

func (d *DB) Activities(opts ActivityOptions) ([]ActivityRecord, error) {
	query := `SELECT ` + activityColumns + `
		FROM activities a
		LEFT JOIN sessions parent ON parent.sourcePath = a.parentSourcePath
		LEFT JOIN sessions child ON child.sourcePath = a.childSessionPath
		WHERE 1 = 1`
	args := []any{}
	if opts.SessionID != "" {
		query += ` AND (COALESCE(NULLIF(a.parentSessionId, ''), parent.sessionId) = ? OR child.sessionId = ?)`
		args = append(args, opts.SessionID, opts.SessionID)
	}
	if opts.Status != "" {
		query += ` AND lower(a.status) = lower(?)`
		args = append(args, opts.Status)
	}
	query += ` ORDER BY COALESCE(NULLIF(a.startedAt, 0), a.completedAt) DESC, a.activityId`
	if opts.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, opts.Limit)
	}
	return d.collectActivities(query, args...)
}

var (
	ErrNoSuchActivity    = errors.New("no activity with that id")
	ErrAmbiguousActivity = errors.New("ambiguous activity id")
)

// Activity resolves an exact activity id or a unique prefix.
func (d *DB) Activity(id string) (ActivityRecord, error) {
	query := `SELECT ` + activityColumns + `
		FROM activities a
		LEFT JOIN sessions parent ON parent.sourcePath = a.parentSourcePath
		LEFT JOIN sessions child ON child.sourcePath = a.childSessionPath
		WHERE a.activityId = ?`
	rows, err := d.collectActivities(query, id)
	if err != nil {
		return ActivityRecord{}, err
	}
	if len(rows) == 0 {
		rows, err = d.collectActivities(`SELECT `+activityColumns+`
			FROM activities a
			LEFT JOIN sessions parent ON parent.sourcePath = a.parentSourcePath
			LEFT JOIN sessions child ON child.sourcePath = a.childSessionPath
			WHERE a.activityId LIKE ? ESCAPE '\\' ORDER BY a.activityId LIMIT 2`, escapeLike(id)+"%")
		if err != nil {
			return ActivityRecord{}, err
		}
		switch len(rows) {
		case 0:
			return ActivityRecord{}, fmt.Errorf("%w: %q", ErrNoSuchActivity, id)
		case 1:
		default:
			return ActivityRecord{}, fmt.Errorf("%w: %q matches at least %q and %q",
				ErrAmbiguousActivity, id, rows[0].ActivityID, rows[1].ActivityID)
		}
	}
	return rows[0], nil
}

func (d *DB) collectActivities(query string, args ...any) ([]ActivityRecord, error) {
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ActivityRecord{}
	for rows.Next() {
		var activity ActivityRecord
		if err := rows.Scan(&activity.ActivityID, &activity.Status, &activity.Title,
			&activity.Description, &activity.Model, &activity.Effort, &activity.ToolUses,
			&activity.StartedAt, &activity.CompletedAt, &activity.ParentSessionID,
			&activity.ChildSessionID, &activity.ParentActivityID, &activity.Kind,
			&activity.Namespace, &activity.ResultSummary, &activity.StartEntryID,
			&activity.StartParentID, &activity.LinkedEntryID, &activity.TerminalEntryID,
			&activity.TerminalParentID); err != nil {
			return nil, err
		}
		out = append(out, activity)
	}
	return out, rows.Err()
}

// CommandOptions selects exact tool invocations. Query is matched against the
// full indexed message, so either invocation arguments or a paired result can
// locate a command.
type CommandOptions struct {
	Query         string
	Tool          string
	SessionID     string
	Limit         int
	IncludeOutput bool
}

// CommandRecord is a normalized historical tool invocation.
type CommandRecord struct {
	Tool      string
	Arguments string
	SessionID string
	MessageID string
	Timestamp int64
	Output    string
}

// Commands returns tool calls ordered newest first. Tool results are paired by
// their durable tool-call id; old records without ids fall back to a nearby
// message in the same session. Only messages matching Query are read from the
// FTS index; nearby rows are fetched on demand, rather than scanning every
// tool record in the corpus.
func (d *DB) Commands(opts CommandOptions) ([]CommandRecord, error) {
	var candidates []transcript.Message
	if opts.Query != "" {
		var err error
		// A command result is discovered through either its call or its
		// paired output. Overfetch a bounded number of FTS hits so a small
		// command limit does not require materializing every prose match.
		candidateLimit := 0
		if opts.Limit > 0 {
			candidateLimit = opts.Limit * 16
			if candidateLimit < 256 {
				candidateLimit = 256
			}
		}
		candidates, err = d.Search(SearchOptions{Query: opts.Query, Limit: candidateLimit, SessionID: opts.SessionID, ProseOnly: false})
		if err != nil {
			return nil, err
		}
	} else {
		var err error
		candidates, err = d.collect(`SELECT ` + selectColumns("m") + ` FROM messages m` + activityJoins("m") +
			` WHERE m.toolCalls != '' OR m.toolResults != ''
			ORDER BY m.sessionId, m.timestamp, m.rowid`)
		if err != nil {
			return nil, err
		}
	}

	type call struct {
		record CommandRecord
		id     string
	}
	callsByID := map[string]*call{}
	matchedCalls := map[*call]bool{}
	pendingResults := []struct {
		message transcript.Message
		result  transcript.ToolResult
	}{}
	addCall := func(message transcript.Message, tool transcript.ToolCall) *call {
		if opts.Tool != "" && !strings.EqualFold(opts.Tool, tool.Name) {
			return nil
		}
		key := messageKey(message) + "\x00" + tool.ID
		if existing := callsByID[key]; existing != nil {
			return existing
		}
		item := &call{record: CommandRecord{Tool: tool.Name, Arguments: tool.Arguments,
			SessionID: message.SessionID, MessageID: message.ID, Timestamp: message.Timestamp}, id: tool.ID}
		if tool.ID != "" {
			callsByID[key] = item
			// This secondary map is filled below with the session-scoped id.
			callsByID[message.SessionID+"\x00"+tool.ID] = item
		}
		return item
	}
	for _, message := range candidates {
		if opts.SessionID != "" && message.SessionID != opts.SessionID {
			continue
		}
		for _, tool := range message.ToolCalls {
			if item := addCall(message, tool); item != nil {
				matchedCalls[item] = true
			}
		}
		for _, result := range message.ToolResults {
			pendingResults = append(pendingResults, struct {
				message transcript.Message
				result  transcript.ToolResult
			}{message, result})
		}
	}

	for _, pending := range pendingResults {
		item := callsByID[pending.message.SessionID+"\x00"+pending.result.ID]
		if item == nil {
			nearby, err := d.toolMessagesBefore(pending.message.SessionID, pending.message.Timestamp)
			if err != nil {
				return nil, err
			}
			for _, message := range nearby {
				for _, tool := range message.ToolCalls {
					if pending.result.ID == "" || tool.ID == pending.result.ID {
						item = addCall(transcript.Message{ID: message.ID, SessionID: message.SessionID, Timestamp: message.Timestamp}, tool)
						if item != nil {
							break
						}
					}
				}
				if item != nil {
					break
				}
			}
		}
		if item == nil {
			continue
		}
		matchedCalls[item] = true
		if opts.IncludeOutput {
			appendToolOutput(&item.record, pending.result.Content)
		}
	}

	items := make([]*call, 0, len(matchedCalls))
	for item := range matchedCalls {
		items = append(items, item)
	}
	slices.SortFunc(items, func(a, b *call) int {
		if a.record.Timestamp != b.record.Timestamp {
			if a.record.Timestamp > b.record.Timestamp {
				return -1
			}
			return 1
		}
		return strings.Compare(a.record.MessageID, b.record.MessageID)
	})
	if opts.Limit > 0 && len(items) > opts.Limit {
		items = items[:opts.Limit]
	}
	if opts.IncludeOutput {
		for _, item := range items {
			// A result that matched the query was already attached above. The
			// forward lookup is only needed when the call itself matched.
			if item.record.Output != "" {
				continue
			}
			nearby, err := d.toolMessagesAfter(item.record.SessionID, item.record.Timestamp)
			if err != nil {
				return nil, err
			}
			for _, message := range nearby {
				for _, result := range message.ToolResults {
					if item.id == "" || result.ID == item.id {
						appendToolOutput(&item.record, result.Content)
					}
				}
			}
		}
	}

	out := make([]CommandRecord, 0, len(items))
	for _, item := range items {
		out = append(out, item.record)
	}
	return out, nil
}

func appendToolOutput(record *CommandRecord, content string) {
	if content == "" {
		return
	}
	if record.Output != "" {
		record.Output += "\n"
	}
	record.Output += content
}

type toolMessage struct {
	ID          string
	SessionID   string
	Timestamp   int64
	ToolCalls   []transcript.ToolCall
	ToolResults []transcript.ToolResult
}

func (d *DB) toolMessagesBefore(sessionID string, timestamp int64) ([]toolMessage, error) {
	return d.toolMessages(`m.timestamp <= ? ORDER BY m.timestamp DESC, m.rowid DESC`, sessionID, timestamp)
}

func (d *DB) toolMessagesAfter(sessionID string, timestamp int64) ([]toolMessage, error) {
	return d.toolMessages(`m.timestamp >= ? ORDER BY m.timestamp ASC, m.rowid ASC`, sessionID, timestamp)
}

func (d *DB) toolMessages(order string, sessionID string, timestamp int64) ([]toolMessage, error) {
	rows, err := d.sql.Query(`SELECT m.id, m.sessionId, m.timestamp, m.toolCalls, m.toolResults
		FROM messages m WHERE m.sessionId = ? AND (m.toolCalls != '' OR m.toolResults != '') AND `+order+` LIMIT 64`, sessionID, timestamp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []toolMessage{}
	for rows.Next() {
		var message toolMessage
		var calls, results string
		if err := rows.Scan(&message.ID, &message.SessionID, &message.Timestamp, &calls, &results); err != nil {
			return nil, err
		}
		if calls != "" {
			_ = json.Unmarshal([]byte(calls), &message.ToolCalls)
		}
		if results != "" {
			_ = json.Unmarshal([]byte(results), &message.ToolResults)
		}
		out = append(out, message)
	}
	return out, rows.Err()
}

func messageKey(msg transcript.Message) string { return msg.SessionID + "\x00" + msg.ID }

// ContextMessage is a deduplicated message plus the search hits whose windows
// caused it to be included.
type ContextMessage struct {
	Message transcript.Message
	HitIDs  []string
}

// ExpandContext expands selected hits and deduplicates overlapping windows.
func (d *DB) ExpandContext(hits []transcript.Message, before, after int, proseOnly bool) ([]ContextMessage, error) {
	byKey := map[string]*ContextMessage{}
	for _, hit := range hits {
		messages, err := d.Around(hit.ID, before, after, proseOnly)
		if err != nil {
			return nil, err
		}
		for _, message := range messages {
			key := messageKey(message)
			item := byKey[key]
			if item == nil {
				item = &ContextMessage{Message: message}
				byKey[key] = item
			}
			if !slices.Contains(item.HitIDs, hit.ID) {
				item.HitIDs = append(item.HitIDs, hit.ID)
			}
		}
	}
	out := make([]ContextMessage, 0, len(byKey))
	for _, item := range byKey {
		out = append(out, *item)
	}
	slices.SortStableFunc(out, func(a, b ContextMessage) int {
		if a.Message.SessionID != b.Message.SessionID {
			return strings.Compare(a.Message.SessionID, b.Message.SessionID)
		}
		if a.Message.Timestamp != b.Message.Timestamp {
			if a.Message.Timestamp < b.Message.Timestamp {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Message.ID, b.Message.ID)
	})
	return out, nil
}

// IndexInfo reports derived-index counts and the transcript files it contains.
type IndexInfo struct {
	IndexPath     string
	SchemaVersion int
	MessageCount  int
	SessionCount  int
	ActivityCount int
	FileCount     int
	Sources       []SourceInfo
	IndexHealthy  bool
}

type SourceInfo struct {
	Path         string
	MessageCount int
	SessionCount int
}

// Info returns counts and source paths from the index.
func (d *DB) Info() (IndexInfo, error) {
	info := IndexInfo{IndexPath: d.path}
	if err := d.sql.QueryRow(`PRAGMA user_version`).Scan(&info.SchemaVersion); err != nil {
		return info, err
	}
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&info.MessageCount); err != nil {
		return info, err
	}
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&info.SessionCount); err != nil {
		return info, err
	}
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM activities`).Scan(&info.ActivityCount); err != nil {
		return info, err
	}
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&info.FileCount); err != nil {
		return info, err
	}
	rows, err := d.sql.Query(`SELECT f.path, COUNT(m.id),
			(SELECT COUNT(*) FROM sessions s WHERE s.sourcePath = f.path)
		FROM files f LEFT JOIN messages m ON m.sourcePath = f.path
		GROUP BY f.path ORDER BY f.path`)
	if err != nil {
		return info, err
	}
	defer rows.Close()
	for rows.Next() {
		var source SourceInfo
		if err := rows.Scan(&source.Path, &source.MessageCount, &source.SessionCount); err != nil {
			return info, err
		}
		info.Sources = append(info.Sources, source)
	}
	if err := rows.Err(); err != nil {
		return info, err
	}
	// Opening the database and completing the count queries above provide a
	// cheap operational check. The full integrity scan belongs to `doctor`.
	info.IndexHealthy = true
	return info, nil
}

// Check runs SQLite's lightweight integrity check.
func (d *DB) Check() error {
	var result string
	if err := d.sql.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("index integrity check: %s", result)
	}
	return nil
}

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
