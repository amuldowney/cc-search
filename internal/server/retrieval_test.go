package server

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAPIRetrievalControls(t *testing.T) {
	root := t.TempDir()
	for _, s := range []string{"a", "b"} {
		ts := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		body := fmt.Sprintf(`{"type":"session","id":"session-%s","cwd":"/work/%s","timestamp":%q}
{"type":"message","id":"one","timestamp":%q,"message":{"role":"user","content":"deploy snapshot"}}
{"type":"message","id":"two","timestamp":%q,"message":{"role":"assistant","content":"deploy decision"}}
{"type":"message","id":"call","timestamp":%q,"message":{"role":"assistant","content":[{"type":"toolCall","id":"tool-1","name":"bash","arguments":{"command":"deploy production"}},{"type":"toolCall","id":"tool-2","name":"bash","arguments":{"command":"echo unrelated"}}]}}
{"type":"message","id":"result","timestamp":%q,"message":{"role":"toolResult","toolCallId":"tool-1","toolName":"bash","content":[{"type":"text","text":"successmarker"}]}}
`, s, s, ts, ts, ts, ts, ts)
		if err := os.WriteFile(filepath.Join(root, s+".jsonl"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	api, err := New(Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{root}})
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close()
	t.Setenv("PI_SESSION_ID", "session-a")
	tests := []struct {
		url           string
		status, total int
	}{
		{"/v1/search?pattern=deploy&cwd=/work/a&include_current=true&per_session=1", 200, 1},
		{"/v1/search?pattern=deploy&cwd=/work&include_current=true", 200, 0},
		{"/v1/context?pattern=deploy&cwd=/work/a&include_current=true&per_session=1&before=0&after=0", 200, 1},
		{"/v1/last?limit=1&cwd=/work/a", 200, 1},
		{"/v1/last?limit=0&cwd=/work/a", 200, 2},
		{"/v1/commands?pattern=deploy&match=arguments", 200, 1},
		{"/v1/commands?pattern=deploy&match=arguments&include_current=true", 200, 2},
		{"/v1/commands?pattern=successmarker&match=output&session=session-a&hours=2", 200, 1},
		{"/v1/commands?pattern=successmarker&match=arguments", 200, 0},
		{"/v1/search?pattern=deploy&per_session=-1", 400, 0},
		{"/v1/search?pattern=deploy&reduce_noise=bogus", 400, 0},
		{"/v1/search?pattern=deploy&hours=1&window_hours=2", 400, 0},
		{"/v1/context?pattern=deploy&hours=-1", 400, 0},
		{"/v1/commands?pattern=deploy&match=bogus", 400, 0},
		{"/v1/commands?pattern=deploy&include_current=bogus", 400, 0},
		{"/v1/last?count=1&limit=2", 400, 0},
	}
	for _, tc := range tests {
		t.Run(tc.url, func(t *testing.T) {
			w := httptest.NewRecorder()
			api.Handler().ServeHTTP(w, httptest.NewRequest("GET", tc.url, nil))
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.status, w.Body.String())
			}
			if tc.status == 200 {
				var v struct {
					Total int `json:"total"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
					t.Fatal(err)
				}
				if v.Total != tc.total {
					t.Fatalf("total=%d want=%d body=%s", v.Total, tc.total, w.Body.String())
				}
			}
		})
	}
}
