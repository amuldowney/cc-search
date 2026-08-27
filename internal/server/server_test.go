package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewmuldowney/cc-search/internal/index"
)

func TestHandlerServesSearchAndOpenAPI(t *testing.T) {
	root := t.TempDir()
	transcripts := filepath.Join(root, "transcripts")
	if err := os.MkdirAll(transcripts, 0o755); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","uuid":"msg-auth","sessionId":"session-a","timestamp":"2026-08-24T00:00:00Z","message":{"role":"user","content":"the auth flow decision"}}` + "\n"
	if err := os.WriteFile(filepath.Join(transcripts, "session-a.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	api, err := New(Config{
		IndexPath:      filepath.Join(root, "index.db"),
		TranscriptDirs: []string{transcripts},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()

	server := httptest.NewServer(api.Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/v1/search?pattern=auth&full=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("search status = %d, want 200", response.StatusCode)
	}
	var search map[string]any
	if err := json.NewDecoder(response.Body).Decode(&search); err != nil {
		t.Fatal(err)
	}
	results, ok := search["results"].([]any)
	if !ok || len(results) != 1 {
		t.Fatalf("results = %#v, want one result", search["results"])
	}
	result := results[0].(map[string]any)
	if result["id"] != "msg-auth" || result["content"] != "the auth flow decision" {
		t.Fatalf("result = %#v", result)
	}

	openapi, err := http.Get(server.URL + "/openapi.json")
	if err != nil {
		t.Fatal(err)
	}
	defer openapi.Body.Close()
	if openapi.StatusCode != http.StatusOK {
		t.Fatalf("openapi status = %d, want 200", openapi.StatusCode)
	}
	var document struct {
		OpenAPI string                     `json:"openapi"`
		Paths   map[string]json.RawMessage `json:"paths"`
	}
	if err := json.NewDecoder(openapi.Body).Decode(&document); err != nil {
		t.Fatal(err)
	}
	if document.OpenAPI != "3.1.0" {
		t.Fatalf("openapi version = %q, want 3.1.0", document.OpenAPI)
	}
	if _, ok := document.Paths["/v1/search"]; !ok {
		t.Fatalf("OpenAPI paths omit /v1/search: %v", document.Paths)
	}
}

func TestWithDBWaitsForLifecycleLock(t *testing.T) {
	root := t.TempDir()
	indexPath := filepath.Join(root, "index.db")
	api, err := New(Config{IndexPath: indexPath})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := api.Close(); err != nil {
			t.Errorf("close server: %v", err)
		}
	})

	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()

	held, err := index.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := held.Close(); err != nil {
			t.Errorf("close lock holder: %v", err)
		}
	})

	requestDone := make(chan error, 1)
	go func() {
		response, err := http.Get(httpServer.URL + "/v1/last")
		if err != nil {
			requestDone <- err
			return
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			requestDone <- fmt.Errorf("last status = %d, want 200", response.StatusCode)
			return
		}
		requestDone <- nil
	}()

	select {
	case err := <-requestDone:
		t.Fatalf("request completed while lifecycle lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := held.ReleaseLifecycleLock(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not complete after lifecycle lock was released")
	}
}

func TestLastHoursDoesNotApplyDefaultCount(t *testing.T) {
	root := t.TempDir()
	transcripts := filepath.Join(root, "transcripts")
	if err := os.MkdirAll(transcripts, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	line := fmt.Sprintf(
		`{"type":"user","uuid":"msg-recent","sessionId":"session-a","timestamp":%q,"message":{"role":"user","content":"recent message"}}
{"type":"user","uuid":"msg-old","sessionId":"session-a","timestamp":%q,"message":{"role":"user","content":"old message"}}
`,
		now.Format(time.RFC3339), now.Add(-2*time.Hour).Format(time.RFC3339),
	)
	if err := os.WriteFile(filepath.Join(transcripts, "session-a.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	api, err := New(Config{IndexPath: filepath.Join(root, "index.db"), TranscriptDirs: []string{transcripts}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()

	httpServer := httptest.NewServer(api.Handler())
	defer httpServer.Close()
	response, err := http.Get(httpServer.URL + "/v1/last?hours=1&full=true")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("last status = %d, want 200", response.StatusCode)
	}
	var body struct {
		Results []map[string]any `json:"results"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Results) != 1 || body.Results[0]["id"] != "msg-recent" {
		t.Fatalf("results = %#v, want only recent message", body.Results)
	}
}
