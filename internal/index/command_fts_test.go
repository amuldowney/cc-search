package index

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandsMatchInvocationsInsteadOfWholeMessages(t *testing.T) {
	root := t.TempDir()
	writeRawTranscript(t, root, "session", []string{
		`{"type":"assistant","uuid":"calls","sessionId":"session","timestamp":"2026-08-25T00:00:01Z","message":{"content":[{"type":"text","text":"needle appears in surrounding prose"},{"type":"tool_use","id":"matching","name":"Bash","input":{"command":"find needle"}},{"type":"tool_use","id":"sibling","name":"Edit","input":{"file_path":"unrelated.txt"}}]}}`,
	})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}

	got, err := db.Commands(CommandOptions{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Tool != "Bash" {
		t.Fatalf("command search returned whole-message siblings/prose: %+v; want only the Bash invocation", got)
	}
	proseOnly, err := db.Commands(CommandOptions{Query: "surrounding"})
	if err != nil || len(proseOnly) != 0 {
		t.Fatalf("surrounding prose matched a command: %+v, err=%v", proseOnly, err)
	}
}

func TestCommandsMatchModesUseOnlyPairedInvocationColumns(t *testing.T) {
	root := t.TempDir()
	writeRawTranscript(t, root, "scope", []string{
		`{"type":"assistant","uuid":"calls","sessionId":"scope","timestamp":"2026-08-25T00:00:01Z","message":{"content":[{"type":"tool_use","id":"one","name":"Bash","input":{"command":"amber deployed display.cpp"}},{"type":"tool_use","id":"two","name":"Edit","input":{"file_path":"amber sibling"}},{"type":"tool_use","id":"three","name":"Read","input":{"file_path":"unrelated"}}]}}`,
		`{"type":"user","uuid":"result-one","sessionId":"scope","timestamp":"2026-08-25T00:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"one","content":"violet paired output"}]}}`,
		`{"type":"user","uuid":"result-two","sessionId":"scope","timestamp":"2026-08-25T00:00:03Z","message":{"content":[{"type":"tool_result","tool_use_id":"two","content":"indigo sibling output"}]}}`,
		`{"type":"user","uuid":"result-three","sessionId":"scope","timestamp":"2026-08-25T00:00:04Z","message":{"content":[{"type":"tool_result","tool_use_id":"three","content":"violet independent output"}]}}`,
	})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}

	both, err := db.Commands(CommandOptions{Query: "amber violet", IncludeOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 1 || both[0].Tool != "Bash" || both[0].Output != "violet paired output" {
		t.Fatalf("both-column same-invocation match = %+v, want only the paired Bash call", both)
	}
	for _, query := range []struct {
		match     string
		term      string
		wantTools []string
	}{
		{match: "arguments", term: "violet", wantTools: nil},
		{match: "output", term: "amber", wantTools: nil},
		{match: "arguments", term: "deploy", wantTools: []string{"Bash"}},
		{match: "arguments", term: "display.cpp", wantTools: []string{"Bash"}},
		{match: "arguments", term: "Edit", wantTools: []string{"Edit"}},
		{match: "output", term: "violet", wantTools: []string{"Bash", "Read"}},
	} {
		got, err := db.Commands(CommandOptions{Query: query.term, Match: query.match})
		if err != nil {
			t.Fatalf("Match=%q Query=%q: %v", query.match, query.term, err)
		}
		if len(got) != len(query.wantTools) {
			t.Fatalf("Match=%q Query=%q returned %+v, want %d results", query.match, query.term, got, len(query.wantTools))
		}
		for i, wantTool := range query.wantTools {
			if got[i].Tool != wantTool {
				t.Fatalf("Match=%q Query=%q result[%d]=%+v, want tool %q", query.match, query.term, i, got[i], wantTool)
			}
		}
	}
	if _, err := db.Commands(CommandOptions{Match: "messages"}); err == nil {
		t.Fatal("invalid command match mode was accepted")
	}
	defaultMode, err := db.Commands(CommandOptions{Query: "amber violet"})
	if err != nil || len(defaultMode) != 1 || defaultMode[0].Tool != "Bash" {
		t.Fatalf("empty Match default = %+v, err=%v; want both-column behavior", defaultMode, err)
	}
}

func TestCommandsApplyCWDHoursAndSessionExclusion(t *testing.T) {
	root := t.TempDir()
	now := time.Now().UTC()
	for _, session := range []struct {
		id     string
		cwd    string
		age    time.Duration
		callID string
	}{
		{id: "a", cwd: "/work/one", age: 10 * time.Minute, callID: "a-call"},
		{id: "b", cwd: "/work/two", age: 20 * time.Minute, callID: "b-call"},
		{id: "c", cwd: "/work/one", age: 24 * time.Hour, callID: "c-call"},
	} {
		line := fmt.Sprintf(`{"type":"assistant","uuid":%q,"sessionId":%q,"cwd":%q,"timestamp":%q,"message":{"content":[{"type":"tool_use","id":%q,"name":"Bash","input":{"command":%q}}]}}`,
			session.callID, session.id, session.cwd, now.Add(-session.age).Format(time.RFC3339Nano), session.callID, "needle "+session.id)
		writeRawTranscript(t, root, session.id, []string{line})
	}
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}

	got, err := db.Commands(CommandOptions{CWD: "/work/one"})
	if err != nil || len(got) != 2 || got[0].SessionID != "a" || got[1].SessionID != "c" {
		t.Fatalf("exact CWD filter/order = %+v, err=%v", got, err)
	}
	wrongCWD, err := db.Commands(CommandOptions{CWD: "/work"})
	if err != nil || len(wrongCWD) != 0 {
		t.Fatalf("non-exact CWD match = %+v, err=%v", wrongCWD, err)
	}
	recent, err := db.Commands(CommandOptions{Hours: 2})
	if err != nil || len(recent) != 2 || recent[0].SessionID != "a" || recent[1].SessionID != "b" {
		t.Fatalf("Hours filter = %+v, err=%v", recent, err)
	}
	excluded, err := db.Commands(CommandOptions{ExcludeSessionID: "a"})
	if err != nil || len(excluded) != 2 || excluded[0].SessionID != "b" || excluded[1].SessionID != "c" {
		t.Fatalf("session exclusion = %+v, err=%v", excluded, err)
	}
	override, err := db.Commands(CommandOptions{SessionID: "a", ExcludeSessionID: "a"})
	if err != nil || len(override) != 1 || override[0].SessionID != "a" {
		t.Fatalf("explicit SessionID did not override exclusion: %+v, err=%v", override, err)
	}
	limited, err := db.Commands(CommandOptions{Limit: 1})
	if err != nil || len(limited) != 1 || limited[0].SessionID != "a" {
		t.Fatalf("finite newest-first limit = %+v, err=%v", limited, err)
	}
	unlimited, err := db.Commands(CommandOptions{})
	if err != nil || len(unlimited) != 3 {
		t.Fatalf("unlimited commands = %+v, err=%v", unlimited, err)
	}
}

func TestCommandSearchIndexTracksRelinkingAndDeletes(t *testing.T) {
	root := t.TempDir()
	writeRawTranscript(t, root, "calls", []string{
		`{"type":"assistant","uuid":"call","sessionId":"same-session","timestamp":"2026-08-25T00:00:01Z","message":{"content":[{"type":"tool_use","id":"tool-id","name":"Bash","input":{"command":"keep-call"}}]}}`,
	})
	writeRawTranscript(t, root, "results", []string{
		`{"type":"user","uuid":"result","sessionId":"same-session","timestamp":"2026-08-25T00:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"tool-id","content":"ephemeral-result"}]}}`,
	})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}
	matched, err := db.Commands(CommandOptions{Query: "ephemeral-result", Match: "output", IncludeOutput: true})
	if err != nil || len(matched) != 1 || matched[0].Output != "ephemeral-result" {
		t.Fatalf("paired output before replacement = %+v, err=%v", matched, err)
	}

	writeRawTranscript(t, root, "results", nil)
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}
	stale, err := db.Commands(CommandOptions{Query: "ephemeral-result", Match: "output"})
	if err != nil || len(stale) != 0 {
		t.Fatalf("deleted result remains searchable = %+v, err=%v", stale, err)
	}
	call, err := db.Commands(CommandOptions{Query: "keep-call", Match: "arguments", IncludeOutput: true})
	if err != nil || len(call) != 1 || call[0].Output != "" {
		t.Fatalf("call after paired-result removal = %+v, err=%v", call, err)
	}

	writeRawTranscript(t, root, "calls", nil)
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}
	stale, err = db.Commands(CommandOptions{Query: "keep-call", Match: "arguments"})
	if err != nil || len(stale) != 0 {
		t.Fatalf("deleted invocation remains searchable = %+v, err=%v", stale, err)
	}
}

func TestCommandSearchIndexTracksResultRelinking(t *testing.T) {
	root := t.TempDir()
	writeRawTranscript(t, root, "first", []string{
		`{"type":"assistant","uuid":"first-call","sessionId":"legacy-pair","timestamp":"2026-08-25T00:00:01Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"first legacy command"}}]}}`,
	})
	writeRawTranscript(t, root, "results", []string{
		`{"type":"user","uuid":"result","sessionId":"legacy-pair","timestamp":"2026-08-25T00:00:03Z","message":{"content":[{"type":"tool_result","name":"Bash","content":"relocatable output"}]}}`,
	})
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}
	initial, err := db.Commands(CommandOptions{Query: "relocatable", Match: "output"})
	if err != nil || len(initial) != 1 || initial[0].MessageID != "legacy-pair:first-call" {
		t.Fatalf("initial legacy result pair = %+v, err=%v", initial, err)
	}

	writeRawTranscript(t, root, "later-call", []string{
		`{"type":"assistant","uuid":"second-call","sessionId":"legacy-pair","timestamp":"2026-08-25T00:00:02Z","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"second legacy command"}}]}}`,
	})
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}
	relinked, err := db.Commands(CommandOptions{Query: "relocatable", Match: "output", IncludeOutput: true})
	if err != nil || len(relinked) != 1 || relinked[0].MessageID != "legacy-pair:second-call" || relinked[0].Output != "relocatable output" {
		t.Fatalf("relinked result search = %+v, err=%v", relinked, err)
	}
}

func TestOpenMigratesV8CommandIndexAdditively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v8.db")
	legacy, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	v8Schema := strings.TrimSuffix(schema, commandSearchSchema)
	if _, err := legacy.Exec(v8Schema); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE refresh_state(profile TEXT PRIMARY KEY, roots TEXT NOT NULL, last_success INTEGER NOT NULL DEFAULT 0, last_attempt INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '')`,
		`INSERT INTO sessions(sourcePath,sessionId,cwd) VALUES('/old/session.jsonl','old-session','/old/work')`,
		`INSERT INTO messages(rowid,id,entryId,sourcePath,sessionId,timestamp,type,content,prose,charCount,activityId,activityRole,toolCalls,toolResults)
		 VALUES(77,'call','call','/old/session.jsonl','old-session',123,'assistant','deployed arguments','',18,'','','[]','[]')`,
		`INSERT INTO tool_invocations(invocationId,messageId,sessionId,toolId,toolName,arguments,timestamp)
		 VALUES(91,'call','old-session','tool-id','Bash','{"command":"deployed arguments"}',123)`,
		`INSERT INTO tool_results(resultId,messageId,sessionId,toolId,toolName,content,isError,timestamp,invocationId)
		 VALUES(92,'result','old-session','tool-id','Bash','paired legacy output',0,124,91)`,
		`INSERT INTO refresh_state(profile,roots,last_success,last_attempt,error) VALUES('profile','[]',111,112,'')`,
		`INSERT INTO message_projections(messageId,contentPrefix,prosePrefix,contentCharCount,proseCharCount)
		 VALUES('call','deployed arguments','',18,0)`,
		`INSERT INTO source_projections(sourcePath,sessionId,messageCount,latestTimestamp,latestMessageId,latestContentPrefix,latestProsePrefix)
		 VALUES('/old/session.jsonl','old-session',1,123,'call','deployed arguments','')`,
		`PRAGMA user_version=8`,
		`PRAGMA journal_mode=WAL`,
	} {
		if _, err := legacy.Exec(statement); err != nil {
			t.Fatalf("v8 fixture statement %q: %v", statement, err)
		}
	}
	var oldFTSHits int
	if err := legacy.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'deployed'`).Scan(&oldFTSHits); err != nil {
		t.Fatal(err)
	}
	if oldFTSHits != 1 {
		t.Fatalf("v8 fixture FTS hits = %d, want 1", oldFTSHits)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	snapshotDSN, err := readOnlyDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshotDB, err := sql.Open("sqlite3", snapshotDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshotDB.Close()
	snapshot, err := snapshotDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Rollback()
	var snapshotRowID int64
	if err := snapshot.QueryRow(`SELECT rowid FROM messages WHERE id='call'`).Scan(&snapshotRowID); err != nil {
		t.Fatal(err)
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	var rowID int64
	if err := db.sql.QueryRow(`SELECT rowid FROM messages WHERE id='call'`).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	var refreshCount, ftsHits int
	if err := db.sql.QueryRow(`SELECT count(*) FROM refresh_state WHERE profile='profile' AND last_success=111`).Scan(&refreshCount); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'deployed'`).Scan(&ftsHits); err != nil {
		t.Fatal(err)
	}
	commands, err := db.Commands(CommandOptions{Query: "deploy output", IncludeOutput: true})
	if err != nil {
		t.Fatal(err)
	}
	var snapshotFTSHits int
	if err := snapshot.QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'deployed'`).Scan(&snapshotFTSHits); err != nil {
		t.Fatal(err)
	}
	if version != 9 || rowID != 77 || refreshCount != 1 || ftsHits != oldFTSHits || len(commands) != 1 || commands[0].Output != "paired legacy output" || snapshotRowID != 77 || snapshotFTSHits != oldFTSHits {
		t.Fatalf("v8 migration version=%d rowid=%d refresh=%d FTS=%d/%d commands=%+v snapshot=%d FTS=%d", version, rowID, refreshCount, ftsHits, oldFTSHits, commands, snapshotRowID, snapshotFTSHits)
	}
}

func TestCommandSearchMigrationRollsBackOnSchemaConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(strings.TrimSuffix(schema, commandSearchSchema)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE command_search_content(invocationId INTEGER PRIMARY KEY, arguments TEXT NOT NULL); PRAGMA user_version=8`); err != nil {
		t.Fatal(err)
	}
	if err := migrateCommandSearch(db); err == nil {
		t.Fatal("migration succeeded despite the conflicting read-model table")
	}
	var version, commandFTSTables int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='command_fts'`).Scan(&commandFTSTables); err != nil {
		t.Fatal(err)
	}
	if version != 8 || commandFTSTables != 0 {
		t.Fatalf("failed migration left version=%d command FTS objects=%d", version, commandFTSTables)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}
