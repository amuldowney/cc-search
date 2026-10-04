package index

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenReaderDoesNotWaitForWriterAndIsReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO files(path,mtime,size) VALUES('uncommitted',1,1)`); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		reader, err := OpenReader(path)
		if err == nil {
			defer reader.Close()
			var count int
			err = reader.sql.QueryRow(`SELECT count(*) FROM files WHERE path='uncommitted'`).Scan(&count)
			if err == nil && count != 0 {
				err = errors.New("snapshot exposed uncommitted writer data")
			}
			if err == nil {
				if _, writeErr := reader.sql.Exec(`INSERT INTO files(path,mtime,size) VALUES('forbidden',1,1)`); writeErr == nil {
					err = errors.New("read-only snapshot accepted INSERT")
				}
			}
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OpenReader waited for lifecycle or SQLite writer lock")
	}
}

func TestOpenReaderReadsCommittedSnapshotDuringReplacement(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "session-a", []msg{{"user", "committed original", 1}})
	path := filepath.Join(t.TempDir(), "index.db")
	if _, err := Refresh(path, []string{root}, RefreshOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	writerDone := make(chan error, 1)
	go func() {
		conn, err := writer.sql.Conn(t.Context())
		if err != nil {
			writerDone <- err
			return
		}
		defer conn.Close()
		_, err = conn.ExecContext(t.Context(), `BEGIN IMMEDIATE`)
		if err == nil {
			_, err = conn.ExecContext(t.Context(), `DELETE FROM messages`)
		}
		if err == nil {
			close(entered)
			<-release
			_, err = conn.ExecContext(t.Context(), `ROLLBACK`)
		}
		writerDone <- err
	}()
	<-entered
	reader, err := OpenReader(path)
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	defer reader.Close()
	var count int
	if err := reader.sql.QueryRow(`SELECT count(*) FROM messages`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("reader saw %d messages, want committed snapshot of 1", count)
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
}

func TestOpenReaderKeepsSnapshotAcrossWriterCommit(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "session-a", []msg{{"user", "snapshot old marker", 1}})
	path := filepath.Join(t.TempDir(), "index.db")
	if _, err := Refresh(path, []string{root}, RefreshOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	oldReader, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer oldReader.Close()
	search := func(db *DB, query string) (int, error) {
		rows, err := db.Search(SearchOptions{Query: query})
		return len(rows), err
	}
	if n, err := search(oldReader, "snapshot old marker"); err != nil || n != 1 {
		t.Fatalf("initial old snapshot result count=%d err=%v", n, err)
	}
	writeTranscript(t, root, "session-a", []msg{{"user", "snapshot new marker with longer content", 0}})
	if _, err := Refresh(path, []string{root}, RefreshOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if n, err := search(oldReader, "snapshot old marker"); err != nil || n != 1 {
		t.Fatalf("pinned old snapshot result count=%d err=%v; want original row after writer commit", n, err)
	}
	fresh, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if n, err := search(fresh, "snapshot new marker"); err != nil || n != 1 {
		t.Fatalf("fresh snapshot result count=%d err=%v; want replacement row", n, err)
	}
}

func TestRefreshForceWaitsAndBackgroundSkipsHeldWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	root := t.TempDir()
	type result struct {
		stats SyncStats
		err   error
	}
	backgroundDone := make(chan result, 1)
	go func() {
		stats, err := Refresh(path, []string{root}, RefreshOptions{Background: true})
		backgroundDone <- result{stats, err}
	}()
	select {
	case got := <-backgroundDone:
		if got.err != nil || got.stats != (SyncStats{}) {
			t.Fatalf("background refresh = %+v, %v", got.stats, got.err)
		}
	case <-time.After(5 * time.Second):
		_ = writer.ReleaseLifecycleLock()
		<-backgroundDone
		t.Fatal("background refresh queued behind held writer lock")
	}
	writeTranscript(t, root, "forced", []msg{{"user", "force actually synchronized", 1}})
	started, resultCh := make(chan struct{}), make(chan result, 1)
	go func() {
		close(started)
		stats, err := Refresh(path, []string{root}, RefreshOptions{Force: true})
		resultCh <- result{stats, err}
	}()
	<-started
	if err := writer.ReleaseLifecycleLock(); err != nil {
		t.Fatal(err)
	}
	got := <-resultCh
	if got.err != nil || got.stats.MessagesIndexed != 1 {
		t.Fatalf("forced refresh = %+v, %v", got.stats, got.err)
	}
}

func TestRefreshConcurrentRequestsRecheckThrottleAfterLock(t *testing.T) {
	path, root := filepath.Join(t.TempDir(), "index.db"), t.TempDir()
	writeTranscript(t, root, "single", []msg{{"user", "only one refresh pass", 1}})
	held, err := acquireLifecycleLock(path)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		stats SyncStats
		err   error
	}
	ready, results := make(chan struct{}, 2), make(chan result, 2)
	for range 2 {
		go func() {
			ready <- struct{}{}
			stats, err := Refresh(path, []string{root}, RefreshOptions{})
			results <- result{stats, err}
		}()
	}
	<-ready
	<-ready
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	var total SyncStats
	for range 2 {
		got := <-results
		if got.err != nil {
			t.Fatal(got.err)
		}
		total.MessagesIndexed += got.stats.MessagesIndexed
		total.FilesSkipped += got.stats.FilesSkipped
	}
	if total.MessagesIndexed != 1 || total.FilesSkipped != 0 {
		t.Fatalf("concurrent stats=%+v, want one sync and one post-lock throttle skip", total)
	}
}

func TestRefreshThrottleAndRootProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	rootA, rootB := t.TempDir(), t.TempDir()
	writeTranscript(t, rootA, "a", []msg{{"user", "root alpha", 1}})
	writeTranscript(t, rootB, "b", []msg{{"user", "root bravo", 1}})
	first, err := Refresh(path, []string{rootA}, RefreshOptions{Force: true})
	if err != nil || first.MessagesIndexed != 1 {
		t.Fatalf("first refresh=%+v err=%v", first, err)
	}
	throttled, err := Refresh(path, []string{rootA}, RefreshOptions{})
	if err != nil || throttled != (SyncStats{}) {
		t.Fatalf("throttle=%+v err=%v", throttled, err)
	}
	rootsB := []string{rootB, filepath.Join(rootB, "missing")}
	second, err := Refresh(path, rootsB, RefreshOptions{})
	if err != nil || second.MessagesIndexed != 1 {
		t.Fatalf("different roots=%+v err=%v", second, err)
	}
	reader, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, roots := range [][]string{{rootA}, rootsB} {
		status, err := reader.Freshness(roots)
		if err != nil || status.LastSuccess == 0 || status.LastAttempt == 0 {
			t.Fatalf("status=%+v err=%v", status, err)
		}
	}
	var profiles int
	if err := reader.sql.QueryRow(`SELECT count(*) FROM refresh_state`).Scan(&profiles); err != nil {
		t.Fatal(err)
	}
	if profiles != 2 {
		t.Fatalf("profiles=%d, want 2 distinct root sets", profiles)
	}
}

func TestRefreshFailurePreservesSuccessAndSanitizesFreshness(t *testing.T) {
	root, path := t.TempDir(), filepath.Join(t.TempDir(), "index.db")
	writeTranscript(t, root, "a", []msg{{"user", "still readable", 1}})
	if _, err := Refresh(path, []string{root}, RefreshOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	before, err := reader.Freshness([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
	writeTranscript(t, root, "new", []msg{{"user", "new data must not replace snapshot", 0}})
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.sql.Exec(`CREATE TRIGGER fail_refresh BEFORE INSERT ON messages BEGIN SELECT RAISE(ABORT, 'sensitive transcript payload'); END`); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(path, []string{root}, RefreshOptions{Force: true}); err == nil {
		t.Fatal("triggered refresh succeeded")
	}
	reader, err = OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	after, err := reader.Freshness([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if after.LastSuccess != before.LastSuccess || after.LastAttempt == 0 || after.Error == "" {
		t.Fatalf("after=%+v before=%+v", after, before)
	}
	if strings.Contains(after.Error, "sensitive transcript payload") || strings.Contains(after.Error, root) {
		t.Fatalf("error leaked: %q", after.Error)
	}
	if stats, err := Refresh(path, []string{root}, RefreshOptions{}); err != nil || stats != (SyncStats{}) {
		t.Fatalf("failed attempt not throttled: %+v %v", stats, err)
	}
	if _, err := reader.Search(SearchOptions{Query: "readable"}); err != nil {
		t.Fatalf("snapshot unreadable: %v", err)
	}
}

func TestOpenSnapshotWaitsForCompleteBootstrap(t *testing.T) {
	path, root := filepath.Join(t.TempDir(), "index.db"), t.TempDir()
	writeTranscript(t, root, "first", []msg{{"user", "bootstrap complete marker", 1}})
	writeTranscript(t, root, "second", []msg{{"user", "bootstrap second marker", 1}})
	snapshot, err := OpenSnapshot(path, []string{root}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	got, err := snapshot.Search(SearchOptions{Query: "bootstrap"})
	if err != nil || len(got) != 2 {
		t.Fatalf("bootstrap results=%d err=%v", len(got), err)
	}
	status, err := snapshot.Freshness([]string{root})
	if err != nil || status.LastSuccess == 0 {
		t.Fatalf("bootstrap status=%+v err=%v", status, err)
	}
}

func TestOpenReaderSupportsReservedDatabasePathCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "plus+question?hash#")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "index+? #.db")
	writer, err := Open(filepath.Join(t.TempDir(), "writer.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(writer.path, path); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	rows, err := reader.sql.Query(`SELECT 1`)
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFreshnessIsReadOnlyAndUsesSuccessAge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	status, err := reader.Freshness(nil)
	if err != nil || !status.Stale || status.LastSuccess != 0 {
		t.Fatalf("empty status=%+v err=%v", status, err)
	}
	var count int
	if err := reader.sql.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='refresh_state'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("reader initialized writer metadata")
	}
	reader.Close()
	if _, err := writer.sql.Exec(`CREATE TABLE refresh_state (profile TEXT PRIMARY KEY, roots TEXT NOT NULL, last_success INTEGER NOT NULL, last_attempt INTEGER NOT NULL, error TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	encoded, profile, err := refreshProfile(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.sql.Exec(`INSERT INTO refresh_state VALUES(?, ?, ?, ?, '')`, profile, encoded, time.Now().Add(-RefreshInterval).UnixMilli(), 1); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err = OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	status, err = reader.Freshness(nil)
	if err != nil || !status.Stale || status.LastSuccess == 0 {
		t.Fatalf("aged status=%+v err=%v", status, err)
	}
}

func TestOpenReaderNeedsRefreshForMissingOldAndNewerIndexes(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := OpenReader(missing); !errors.Is(err, ErrNeedsRefresh) {
		t.Fatalf("missing error=%v", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reader created missing index: %v", err)
	}
	makeDB := func(path string, version int) {
		db, err := sql.Open("sqlite3", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`PRAGMA user_version = ` + fmt.Sprint(version)); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	old := filepath.Join(t.TempDir(), "old.db")
	makeDB(old, 1)
	if _, err := OpenReader(old); !errors.Is(err, ErrNeedsRefresh) {
		t.Fatalf("old error=%v", err)
	}
	newer := filepath.Join(t.TempDir(), "newer.db")
	makeDB(newer, SchemaVersion+1)
	if _, err := OpenReader(newer); !errors.Is(err, errNewerSchema) || !strings.Contains(err.Error(), "deploy a newer cc-search") {
		t.Fatalf("newer error=%v", err)
	}
	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("not sqlite data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReader(corrupt); err == nil || errors.Is(err, ErrNeedsRefresh) {
		t.Fatalf("corrupt error=%v", err)
	}
}
