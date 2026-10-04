package index

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenMigratesV7WithoutDiscardingIndexedMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	legacy, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(strings.TrimSuffix(schema, projectionSchema)); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO sessions(sourcePath,sessionId) VALUES('/absent/source.jsonl','legacy'),('/absent/empty.jsonl','empty');
	INSERT INTO messages(rowid,id,entryId,sourcePath,sessionId,timestamp,type,content,prose,charCount,activityId,activityRole,toolCalls,toolResults)
	VALUES(77,'kept','kept','/absent/source.jsonl','legacy',123,'user','migrationneedle','migrationneedle',15,'','','[]','[]');
	PRAGMA user_version=7;`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	messages, err := db.Search(SearchOptions{Query: "migrationneedle", PreviewLength: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].ID != "kept" {
		t.Fatalf("v7 migration discarded indexed content: %#v", messages)
	}
	var rowid int64
	if err := db.sql.QueryRow(`SELECT rowid FROM messages WHERE id='kept'`).Scan(&rowid); err != nil {
		t.Fatal(err)
	}
	if rowid != 77 {
		t.Fatalf("migration changed FTS message rowid: %d", rowid)
	}
	sessions, err := db.Sessions(SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("migration dropped header-only session: %#v", sessions)
	}
}
