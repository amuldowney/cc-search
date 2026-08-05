package index

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrewmuldowney/cc-search/internal/transcript"
)

// writeTranscript writes a JSONL transcript file for sessionID whose lines are
// built from msgs, each entry being (type, content, minutesAgo).
type msg struct {
	typ        string
	content    string
	minutesAgo int
}

func writeTranscript(t *testing.T, dir, sessionID string, msgs []msg) string {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	var body string
	for i, m := range msgs {
		ts := time.Now().Add(-time.Duration(m.minutesAgo) * time.Minute).UTC()
		body += fmt.Sprintf(
			`{"type":%q,"uuid":"%s-%d","sessionId":%q,"timestamp":%q,"message":{"content":%q}}`+"\n",
			m.typ, sessionID, i, sessionID, ts.Format(time.RFC3339Nano), m.content)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeRawTranscript writes literal JSONL lines, for records that the simple
// msg fixture cannot express (block content, tool calls).
func writeRawTranscript(t *testing.T, dir, sessionID string, lines []string) {
	t.Helper()
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newIndex opens a fresh index over a transcript dir seeded with msgs.
func newIndex(t *testing.T, msgs []msg) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	writeTranscript(t, dir, "session-a", msgs)
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}
	return db, dir
}

func TestSyncIndexesMessagesFromTranscripts(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "build the search index", 10},
		{"assistant", "index built", 9},
	})

	got, err := db.Last(LastOptions{N: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
}

func TestLastReturnsNewestFirst(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "oldest", 30},
		{"user", "middle", 20},
		{"user", "newest", 10},
	})

	got, err := db.Last(LastOptions{N: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].Content != "newest" || got[1].Content != "middle" {
		t.Errorf("got %q, %q; want newest, middle", got[0].Content, got[1].Content)
	}
}

func TestLastHoursFiltersByAge(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "three hours ago", 180},
		{"user", "ten minutes ago", 10},
	})

	got, err := db.Last(LastOptions{Hours: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
	if got[0].Content != "ten minutes ago" {
		t.Errorf("Content = %q", got[0].Content)
	}
}

func TestLastFiltersBySession(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "session-a", []msg{{"user", "from a", 10}})
	writeTranscript(t, dir, "session-b", []msg{{"user", "from b", 5}})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	got, err := db.Last(LastOptions{N: 10, SessionID: "session-b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "from b" {
		t.Fatalf("got %+v, want only session-b message", got)
	}
}

func TestSearchFindsMatchingMessages(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "the waveform lookup table is in display.cpp", 30},
		{"user", "unrelated chatter about lunch", 20},
	})

	got, err := db.Search(SearchOptions{Query: "waveform"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if got[0].Content != "the waveform lookup table is in display.cpp" {
		t.Errorf("Content = %q", got[0].Content)
	}
}

func TestSearchRespectsLimit(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "grayscale one", 40},
		{"user", "grayscale two", 30},
		{"user", "grayscale three", 20},
	})

	got, err := db.Search(SearchOptions{Query: "grayscale", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
}

func TestSearchFiltersByType(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "deploy the server", 30},
		{"assistant", "deploy finished", 20},
	})

	got, err := db.Search(SearchOptions{Query: "deploy", Type: "assistant"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Type != "assistant" {
		t.Fatalf("got %+v, want one assistant message", got)
	}
}

func TestSearchExcludesOneSession(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "current", []msg{{"user", "needle current", 10}})
	writeTranscript(t, dir, "history", []msg{{"user", "needle history", 20}})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	got, err := db.Search(SearchOptions{Query: "needle", ExcludeSessionID: "current"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "history" {
		t.Fatalf("got %+v, want only the history session", got)
	}
}

func TestSearchExplicitSessionOverridesExclusion(t *testing.T) {
	db, dir := newIndex(t, []msg{{"user", "needle current", 10}})
	writeTranscript(t, dir, "history", []msg{{"user", "needle history", 20}})
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	got, err := db.Search(SearchOptions{
		Query: "needle", SessionID: "session-a", ExcludeSessionID: "session-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SessionID != "session-a" {
		t.Fatalf("got %+v, want the explicitly selected session", got)
	}
}

func TestSearchWindowHoursLimitsHistory(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "battery dispatch old", 60 * 5},
		{"user", "battery dispatch recent", 30},
	})

	got, err := db.Search(SearchOptions{Query: "dispatch", WindowHours: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "battery dispatch recent" {
		t.Fatalf("got %+v, want only the recent message", got)
	}
}

func TestSearchWindowMessagesLimitsHistory(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "portfolio snapshot", 50},
		{"user", "filler one", 40},
		{"user", "filler two", 30},
		{"user", "filler three", 20},
	})

	got, err := db.Search(SearchOptions{Query: "portfolio", WindowMessages: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d results, want 0 — the match is outside the 3-message window", len(got))
	}

	got, err = db.Search(SearchOptions{Query: "portfolio", WindowMessages: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1 — a 4-message window reaches the match", len(got))
	}
}

func TestOpenRebuildsCorruptIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	if err := os.WriteFile(path, []byte("this is not a sqlite database"), 0o644); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a corrupt index returned an error: %v", err)
	}
	defer db.Close()

	dir := t.TempDir()
	writeTranscript(t, dir, "session-a", []msg{{"user", "after recovery", 5}})
	if _, err := db.Sync(dir); err != nil {
		t.Fatalf("Sync after recovery failed: %v", err)
	}
	got, err := db.Last(LastOptions{N: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1", len(got))
	}
}

func TestRemoveDatabaseFilesLeavesPrimaryWhenSidecarCannotBeRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	if err := os.WriteFile(path, []byte("database"), 0o644); err != nil {
		t.Fatal(err)
	}
	sidecar := path + "-journal"
	if err := os.Mkdir(sidecar, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sidecar, "in-use"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := removeDatabaseFiles(path); err == nil {
		t.Fatal("removeDatabaseFiles succeeded despite an undeletable recovery sidecar")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("primary index was removed after sidecar cleanup failed: %v", err)
	}
}

func TestRemoveDatabaseFilesRemovesRollbackJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	if err := os.WriteFile(path, []byte("database"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal := path + "-journal"
	if err := os.WriteFile(journal, []byte("stale journal"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := removeDatabaseFiles(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback journal still exists: %v", err)
	}
}

func TestOpenPreservesIndexOnTransientBusyError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	locker, err := sql.Open("sqlite3", path+"?_busy_timeout=100")
	if err != nil {
		t.Fatal(err)
	}
	locker.SetMaxOpenConns(1)
	if _, err := locker.Exec(`BEGIN EXCLUSIVE`); err != nil {
		locker.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = locker.Exec(`ROLLBACK`)
		_ = locker.Close()
	})

	if reopened, err := Open(path); err == nil {
		_ = reopened.Close()
		t.Fatal("Open succeeded while another process held an exclusive lock")
	} else if !strings.Contains(strings.ToLower(err.Error()), "locked") &&
		!strings.Contains(strings.ToLower(err.Error()), "busy") {
		t.Fatalf("Open returned unrelated error for a locked index: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("Open replaced the existing index after a transient lock error")
	}
}

func TestLifecycleLockTimeoutNamesIndexAndWait(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	held, err := acquireLifecycleLockWithTimeout(path, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	started := time.Now()
	_, err = acquireLifecycleLockWithTimeout(path, time.Millisecond)
	if err == nil {
		t.Fatal("acquireLifecycleLock succeeded while the index lock was held")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("lock timeout took %s, want a bounded wait", elapsed)
	}
	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "1ms") {
		t.Fatalf("lock timeout error = %q, want index path and wait duration", err)
	}
}

func TestOpenDiscardsIndexBuiltByAnOlderSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	legacy, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly the v1 schema: no prose column, single-column FTS table.
	if _, err := legacy.Exec(`
		CREATE TABLE messages (
		  id TEXT PRIMARY KEY, sessionId TEXT, timestamp INTEGER, type TEXT,
		  content TEXT, charCount INTEGER, isRecap INTEGER NOT NULL DEFAULT 0);
		CREATE VIRTUAL TABLE messages_fts USING fts5(content, content=messages, content_rowid=rowid);
		CREATE TABLE files (path TEXT PRIMARY KEY, mtime INTEGER, size INTEGER);
		INSERT INTO messages VALUES ('stale', 's0', 0, 'user', 'from the old schema', 19, 0);`); err != nil {
		t.Fatal(err)
	}
	legacy.Close()

	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open on an outdated index returned an error: %v", err)
	}
	defer db.Close()

	dir := t.TempDir()
	writeTranscript(t, dir, "session-a", []msg{{"user", "fresh message", 5}})
	if _, err := db.Sync(dir); err != nil {
		t.Fatalf("Sync after migration failed: %v", err)
	}
	got, err := db.Last(LastOptions{N: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "fresh message" {
		t.Fatalf("got %+v, want only the freshly indexed message", got)
	}
}

func TestLastProseOnlySkipsMessagesWithoutProse(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	writeRawTranscript(t, dir, "session-a", []string{
		fmt.Sprintf(`{"type":"assistant","uuid":"l1","sessionId":"session-a","timestamp":%q,`+
			`"message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`, ts),
		fmt.Sprintf(`{"type":"assistant","uuid":"l2","sessionId":"session-a","timestamp":%q,`+
			`"message":{"content":[{"type":"text","text":"here is the listing"}]}}`, ts),
	})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	all, err := db.Last(LastOptions{N: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered Last got %d messages, want 2", len(all))
	}

	got, err := db.Last(LastOptions{N: 10, ProseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "l2" {
		t.Fatalf("got %+v, want only the message that says something", got)
	}
}

func TestSearchProseOnlyIgnoresToolArguments(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	writeRawTranscript(t, dir, "session-a", []string{
		fmt.Sprintf(`{"type":"assistant","uuid":"t1","sessionId":"session-a","timestamp":%q,`+
			`"message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"caddy.json"}},`+
			`{"type":"text","text":"checking now"}]}}`, ts),
		fmt.Sprintf(`{"type":"assistant","uuid":"t2","sessionId":"session-a","timestamp":%q,`+
			`"message":{"content":[{"type":"text","text":"the caddy config moved to the HP"}]}}`, ts),
	})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	all, err := db.Search(SearchOptions{Query: "caddy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("unfiltered search got %d results, want 2", len(all))
	}

	got, err := db.Search(SearchOptions{Query: "caddy", ProseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1 — the tool-argument match is not prose", len(got))
	}
	if got[0].ID != "t2" {
		t.Errorf("matched %q, want the message whose prose mentions caddy", got[0].ID)
	}
}

func TestSearchProseOnlyMatchesPlainStringContent(t *testing.T) {
	db, _ := newIndex(t, []msg{{"user", "restart the caddy container", 10}})

	got, err := db.Search(SearchOptions{Query: "caddy", ProseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1 — plain string content is all prose", len(got))
	}
}

func TestSearchRecapsOnlyExcludesNonRecaps(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"assistant", "recap: the firmware rollout is done", 30},
		{"assistant", "the firmware rollout is done", 20},
	})

	got, err := db.Search(SearchOptions{Query: "firmware", RecapsOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	if !got[0].IsRecap {
		t.Error("result is not flagged as a recap")
	}
}

func TestSearchPreferRecapsBoostsRecapsToTop(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"assistant", "recap: we discussed the caddy proxy at length", 30},
		{"assistant", "caddy proxy", 20},
	})

	got, err := db.Search(SearchOptions{Query: "caddy", PreferRecaps: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	if !got[0].IsRecap {
		t.Errorf("first result is not a recap: %q", got[0].Content)
	}
}

func TestSyncSkipsUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "session-a", []msg{{"user", "hello", 10}})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	stats, err := db.Sync(dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionsIndexed != 0 {
		t.Errorf("SessionsIndexed = %d, want 0 on an unchanged rescan", stats.SessionsIndexed)
	}
	if stats.FilesSkipped != 1 {
		t.Errorf("FilesSkipped = %d, want 1", stats.FilesSkipped)
	}
}

func TestSyncReindexesChangedFileWithoutDuplicating(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "session-a", []msg{{"user", "first", 10}})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	path := writeTranscript(t, dir, "session-a", []msg{
		{"user", "first", 10},
		{"user", "second", 5},
	})
	future := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	got, err := db.Last(LastOptions{N: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2 (no duplicates)", len(got))
	}
}

func TestSyncSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session-a.jsonl")
	good := fmt.Sprintf(
		`{"type":"user","uuid":"u1","sessionId":"session-a","timestamp":%q,"message":{"content":"survivor"}}`,
		time.Now().UTC().Format(time.RFC3339Nano))
	body := "{\"broken\": \n" + good + "\n\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	stats, err := db.Sync(dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.MessagesIndexed != 1 {
		t.Fatalf("MessagesIndexed = %d, want 1", stats.MessagesIndexed)
	}
}

func TestRebuildSingleSessionLeavesOthersIntact(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "session-a", []msg{{"user", "from a", 10}})
	writeTranscript(t, dir, "session-b", []msg{{"user", "from b", 10}})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	stats, err := db.Rebuild(dir, "session-a")
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionsIndexed != 1 {
		t.Errorf("SessionsIndexed = %d, want 1", stats.SessionsIndexed)
	}

	got, err := db.Last(LastOptions{N: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2 — session-b should survive", len(got))
	}
}

func TestSearchEmptyResultIsNotAnError(t *testing.T) {
	db, _ := newIndex(t, []msg{{"user", "hello", 10}})

	got, err := db.Search(SearchOptions{Query: "nonexistentterm"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d results, want 0", len(got))
	}
}

func TestSearchQuotesQueryWithFTSSyntax(t *testing.T) {
	db, _ := newIndex(t, []msg{{"user", "the file is display.cpp in src", 10}})

	got, err := db.Search(SearchOptions{Query: "display.cpp"})
	if err != nil {
		t.Fatalf("query with FTS punctuation returned an error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
}

func readFixture(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	var lines []string
	for i, text := range []string{"one", "two", "three", "four", "five"} {
		ts := base.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339Nano)
		lines = append(lines, fmt.Sprintf(
			`{"type":"user","uuid":"aaaa%d-msg","sessionId":"session-a","timestamp":%q,`+
				`"message":{"content":%q}}`, i, ts, text))
	}
	// A second session must never leak into the neighbours of the first.
	lines = append(lines, fmt.Sprintf(
		`{"type":"user","uuid":"bbbb0-msg","sessionId":"session-b","timestamp":%q,`+
			`"message":{"content":"other session"}}`,
		base.Add(90*time.Second).UTC().Format(time.RFC3339Nano)))
	writeRawTranscript(t, dir, "session-a", lines[:5])
	writeRawTranscript(t, dir, "session-b", lines[5:])

	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}
	return db
}

func contents(msgs []transcript.Message) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

func TestAroundReturnsNeighboursInChronologicalOrder(t *testing.T) {
	db := readFixture(t)

	got, err := db.Around("aaaa2-msg", 1, 1, false)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"two", "three", "four"}
	if !slices.Equal(contents(got), want) {
		t.Errorf("got %v, want %v", contents(got), want)
	}
}

func TestAroundClampsAtSessionEdges(t *testing.T) {
	db := readFixture(t)

	got, err := db.Around("aaaa0-msg", 5, 1, false)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"one", "two"}
	if !slices.Equal(contents(got), want) {
		t.Errorf("got %v, want %v", contents(got), want)
	}
}

func TestAroundStaysWithinOneSession(t *testing.T) {
	db := readFixture(t)

	got, err := db.Around("aaaa2-msg", 5, 5, false)
	if err != nil {
		t.Fatal(err)
	}

	for _, m := range got {
		if m.SessionID != "session-a" {
			t.Errorf("neighbour from %s leaked in: %q", m.SessionID, m.Content)
		}
	}
	if len(got) != 5 {
		t.Errorf("got %d messages, want all 5 of session-a", len(got))
	}
}

func TestAroundResolvesUniquePrefix(t *testing.T) {
	db := readFixture(t)

	got, err := db.Around("aaaa3", 0, 0, false)
	if err != nil {
		t.Fatal(err)
	}

	if len(got) != 1 || got[0].Content != "four" {
		t.Fatalf("got %v, want just the addressed message", contents(got))
	}
}

func TestAroundRejectsAmbiguousPrefix(t *testing.T) {
	db := readFixture(t)

	_, err := db.Around("aaaa", 0, 0, false)

	if !errors.Is(err, ErrAmbiguousID) {
		t.Errorf("err = %v, want ErrAmbiguousID", err)
	}
}

func TestAroundReportsUnknownID(t *testing.T) {
	db := readFixture(t)

	_, err := db.Around("nosuchid", 0, 0, false)

	if !errors.Is(err, ErrNoSuchID) {
		t.Errorf("err = %v, want ErrNoSuchID", err)
	}
}

func TestAroundProseOnlySkipsToolNeighboursButKeepsTarget(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	ts := func(i int) string {
		return base.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339Nano)
	}
	writeRawTranscript(t, dir, "session-a", []string{
		fmt.Sprintf(`{"type":"assistant","uuid":"r0","sessionId":"session-a","timestamp":%q,`+
			`"message":{"content":[{"type":"text","text":"spoken before"}]}}`, ts(0)),
		fmt.Sprintf(`{"type":"assistant","uuid":"r1","sessionId":"session-a","timestamp":%q,`+
			`"message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"ls"}}]}}`, ts(1)),
		fmt.Sprintf(`{"type":"assistant","uuid":"r2","sessionId":"session-a","timestamp":%q,`+
			`"message":{"content":[{"type":"text","text":"spoken after"}]}}`, ts(2)),
	})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	got, err := db.Around("r0", 0, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].ID != "r2" {
		t.Errorf("got %v, want the tool-only neighbour skipped", contents(got))
	}

	// The addressed message is always returned, even with no prose of its own.
	target, err := db.Around("r1", 0, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(target) != 1 || target[0].ID != "r1" {
		t.Errorf("got %v, want the addressed message itself", contents(target))
	}
}

func TestSearchAnyMatchesEitherTerm(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "alpha and beta together", 30},
		{"user", "gamma on its own", 20},
		{"user", "nothing relevant", 10},
	})

	strict, err := db.Search(SearchOptions{Query: "alpha gamma"})
	if err != nil {
		t.Fatal(err)
	}
	if len(strict) != 0 {
		t.Fatalf("got %d results, want 0 — terms are ANDed by default", len(strict))
	}

	got, err := db.Search(SearchOptions{Query: "alpha gamma", Any: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 with Any", len(got))
	}
}

func TestSearchMatchesInflectedForms(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "we discussed the caching strategy", 30},
		{"assistant", "we deployed it cleanly", 20},
	})

	for _, tc := range []struct{ query, want string }{
		{"cache", "we discussed the caching strategy"},
		{"deploy", "we deployed it cleanly"},
	} {
		got, err := db.Search(SearchOptions{Query: tc.query})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Content != tc.want {
			t.Errorf("%q matched %v, want %q", tc.query, contents(got), tc.want)
		}
	}
}

func TestSearchKeepsIdentifiersLiteralUnderStemming(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "set CARGO_TARGET_DIR before building", 30},
		{"user", "the host is 192.168.1.112 on the lan", 20},
		{"user", "edit display.cpp for the waveform", 10},
	})

	for _, q := range []string{"CARGO_TARGET_DIR", "192.168.1.112", "display.cpp"} {
		got, err := db.Search(SearchOptions{Query: q})
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if len(got) != 1 {
			t.Errorf("%q matched %d messages, want 1 — identifiers must stay literal",
				q, len(got))
		}
	}
}

// TestSearchStemmingSplitsMentNominalizations records a known limitation of
// the porter stemmer rather than a desired behaviour. "deploy", "deployed"
// and "deploying" all stem to "deploi", but stripping "-ment" from
// "deployment" yields "deploy", a different stem, so the noun and the verb do
// not find each other. This is the cost measured when stemming was adopted;
// the gains on -ing/-ed forms were far larger.
func TestSearchStemmingSplitsMentNominalizations(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "the deployment finished", 30},
		{"user", "we deployed it", 20},
	})

	got, err := db.Search(SearchOptions{Query: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "we deployed it" {
		t.Errorf("%q matched, want only the verb form — if this now also matches "+
			"the noun, the stemmer improved and this test can go", contents(got))
	}
}

func TestSearchRawAcceptsBooleanOperators(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"user", "alpha and beta together", 30},
		{"user", "gamma on its own", 20},
		{"user", "beta without the others", 10},
	})

	got, err := db.Search(SearchOptions{Query: "alpha OR gamma", Raw: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("OR matched %d, want 2", len(got))
	}

	got, err = db.Search(SearchOptions{Query: "beta NOT alpha", Raw: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "beta without the others" {
		t.Errorf("NOT matched %v, want only the message without alpha", contents(got))
	}
}

func TestSearchRawReportsSyntaxErrors(t *testing.T) {
	db, _ := newIndex(t, []msg{{"user", "edit display.cpp today", 10}})

	_, err := db.Search(SearchOptions{Query: "display.cpp", Raw: true})

	if !errors.Is(err, ErrBadQuery) {
		t.Fatalf("err = %v, want ErrBadQuery", err)
	}
	// Quoting is the documented fix, so it must actually work.
	got, qerr := db.Search(SearchOptions{Query: `"display.cpp"`, Raw: true})
	if qerr != nil {
		t.Fatalf("quoted raw query failed: %v", qerr)
	}
	if len(got) != 1 {
		t.Errorf("quoted raw query matched %d, want 1", len(got))
	}
}

func TestSearchRawStillHonoursProseOnly(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	writeRawTranscript(t, dir, "session-a", []string{
		fmt.Sprintf(`{"type":"assistant","uuid":"w1","sessionId":"session-a","timestamp":%q,`+
			`"message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"alpha.json"}}]}}`, ts),
		fmt.Sprintf(`{"type":"assistant","uuid":"w2","sessionId":"session-a","timestamp":%q,`+
			`"message":{"content":[{"type":"text","text":"alpha was discussed"}]}}`, ts),
	})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}

	got, err := db.Search(SearchOptions{Query: "alpha OR beta", Raw: true, ProseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "w2" {
		t.Errorf("got %v, want the tool-argument match still excluded", contents(got))
	}
}

// writePiTranscript writes a pi-style transcript in a nested per-directory
// folder: a session header line followed by role-tagged message records.
func writePiTranscript(t *testing.T, root, folder, filename, sessionID string, lines []string) string {
	t.Helper()
	dir := filepath.Join(root, folder)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	header := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-07-31T23:34:07.731Z","cwd":"/work"}`, sessionID)
	body := header + "\n" + strings.Join(lines, "\n") + "\n"
	path := filepath.Join(dir, filename+".jsonl")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSyncIndexesNestedPiTranscripts(t *testing.T) {
	dir := t.TempDir()
	writePiTranscript(t, dir, "--home-andrew-Projects--", "2026-07-31T23-34-07-731Z_019fba87",
		"019fba87", []string{
			`{"type":"message","id":"m1","timestamp":"2026-07-31T23:34:18.152Z","message":{"role":"user","content":[{"type":"text","text":"fix the hub retry loop"}]}}`,
			`{"type":"message","id":"m2","timestamp":"2026-07-31T23:34:19.197Z","message":{"role":"assistant","content":[{"type":"text","text":"retry loop fixed"}]}}`,
		})

	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	stats, err := db.Sync(dir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionsIndexed != 1 {
		t.Fatalf("SessionsIndexed = %d, want 1", stats.SessionsIndexed)
	}

	got, err := db.Search(SearchOptions{Query: "retry loop"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("Search returned %d messages, want 2", len(got))
	}
	if got[0].SessionID != "019fba87" {
		t.Errorf("SessionID = %q, want the id from the pi session header", got[0].SessionID)
	}
}

func TestRebuildMatchesPiSessionByUUIDSuffix(t *testing.T) {
	dir := t.TempDir()
	writePiTranscript(t, dir, "nested", "2026-07-31T23-34-07-731Z_019fba87",
		"019fba87", []string{
			`{"type":"message","id":"m1","timestamp":"2026-07-31T23:34:18.152Z","message":{"role":"user","content":[{"type":"text","text":"hello"}]}}`,
		})

	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	stats, err := db.Rebuild(dir, "019fba87")
	if err != nil {
		t.Fatal(err)
	}
	if stats.SessionsIndexed != 1 {
		t.Errorf("SessionsIndexed = %d, want 1 (uuid suffix of the pi filename)", stats.SessionsIndexed)
	}
}
