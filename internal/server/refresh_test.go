package server

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/amuldowney/cc-search/internal/index"
	"github.com/amuldowney/cc-search/internal/output"
)

func refreshFixture(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "session-a.jsonl")
	line := `{"type":"user","uuid":"a","sessionId":"a","timestamp":"2026-08-24T00:00:00Z","message":{"content":"oldneedle"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	api, err := New(Config{IndexPath: filepath.Join(root, "index.db"), TranscriptDirs: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := api.Close(); err != nil {
			t.Error(err)
		}
	})
	return api, path
}

func TestAPIExplicitRefreshAndSnapshot(t *testing.T) {
	for _, mode := range []string{"query", "endpoint"} {
		t.Run(mode, func(t *testing.T) {
			api, path := refreshFixture(t)
			line := `{"type":"user","uuid":"new","sessionId":"a","timestamp":"2026-08-25T00:00:00Z","message":{"content":"newneedle"}}` + "\n"
			if err := os.WriteFile(path, []byte(line), 0600); err != nil {
				t.Fatal(err)
			}
			query := func(suffix string) output.Response {
				w := httptest.NewRecorder()
				api.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/search?pattern=newneedle"+suffix, nil))
				if w.Code != 200 {
					t.Fatalf("status %d: %s", w.Code, w.Body.String())
				}
				var result output.Response
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				return result
			}
			if result := query(""); result.Total != 0 {
				t.Fatal("normal query synchronously refreshed transcript")
			}
			suffix := "&refresh=true"
			if mode == "endpoint" {
				w := httptest.NewRecorder()
				api.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/v1/refresh", nil))
				if w.Code != 200 {
					t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
				}
				suffix = ""
			}
			if result := query(suffix); result.Total != 1 {
				t.Fatal("explicit refresh did not publish new transcript")
			}
			w := httptest.NewRecorder()
			api.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/info", nil))
			var info output.InfoResponse
			if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
				t.Fatal(err)
			}
			if info.Freshness == nil || info.Freshness.LastSuccess == 0 || info.Freshness.Stale {
				t.Fatalf("missing fresh snapshot status: %#v", info.Freshness)
			}
		})
	}
}

func TestExplicitRefreshWaitsWithoutBlockingSnapshotRequests(t *testing.T) {
	api, _ := refreshFixture(t)
	held, err := index.Open(api.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/last?refresh=true", nil))
		done <- w
	}()
	select {
	case w := <-done:
		t.Fatalf("explicit refresh did not wait: %d", w.Code)
	case <-time.After(100 * time.Millisecond):
	}
	readDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/last", nil))
		readDone <- w
	}()
	select {
	case w := <-readDone:
		if w.Code != 200 {
			t.Fatalf("snapshot: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(time.Second):
		held.ReleaseLifecycleLock()
		<-readDone
		<-done
		t.Fatal("snapshot waited behind forced refresh")
	}
	if err := held.ReleaseLifecycleLock(); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish after writer release")
	}
}

func TestRefreshRejectsInvalidInputsAndClosedServer(t *testing.T) {
	api, _ := refreshFixture(t)
	for _, tc := range []struct {
		method, path string
		want         int
	}{{"GET", "/v1/last?refresh=bogus", 400}, {"GET", "/v1/refresh", 405}} {
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != tc.want {
			t.Fatalf("%s: %d, want %d", tc.path, w.Code, tc.want)
		}
	}
	api.Close()
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/last", nil))
	if w.Code != 503 {
		t.Fatalf("closed status=%d", w.Code)
	}
}

func TestAPIDoctorUsesSnapshotUnlessRefreshRequested(t *testing.T) {
	api, path := refreshFixture(t)
	line := `{"type":"user","uuid":"fresh","sessionId":"a","timestamp":"2026-08-25T00:00:00Z","message":{"content":"doctornewneedle"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		url := "/v1/doctor"
		if force {
			url += "?refresh=true"
		}
		w := httptest.NewRecorder()
		api.Handler().ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		if w.Code != 200 {
			t.Fatalf("doctor: %d %s", w.Code, w.Body.String())
		}
		w = httptest.NewRecorder()
		api.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/search?pattern=doctornewneedle", nil))
		var resp output.Response
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		want := 0
		if force {
			want = 1
		}
		if resp.Total != want {
			t.Fatalf("doctor force=%v returned%d want%d", force, resp.Total, want)
		}
	}
}

func TestAPIRefreshWorkerKeepsInFlightSnapshotAndFinishesOnClose(t *testing.T) {
	api, path := refreshFixture(t)
	line := `{"type":"user","uuid":"new","sessionId":"a","timestamp":"2026-08-25T00:00:00Z","message":{"content":"backgroundneedle"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", api.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE refresh_state SET last_success=1,last_attempt=1`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	w := httptest.NewRecorder()
	api.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/search?pattern=backgroundneedle", nil))
	var response output.Response
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || response.Total != 0 {
		t.Fatalf("in-flight response changed snapshot: %d %s", w.Code, w.Body.String())
	}
	if err := api.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := index.OpenReader(api.indexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	hits, err := reader.Search(index.SearchOptions{Query: "backgroundneedle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatal("owned background refresh did not finish before Close returned")
	}
}
