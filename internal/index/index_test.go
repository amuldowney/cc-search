package index

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
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
