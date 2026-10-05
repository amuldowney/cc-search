package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amuldowney/cc-search/internal/output"
)

func retrievalFixture(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	for _, s := range []string{"a", "b"} {
		ts := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
		body := fmt.Sprintf(`{"type":"session","id":"session-%s","cwd":"/work/%s","timestamp":%q}
{"type":"message","id":"message-long-id","timestamp":%q,"message":{"role":"user","content":"deployment decision: preserve snapshots"}}
{"type":"message","id":"call","timestamp":%q,"message":{"role":"assistant","content":[{"type":"toolCall","id":"tool-1","name":"bash","arguments":{"command":"deploy production"}},{"type":"toolCall","id":"tool-2","name":"bash","arguments":{"command":"echo unrelated"}}]}}
{"type":"message","id":"result","timestamp":%q,"message":{"role":"toolResult","toolCallId":"tool-1","toolName":"bash","content":[{"type":"text","text":"successmarker"}]}}
`, s, s, ts, ts, ts, ts)
		if err := os.WriteFile(filepath.Join(root, s+".jsonl"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{root}}
}

func TestRetrievalAliasesAndProjectFilters(t *testing.T) {
	cfg := retrievalFixture(t)
	for _, args := range [][]string{
		{"last", "--limit", "1", "--cwd", "/work/a", "--hours", "2"},
		{"search", "deployment", "--hours", "2", "--cwd", "/work/a"},
		{"context", "deployment", "--hours", "2", "--cwd", "/work/a", "--before", "0", "--after", "0"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			got, stderr, code := run(t, cfg, args...)
			if code != 0 || got.Total != 1 || got.Results[0].SessionID != "session-a" {
				t.Fatalf("code=%d stderr=%s result=%+v", code, stderr, got)
			}
		})
	}
	for _, args := range [][]string{{"search", "deployment", "--cwd", "/work"}, {"last", "--cwd", "/work"}} {
		got, stderr, code := run(t, cfg, args...)
		if code != 0 || got.Total != 0 {
			t.Fatalf("cwd must be exact: %v code=%d stderr=%s result=%+v", args, code, stderr, got)
		}
	}
}

func TestRetrievalTextKeepsFullIDAndJSONDefault(t *testing.T) {
	cfg := retrievalFixture(t)
	var out, errout bytes.Buffer
	args := []string{"search", "deployment", "--cwd", "/work/a", "--format", "text"}
	if code := Run(args, cfg, &out, &errout); code != 0 {
		t.Fatalf("%d: %s", code, errout.String())
	}
	for _, want := range []string{"session-a:message-long-id", "preserve snapshots"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	if json.Valid(out.Bytes()) {
		t.Fatal("text output is JSON")
	}
	got, stderr, code := run(t, cfg, "search", "deployment", "--format", "json")
	if code != 0 || got.Total != 2 {
		t.Fatalf("json: %d %s %+v", code, stderr, got)
	}
}

func TestRetrievalCommandModesAndCurrentSession(t *testing.T) {
	cfg := retrievalFixture(t)
	t.Setenv("PI_SESSION_ID", "session-a")
	tests := []struct {
		args []string
		want int
	}{
		{[]string{"commands", "deploy", "--match", "arguments"}, 1},
		{[]string{"commands", "deploy", "--match", "arguments", "--include-current"}, 2},
		{[]string{"commands", "deploy", "--match", "arguments", "--session", "session-a"}, 1},
		{[]string{"commands", "successmarker", "--match", "output", "--cwd", "/work/b", "--hours", "2"}, 1},
		{[]string{"commands", "successmarker", "--match", "arguments"}, 0},
	}
	for _, tc := range tests {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			var out, errout bytes.Buffer
			if code := Run(tc.args, cfg, &out, &errout); code != 0 {
				t.Fatalf("%d: %s", code, errout.String())
			}
			var got output.CommandsResponse
			decodeValue(t, &out, &got)
			if got.Total != tc.want {
				t.Fatalf("got %+v want %d", got, tc.want)
			}
			for _, c := range got.Commands {
				if strings.Contains(fmt.Sprint(c.Arguments), "unrelated") {
					t.Fatalf("sibling leaked: %+v", c)
				}
			}
		})
	}
}

func TestRetrievalRejectsInvalidFlagsBeforeOpeningIndex(t *testing.T) {
	for _, args := range [][]string{
		{"last", "2", "--limit", "3"}, {"last", "--limit", "-1"},
		{"search", "deploy", "--hours", "1", "--window-hours", "2"},
		{"search", "deploy", "--per-session", "-1"}, {"search", "deploy", "--limit", "-1"},
		{"context", "deploy", "--window-messages", "-1"}, {"commands", "deploy", "--match", "bogus"},
		{"last", "--format", "bogus"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "must-not-open.db")
			var out, errout bytes.Buffer
			if code := Run(args, Config{IndexPath: path, TranscriptDirs: []string{t.TempDir()}}, &out, &errout); code != 2 {
				t.Fatalf("code=%d stderr=%s", code, errout.String())
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("invalid flags opened index: %v", err)
			}
		})
	}
}

func TestLastExplicitZeroLimitIsUnlimited(t *testing.T) {
	msgs := make([]msg, 12)
	for i := range msgs {
		msgs[i] = msg{"user", "deploy", i}
	}
	cfg := fixture(t, msgs)
	got, stderr, code := run(t, cfg, "last", "--limit", "0")
	if code != 0 || got.Total != 12 {
		t.Fatalf("code=%d stderr=%s total=%d", code, stderr, got.Total)
	}
}

func TestRetrievalDiversityFlags(t *testing.T) {
	cfg := retrievalFixture(t)
	got, stderr, code := run(t, cfg, "search", "deployment", "--per-session", "1", "--reduce-noise", "--limit", "2")
	if code != 0 || got.Total != 2 || got.Results[0].SessionID == got.Results[1].SessionID {
		t.Fatalf("code=%d stderr=%s %+v", code, stderr, got)
	}
}
