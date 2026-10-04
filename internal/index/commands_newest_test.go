package index

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandsReturnNewestMatchBeyondRelevanceCandidateCap(t *testing.T) {
	root := t.TempDir()
	start := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	var lines []string
	for i := 0; i < 300; i++ {
		lines = append(lines, fmt.Sprintf(`{"type":"assistant","uuid":"old-%d","sessionId":"session","timestamp":%q,"message":{"content":[{"type":"tool_use","id":"call-%d","name":"Bash","input":{"command":"needle"}}]}}`, i, start.Add(time.Duration(i)*time.Second).Format(time.RFC3339), i))
	}
	// This latest invocation matches, but its long document gives it a lower
	// BM25 score than all 300 short old calls. Commands order by recency, not
	// relevance: limiting FTS candidates before choosing commands loses it.
	lines = append(lines, fmt.Sprintf(`{"type":"assistant","uuid":"newest","sessionId":"session","timestamp":%q,"message":{"content":[{"type":"tool_use","id":"call-newest","name":"Bash","input":{"command":%q}}]}}`, start.Add(time.Hour).Format(time.RFC3339), "needle "+strings.Repeat("padding ", 2000)))
	writeRawTranscript(t, root, "session", lines)
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}
	commands, err := db.Commands(CommandOptions{Query: "needle", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].MessageID != "newest" {
		t.Fatalf("newest matching invocation excluded by relevance prefilter; returned IDs: %v", commandIDs(commands))
	}
}

func commandIDs(commands []CommandRecord) []string {
	out := make([]string, 0, len(commands))
	for _, command := range commands {
		out = append(out, command.MessageID)
	}
	return out
}
