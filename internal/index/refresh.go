package index

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// RefreshInterval controls refresh throttling and snapshot freshness.
const RefreshInterval = 30 * time.Second

// ErrNeedsRefresh indicates that a read-only snapshot is missing or is not a
// compatible WAL index. Callers can use Refresh before retrying the read.
var ErrNeedsRefresh = errors.New("index needs refresh")

// RefreshStatus describes refresh activity for one normalized transcript-root
// set. Timestamps are Unix milliseconds. Error is a sanitized category only.
type RefreshStatus struct {
	LastSuccess int64  `json:"lastSuccess"`
	LastAttempt int64  `json:"lastAttempt"`
	Stale       bool   `json:"stale"`
	Error       string `json:"error,omitempty"`
}

// RefreshOptions selects whether a refresh bypasses its persisted throttle and
// whether it should skip instead of waiting when another writer owns the lock.
type RefreshOptions struct {
	Force      bool
	Background bool
}

// Refresh is the sole coordinated writer entry point for transcript snapshots.
// Refresh metadata is keyed by the normalized, sorted set of transcript roots.
func Refresh(path string, roots []string, opts RefreshOptions) (stats SyncStats, err error) {
	rootSet, profile, err := refreshProfile(roots)
	if err != nil {
		return stats, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return stats, fmt.Errorf("create index directory: %w", err)
	}

	var lifecycle *lifecycleLock
	if opts.Background {
		lifecycle, err = acquireLifecycleLockNonblocking(path)
		if errors.Is(err, errLifecycleLockBusy) {
			return SyncStats{}, nil
		}
	} else {
		lifecycle, err = acquireLifecycleLock(path)
	}
	if err != nil {
		return stats, err
	}
	defer func() { err = errors.Join(err, lifecycle.Close()) }()

	// Schema initialization, compatibility recovery, and the refresh metadata
	// table are all writer-only operations performed under the lifecycle lock.
	db, err := openForRefresh(path)
	if err != nil {
		return stats, err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	if _, err := db.sql.Exec(`CREATE TABLE IF NOT EXISTS refresh_state (
		profile TEXT PRIMARY KEY,
		roots TEXT NOT NULL,
		last_success INTEGER NOT NULL DEFAULT 0,
		last_attempt INTEGER NOT NULL DEFAULT 0,
		error TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return stats, fmt.Errorf("initialize refresh metadata: %w", err)
	}

	var lastAttempt int64
	err = db.sql.QueryRow(`SELECT last_attempt FROM refresh_state WHERE profile = ?`, profile).Scan(&lastAttempt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return stats, fmt.Errorf("read refresh metadata: %w", err)
	}
	now := time.Now()
	if !opts.Force && lastAttempt > 0 && now.Sub(time.UnixMilli(lastAttempt)) < RefreshInterval {
		return SyncStats{}, nil
	}

	attemptMillis := now.UnixMilli()
	if _, err := db.sql.Exec(`
		INSERT INTO refresh_state(profile, roots, last_attempt)
		VALUES(?, ?, ?)
		ON CONFLICT(profile) DO UPDATE SET roots=excluded.roots, last_attempt=excluded.last_attempt`,
		profile, rootSet, attemptMillis); err != nil {
		return stats, fmt.Errorf("record refresh attempt: %w", err)
	}

	var refreshErr error
	for _, root := range rootsForSync(rootSet) {
		rootStats, syncErr := db.Sync(root)
		stats.SessionsIndexed += rootStats.SessionsIndexed
		stats.MessagesIndexed += rootStats.MessagesIndexed
		stats.FilesSkipped += rootStats.FilesSkipped
		if syncErr != nil && !errors.Is(syncErr, fs.ErrNotExist) {
			refreshErr = errors.Join(refreshErr, syncErr)
		}
	}
	if refreshErr != nil {
		if _, markErr := db.sql.Exec(`UPDATE refresh_state SET error = ? WHERE profile = ?`, "refresh_failed", profile); markErr != nil {
			refreshErr = errors.Join(refreshErr, fmt.Errorf("record refresh failure: %w", markErr))
		}
		return stats, fmt.Errorf("refresh transcripts: %w", refreshErr)
	}

	if _, err := db.sql.Exec(`UPDATE refresh_state SET last_success = ?, error = '' WHERE profile = ?`,
		time.Now().UnixMilli(), profile); err != nil {
		return stats, fmt.Errorf("record refresh success: %w", err)
	}
	return stats, nil
}

// OpenReader opens an established compatible WAL index in SQLite read-only
// mode. It neither acquires the writer lifecycle lock nor creates files.
func OpenReader(path string) (*DB, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: index does not exist", ErrNeedsRefresh)
		}
		return nil, fmt.Errorf("stat index: %w", err)
	}

	dsn, err := readOnlyDSN(path)
	if err != nil {
		return nil, err
	}
	handle, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	// Keep every query on the same SQLite connection and transaction for the
	// lifetime of this DB. Search and follow-up queries then observe one WAL
	// snapshot even while a refresh commits replacements in the background.
	handle.SetMaxOpenConns(1)
	handle.SetMaxIdleConns(1)
	inSnapshot := false
	fail := func(err error) (*DB, error) {
		var rollbackErr error
		if inSnapshot {
			_, rollbackErr = handle.Exec(`ROLLBACK`)
			inSnapshot = false
		}
		return nil, errors.Join(err, rollbackErr, handle.Close())
	}
	if err := handle.Ping(); err != nil {
		return fail(fmt.Errorf("open read-only index: %w", err))
	}
	if _, err := handle.Exec(`BEGIN`); err != nil {
		return fail(fmt.Errorf("begin read snapshot: %w", err))
	}
	inSnapshot = true
	// BEGIN is deferred; this first database read establishes the WAL snapshot
	// before compatibility checks or any caller-visible query can run.
	var schemaObjects int
	if err := handle.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&schemaObjects); err != nil {
		return fail(fmt.Errorf("start read snapshot: %w", err))
	}

	var version int
	if err := handle.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return fail(fmt.Errorf("read schema version: %w", err))
	}
	if version > schemaVersion {
		return fail(fmt.Errorf("%w: index has schema version %d, but this binary supports %d; deploy a newer cc-search",
			errNewerSchema, version, schemaVersion))
	}
	empty, err := isEmpty(handle)
	if err != nil {
		return fail(fmt.Errorf("check schema contents: %w", err))
	}
	if version != schemaVersion || empty {
		return fail(fmt.Errorf("%w: index has schema version %d, want %d", ErrNeedsRefresh, version, schemaVersion))
	}
	if err := validateReaderSchema(handle); err != nil {
		return fail(err)
	}
	var journalMode string
	if err := handle.QueryRow(`PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		return fail(fmt.Errorf("read journal mode: %w", err))
	}
	if journalMode != "wal" {
		return fail(fmt.Errorf("%w: index journal mode is %q, want WAL", ErrNeedsRefresh, journalMode))
	}
	return &DB{sql: handle, path: path, readerSnapshot: true}, nil
}

// OpenSnapshot returns an established snapshot immediately, even if it is
// stale. It synchronously bootstraps a missing index or uninitialized root
// profile, and Force always performs a synchronous refresh first.
func OpenSnapshot(path string, roots []string, force bool) (*DB, error) {
	if force {
		if _, err := Refresh(path, roots, RefreshOptions{Force: true}); err != nil {
			return nil, err
		}
		return OpenReader(path)
	}

	db, err := OpenReader(path)
	if err != nil {
		if !errors.Is(err, ErrNeedsRefresh) {
			return nil, err
		}
		if _, refreshErr := Refresh(path, roots, RefreshOptions{Force: true}); refreshErr != nil {
			return nil, refreshErr
		}
		return OpenReader(path)
	}
	status, err := db.Freshness(roots)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if status.LastSuccess > 0 {
		return db, nil
	}
	if err := db.Close(); err != nil {
		return nil, err
	}
	if _, err := Refresh(path, roots, RefreshOptions{Force: true}); err != nil {
		return nil, err
	}
	return OpenReader(path)
}

// Freshness returns persisted refresh state for roots. It is read-only; a
// missing metadata table or root profile is reported as stale without error.
func (d *DB) Freshness(roots []string) (RefreshStatus, error) {
	_, profile, err := refreshProfile(roots)
	if err != nil {
		return RefreshStatus{}, err
	}
	var tableCount int
	if err := d.sql.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='refresh_state'`).Scan(&tableCount); err != nil {
		return RefreshStatus{}, err
	}
	if tableCount == 0 {
		return RefreshStatus{Stale: true}, nil
	}
	var status RefreshStatus
	if err := d.sql.QueryRow(`SELECT last_success, last_attempt, error FROM refresh_state WHERE profile = ?`, profile).
		Scan(&status.LastSuccess, &status.LastAttempt, &status.Error); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RefreshStatus{Stale: true}, nil
		}
		return RefreshStatus{}, err
	}
	status.Stale = status.LastSuccess == 0 || time.Since(time.UnixMilli(status.LastSuccess)) >= RefreshInterval
	return status, nil
}

func openForRefresh(path string) (*DB, error) {
	db, err := open(path)
	if err == nil || !shouldRebuild(err) {
		return db, err
	}
	if err := removeDatabaseFiles(path); err != nil {
		return nil, err
	}
	return open(path)
}

func validateReaderSchema(handle *sql.DB) error {
	var missing []string
	for _, table := range []string{"messages", "messages_fts", "sessions", "activities", "files", "message_projections", "source_projections", "tool_invocations", "tool_results", "command_search_content", "command_fts"} {
		var count int
		if err := handle.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name = ? AND type IN ('table','view')`, table).Scan(&count); err != nil {
			return fmt.Errorf("validate index schema: %w", err)
		}
		if count == 0 {
			missing = append(missing, table)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("index schema is missing required tables (%v); stop clients, remove the derived database and its WAL/SHM sidecars, then run cc-search refresh", missing)
	}
	return nil
}

func readOnlyDSN(path string) (string, error) {
	return sqliteDSN(path, true)
}

func sqliteDSN(path string, readOnly bool) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve index path: %w", err)
	}
	u := &url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	query := url.Values{}
	if readOnly {
		query.Set("mode", "ro")
		query.Set("_query_only", "1")
	}
	query.Set("_busy_timeout", "5000")
	return u.String() + "?" + query.Encode(), nil
}

func refreshProfile(roots []string) (encoded, profile string, err error) {
	normalized := make([]string, 0, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return "", "", fmt.Errorf("resolve transcript root: %w", err)
		}
		normalized = append(normalized, filepath.Clean(absolute))
	}
	slices.Sort(normalized)
	normalized = slices.Compact(normalized)
	data, err := json.Marshal(normalized)
	if err != nil {
		return "", "", fmt.Errorf("encode transcript roots: %w", err)
	}
	digest := sha256.Sum256(data)
	return string(data), hex.EncodeToString(digest[:]), nil
}

func rootsForSync(encoded string) []string {
	var roots []string
	_ = json.Unmarshal([]byte(encoded), &roots)
	return roots
}
