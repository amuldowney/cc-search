package index

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amuldowney/cc-search/internal/transcript"
)

func TestOpenMigratesV7WithoutDiscardingIndexedMessages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v7.db")
	legacy, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	legacySchema := strings.TrimSuffix(strings.TrimSuffix(schema, commandSearchSchema), projectionSchema)
	if _, err := legacy.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO sessions(sourcePath,sessionId) VALUES('/absent/source.jsonl','legacy'),('/absent/empty.jsonl','empty');
	INSERT INTO messages(rowid,id,entryId,sourcePath,sessionId,timestamp,type,content,prose,charCount,activityId,activityRole,toolCalls,toolResults)
	VALUES(77,'kept','kept','/absent/source.jsonl','legacy',123,'user','migrationneedle','migrationneedle',15,'','','[]','[]');
	PRAGMA user_version=7;`); err != nil {
		t.Fatal(err)
	}
	calls, err := json.Marshal([]transcript.ToolCall{{ID: "old-call", Name: "Bash", Arguments: `{"command":"fromv7command"}`}})
	if err != nil {
		t.Fatal(err)
	}
	results, err := json.Marshal([]transcript.ToolResult{{ID: "old-call", Name: "Bash", Content: "v7outputtoken"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`UPDATE messages SET type='assistant', toolCalls=? WHERE id='kept'`, string(calls)); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO messages(rowid,id,entryId,sourcePath,sessionId,timestamp,type,content,prose,charCount,activityId,activityRole,toolCalls,toolResults)
		VALUES(78,'result','result','/absent/source.jsonl','legacy',124,'user','v7outputtoken','',13,'','','[]',?)`, string(results)); err != nil {
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
	commands, err := db.Commands(CommandOptions{Query: "fromv7command v7outputtoken", IncludeOutput: true})
	if err != nil || len(commands) != 1 || commands[0].MessageID != "kept" || commands[0].Output != "v7outputtoken" {
		t.Fatalf("v7-to-v9 command migration = %+v, err=%v", commands, err)
	}
	var version int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 9 {
		t.Fatalf("v7 migration version = %d, err=%v; want 9", version, err)
	}
	sessions, err := db.Sessions(SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("migration dropped header-only session: %#v", sessions)
	}
}
