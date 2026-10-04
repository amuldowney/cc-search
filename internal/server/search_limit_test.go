package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amuldowney/cc-search/internal/output"
)

func TestSearchDefaultLimitAndExplicitUnlimited(t *testing.T) {
	root := t.TempDir()
	var lines strings.Builder
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&lines, `{"type":"user","uuid":"limit-%d","sessionId":"a","timestamp":"2026-08-24T00:00:00Z","message":{"content":"searchlimitneedle"}}`+"\n", i)
	}
	if err := os.WriteFile(filepath.Join(root, "a.jsonl"), []byte(lines.String()), 0600); err != nil {
		t.Fatal(err)
	}
	api, err := New(Config{IndexPath: filepath.Join(root, "index.db"), TranscriptDirs: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	for _, tc := range []struct {
		name, query string
		want        int
		truncated   bool
	}{
		{"default", "", 20, true},
		{"unlimited", "&limit=0", 25, false},
		{"explicit", "&limit=3", 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			api.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/search?pattern=searchlimitneedle"+tc.query, nil))
			var resp output.Response
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || len(resp.Results) != tc.want || resp.Truncated != tc.truncated {
				t.Fatalf("status=%d count=%d truncated=%v; want count=%d truncated=%v", w.Code, len(resp.Results), resp.Truncated, tc.want, tc.truncated)
			}
		})
	}
}
