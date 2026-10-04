package index

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/amuldowney/cc-search/internal/output"
)

func TestCompactReadProjectionPreservesPreviewAndOriginalCounts(t *testing.T) {
	body := strings.Repeat("  λ\twide   words\n", 80)
	storedBody := strings.TrimSpace(body)
	db, _ := newIndex(t, []msg{{"assistant", body, 10}})

	got, err := db.Search(SearchOptions{Query: "wide", PreviewLength: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	flat := strings.Join(strings.Fields(body), " ")
	if got[0].Content != string([]rune(flat)[:514]) {
		t.Fatalf("compact content prefix mismatch: len=%d", len([]rune(got[0].Content)))
	}
	if got[0].CharCount != len(storedBody) || got[0].ProseCharCount != len(storedBody) {
		t.Fatalf("original byte counts = content %d prose %d, want %d", got[0].CharCount, got[0].ProseCharCount, len(storedBody))
	}
	formatted := output.Format(got, output.Options{PreviewLength: 20, UseProse: true})
	want := string([]rune(flat)[:20]) + "…"
	if formatted.Results[0].Preview != want || formatted.Results[0].CharCount != len(storedBody) {
		t.Fatalf("formatted result = %+v, want preview %q and original byte count %d", formatted.Results[0], want, len(storedBody))
	}

	last, err := db.Last(LastOptions{N: 1, PreviewLength: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last[0].Content != got[0].Content {
		t.Fatalf("compact Last() = %+v, want compact search projection", last)
	}
	full, err := db.Last(LastOptions{N: 1, PreviewLength: 513})
	if err != nil {
		t.Fatal(err)
	}
	if full[0].Content != storedBody {
		t.Fatal("preview request beyond cached bound did not fall back to full body")
	}
}

func TestCompactProjectionHandlesTrailingWhitespaceAtMaxPreviewBoundary(t *testing.T) {
	body := strings.Repeat("x", 512) + " trailing words"
	db, _ := newIndex(t, []msg{{"assistant", body, 1}})
	got, err := db.Search(SearchOptions{Query: "trailing", PreviewLength: 512})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d boundary search results, want 1", len(got))
	}
	formatted := output.Format(got, output.Options{PreviewLength: 512})
	want := strings.Repeat("x", 512) + "…"
	if len(got) != 1 || formatted.Results[0].Preview != want {
		t.Fatalf("max-boundary preview = %q (cached runes %d), want ellipsis after 512 x runes", formatted.Results[0].Preview, len([]rune(got[0].Content)))
	}
}

func TestCompactProjectionHandlesTinyAndExactPreviewLengths(t *testing.T) {
	body := "é\t雪  tree"
	db, _ := newIndex(t, []msg{{"assistant", body, 1}})
	got, err := db.Search(SearchOptions{Query: "雪", PreviewLength: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d exact-preview results, want 1", len(got))
	}
	formatted := output.Format(got, output.Options{PreviewLength: 2})
	if formatted.Results[0].Preview != "é …" {
		t.Fatalf("exact-length preview = %q, want %q", formatted.Results[0].Preview, "é …")
	}
	got, err = db.Search(SearchOptions{Query: "雪", PreviewLength: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d tiny-preview results, want 1", len(got))
	}
	formatted = output.Format(got, output.Options{PreviewLength: 1})
	if formatted.Results[0].Preview != "é…" {
		t.Fatalf("tiny preview = %q, want %q", formatted.Results[0].Preview, "é…")
	}
}

func TestSessionsReadCachedSummaryAndRefreshAfterReindex(t *testing.T) {
	db, dir := newIndex(t, []msg{{"assistant", "old summary", 20}, {"assistant", "latest summary", 10}})
	rows, err := db.Sessions(SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MessageCount != 2 || rows[0].LastPreview != "latest summary" {
		t.Fatalf("initial summaries = %+v", rows)
	}
	matched, err := db.Sessions(SessionOptions{Query: "latest"})
	if err != nil {
		t.Fatal(err)
	}
	if len(matched) != 1 || matched[0].SessionID != rows[0].SessionID {
		t.Fatalf("FTS-filtered sessions = %+v, want the matching source", matched)
	}

	path := filepath.Join(dir, "session-a.jsonl")
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	content := fmt.Sprintf(`{"type":"assistant","uuid":"new","sessionId":"session-a","timestamp":%q,"message":{"content":"refreshed summary"}}`+"\n", ts)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Sync(dir); err != nil {
		t.Fatal(err)
	}
	rows, err = db.Sessions(SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].MessageCount != 1 || rows[0].LastPreview != "refreshed summary" {
		t.Fatalf("refreshed summaries = %+v", rows)
	}
}

func TestCommandsUseIndexedPairsAcrossSessionsAndRefreshReplacement(t *testing.T) {
	root := t.TempDir()
	writeRawTranscript(t, root, "session-a", []string{
		`{"type":"assistant","uuid":"call-a","sessionId":"session-a","timestamp":"2026-08-25T00:00:01Z","message":{"content":[{"type":"tool_use","id":"same","name":"Bash","input":{"command":"find red"}},{"type":"tool_use","id":"other","name":"Edit","input":{"file_path":"green"}}]}}`,
		`{"type":"user","uuid":"result-a","sessionId":"session-a","timestamp":"2026-08-25T00:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"same","content":"red output"}]}}`,
		`{"type":"user","uuid":"result-a2","sessionId":"session-a","timestamp":"2026-08-25T00:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"other","content":"green output"}]}}`,
	})
	writeRawTranscript(t, root, "session-b", []string{
		`{"type":"assistant","uuid":"call-b","sessionId":"session-b","timestamp":"2026-08-25T00:00:03Z","message":{"content":[{"type":"tool_use","id":"same","name":"Bash","input":{"command":"find blue"}}]}}`,
		`{"type":"user","uuid":"result-b","sessionId":"session-b","timestamp":"2026-08-25T00:00:04Z","message":{"content":[{"type":"tool_result","tool_use_id":"same","content":"blue output"}]}}`,
	})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}

	got, err := db.Commands(CommandOptions{Query: "red output", SessionID: "session-a", IncludeOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MessageID != "call-a" || got[0].Output != "red output" {
		t.Fatalf("result-only command match = %+v", got)
	}
	withoutOutput, err := db.Commands(CommandOptions{Query: "red output"})
	if err != nil {
		t.Fatal(err)
	}
	if len(withoutOutput) != 1 || withoutOutput[0].Output != "" {
		t.Fatalf("command without IncludeOutput = %+v, want no loaded output", withoutOutput)
	}
	blue, err := db.Commands(CommandOptions{Query: "blue output", IncludeOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(blue) != 1 || blue[0].SessionID != "session-b" || blue[0].Output != "blue output" {
		t.Fatalf("same-id cross-session result = %+v", blue)
	}
	all, err := db.Commands(CommandOptions{Tool: "Edit", IncludeOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Output != "green output" {
		t.Fatalf("tool filter with multiple invocations = %+v", all)
	}

	writeRawTranscript(t, root, "session-a", []string{
		`{"type":"assistant","uuid":"replacement","sessionId":"session-a","timestamp":"2026-08-26T00:00:01Z","message":{"content":[{"type":"tool_use","id":"new-id","name":"Bash","input":{"command":"new command"}}]}}`,
	})
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}
	stale, err := db.Commands(CommandOptions{Query: "red output", IncludeOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("stale command links survived transcript replacement: %+v", stale)
	}
}

func TestCommandsPairLegacyNoIDWithinSession(t *testing.T) {
	root := t.TempDir()
	writeRawTranscript(t, root, "legacy-a", []string{
		`{"type":"assistant","uuid":"call-a","sessionId":"legacy-a","timestamp":"2026-08-25T00:00:01Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"legacy find"}}]}}`,
		`{"type":"user","uuid":"result-a","sessionId":"legacy-a","timestamp":"2026-08-25T00:00:02Z","message":{"content":[{"type":"tool_result","content":"legacy output"}]}}`,
	})
	writeRawTranscript(t, root, "legacy-b", []string{
		`{"type":"assistant","uuid":"call-b","sessionId":"legacy-b","timestamp":"2026-08-25T00:00:03Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"unrelated"}}]}}`,
	})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}
	got, err := db.Commands(CommandOptions{Query: "legacy output", SessionID: "legacy-a", IncludeOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].MessageID != "call-a" || got[0].Output != "legacy output" {
		t.Fatalf("legacy result pairing = %+v", got)
	}
}

func TestMigrateReadProjectionsFromV7(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE messages (id TEXT PRIMARY KEY, entryId TEXT NOT NULL, sourcePath TEXT NOT NULL, sessionId TEXT, timestamp INTEGER, type TEXT, content TEXT, prose TEXT, charCount INTEGER, activityId TEXT, activityRole TEXT, toolCalls TEXT, toolResults TEXT)`,
		`CREATE TABLE sessions (sourcePath TEXT PRIMARY KEY, sessionId TEXT NOT NULL, parentSession TEXT, cwd TEXT, visibility TEXT)`,
		`CREATE VIRTUAL TABLE messages_fts USING fts5(content, prose, content=messages, content_rowid=rowid)`,
		`INSERT INTO sessions VALUES ('/old/s.jsonl', 's', NULL, '/work', 'main')`,
		`INSERT INTO messages VALUES ('call', 'call', '/old/s.jsonl', 's', 100, 'assistant', 'tool arguments', 'spoken', 14, '', '', '[{"ID":"x","Name":"Bash","Arguments":"{}"}]', '[]')`,
		`INSERT INTO messages VALUES ('result', 'result', '/old/s.jsonl', 's', 101, 'user', 'legacy output', '', 13, '', '', '[]', '[{"ID":"x","Name":"Bash","Content":"legacy output","IsError":false}]')`,
		`INSERT INTO messages_fts(rowid, content, prose) SELECT rowid, content, prose FROM messages`,
		`PRAGMA user_version = 7`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("fixture statement %q: %v", statement, err)
		}
	}
	var oldRowID int64
	if err := db.QueryRow(`SELECT rowid FROM messages WHERE id='call'`).Scan(&oldRowID); err != nil {
		t.Fatal(err)
	}
	if err := migrateReadProjections(db); err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	var rowID int64
	if err := db.QueryRow(`SELECT rowid FROM messages WHERE id='call'`).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	var contentPrefix, prosePrefix string
	var contentCount, proseCount int
	if err := db.QueryRow(`SELECT contentPrefix, prosePrefix, contentCharCount, proseCharCount FROM message_projections WHERE messageId='call'`).Scan(&contentPrefix, &prosePrefix, &contentCount, &proseCount); err != nil {
		t.Fatal(err)
	}
	var invocations, linkedResults, ftsHits, summaryCount int
	if err := db.QueryRow(`SELECT count(*) FROM tool_invocations WHERE sessionId='s' AND toolId='x'`).Scan(&invocations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM tool_results WHERE invocationId IN (SELECT invocationId FROM tool_invocations)`).Scan(&linkedResults); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'spoken'`).Scan(&ftsHits); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT messageCount FROM source_projections WHERE sourcePath='/old/s.jsonl'`).Scan(&summaryCount); err != nil {
		t.Fatal(err)
	}
	if version != 8 || rowID != oldRowID || contentPrefix != "tool arguments" || prosePrefix != "spoken" || contentCount != 14 || proseCount != 6 || invocations != 1 || linkedResults != 1 || ftsHits != 1 || summaryCount != 2 {
		t.Fatalf("migration: version=%d rowid=%d/%d prefixes=%q/%q counts=%d/%d invocations=%d linked=%d FTS=%d summary=%d", version, rowID, oldRowID, contentPrefix, prosePrefix, contentCount, proseCount, invocations, linkedResults, ftsHits, summaryCount)
	}
}

func BenchmarkReadModelQueries(b *testing.B) {
	root := b.TempDir()
	for s := 0; s < 250; s++ {
		var lines []string
		for m := 0; m < 40; m++ {
			text := "ordinary archived conversation"
			if m%10 == 0 {
				text += " broadneedle"
			}
			lines = append(lines, fmt.Sprintf(`{"type":"assistant","uuid":"m-%d-%d","sessionId":"s-%d","timestamp":%q,"message":{"content":%q}}`, s, m, s, time.Now().Add(time.Duration(m)*time.Second).UTC().Format(time.RFC3339Nano), text))
		}
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("s-%d.jsonl", s)), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
	db, err := Open(filepath.Join(b.TempDir(), "index.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		b.Fatal(err)
	}
	b.Run("search-broad", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := db.Search(SearchOptions{Query: "broadneedle", Limit: 20}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("search-common", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := db.Search(SearchOptions{Query: "ordinary", Limit: 20}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("search-window", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := db.Search(SearchOptions{Query: "broadneedle", WindowMessages: 1000, Limit: 20}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sessions", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := db.Sessions(SessionOptions{Limit: 100}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
