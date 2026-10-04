package cli

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/amuldowney/cc-search/internal/index"
)

func TestQueriesUseSnapshotUntilExplicitRefresh(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "oldneedle", 1}})
	if _, stderr, code := run(t, cfg, "search", "oldneedle"); code != 0 {
		t.Fatal(stderr)
	}
	line := fmt.Sprintf(`{"type":"user","uuid":"new","sessionId":"session-a","timestamp":%q,"message":{"content":"newneedle"}}`+"\n", time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(filepath.Join(cfg.TranscriptDirs[0], "session-a.jsonl"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	resp, stderr, code := run(t, cfg, "search", "newneedle")
	if code != 0 || len(resp.Results) != 0 {
		t.Fatalf("snapshot lookup: code=%d results=%d stderr=%s; want old snapshot", code, len(resp.Results), stderr)
	}
	resp, stderr, code = run(t, cfg, "search", "newneedle", "--refresh")
	if code != 0 || len(resp.Results) != 1 {
		t.Fatalf("explicit refresh: code=%d results=%d stderr=%s", code, len(resp.Results), stderr)
	}
}

func TestCLIQueriesDoNotWaitForLifecycleLock(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "snapshotneedle", 1}})
	if _, stderr, code := run(t, cfg, "search", "snapshotneedle"); code != 0 {
		t.Fatal(stderr)
	}
	held, err := index.Open(cfg.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	done := make(chan error, 1)
	go func() {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"search", "snapshotneedle"}, cfg, &stdout, &stderr)
		if code != 0 {
			done <- fmt.Errorf("code=%d stderr=%s", code, stderr.String())
		} else {
			done <- nil
		}
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		held.ReleaseLifecycleLock()
		<-done
		t.Fatal("snapshot query waited for the writer lifecycle lock")
	}
}

func TestDoctorUsesSnapshotUnlessRefreshRequested(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "oldneedle", 1}})
	if _, stderr, code := run(t, cfg, "search", "oldneedle"); code != 0 {
		t.Fatal(stderr)
	}
	line := `{"type":"user","uuid":"fresh","sessionId":"session-a","timestamp":"2026-08-25T00:00:00Z","message":{"content":"doctornewneedle"}}` + "\n"
	if err := os.WriteFile(filepath.Join(cfg.TranscriptDirs[0], "session-a.jsonl"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		args := []string{"doctor"}
		if force {
			args = append(args, "--refresh")
		}
		var stdout, stderr bytes.Buffer
		if code := Run(args, cfg, &stdout, &stderr); code != 0 {
			t.Fatalf("doctor: %d %s", code, stderr.String())
		}
		resp, stderrText, code := run(t, cfg, "search", "doctornewneedle")
		want := 0
		if force {
			want = 1
		}
		if code != 0 || resp.Total != want {
			t.Fatalf("doctor force=%v results=%d want%d error%s", force, resp.Total, want, stderrText)
		}
	}
}

func TestCLIRequestsBackgroundRefreshForStaleSnapshot(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "needle", 1}})
	if _, stderr, code := run(t, cfg, "search", "needle"); code != 0 {
		t.Fatal(stderr)
	}
	db, err := sql.Open("sqlite3", cfg.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE refresh_state SET last_success=1,last_attempt=1`); err != nil {
		t.Fatal(err)
	}
	calls := 0
	cfg.StartRefresh = func(path string, roots []string) error {
		calls++
		if path != cfg.IndexPath || !reflect.DeepEqual(roots, cfg.TranscriptDirs) {
			t.Fatalf("scheduler lost overrides: %s %v", path, roots)
		}
		return nil
	}
	if resp, stderr, code := run(t, cfg, "search", "needle"); code != 0 || resp.Total != 1 {
		t.Fatalf("snapshot response: %d %s", code, stderr)
	}
	if calls != 1 {
		t.Fatalf("stale snapshot scheduled %d refreshes", calls)
	}
	if _, err := db.Exec(`UPDATE refresh_state SET last_attempt=?`, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
	if _, stderr, code := run(t, cfg, "search", "needle"); code != 0 {
		t.Fatal(stderr)
	}
	if calls != 1 {
		t.Fatal("recent failed/in-flight attempt was not throttled")
	}
}
