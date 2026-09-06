package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/amuldowney/cc-search/internal/output"
)

func decodeValue(t *testing.T, stdout *bytes.Buffer, value any) {
	t.Helper()
	if err := json.Unmarshal(stdout.Bytes(), value); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, stdout.String())
	}
}

func TestSessionsReturnsCompactRowsAndCWDFilter(t *testing.T) {
	root := t.TempDir()
	line := `{"type":"session","id":"session-a","timestamp":"2026-08-25T00:00:00Z","cwd":"/work/a","visibility":"main"}
{"type":"message","id":"m1","timestamp":"2026-08-25T00:00:01Z","message":{"role":"user","content":[{"type":"text","text":"deploy system"}]}}
`
	if err := os.WriteFile(filepath.Join(root, "session.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{root}}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"sessions", "--cwd", "/work/a"}, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	var response output.SessionsResponse
	decodeValue(t, &stdout, &response)
	if response.Total != 1 || response.Sessions[0].SessionID != "session-a" {
		t.Fatalf("response = %+v", response)
	}
	row := response.Sessions[0]
	if row.CWD != "/work/a" || row.Visibility != "main" || row.LastPreview != "deploy system" || row.MessageCount != 1 {
		t.Fatalf("session row = %+v", row)
	}
}

func TestCommandsPairsToolResultAndHonorsToolFilter(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"assistant","uuid":"call","sessionId":"session-a","timestamp":"2026-08-25T00:00:01Z","message":{"content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"docker compose up"}}]}}
{"type":"user","uuid":"result","sessionId":"session-a","timestamp":"2026-08-25T00:00:02Z","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"no such table: messages"}]}}
`
	if err := os.WriteFile(filepath.Join(root, "session-a.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{root}}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"commands", "no such table", "--tool", "bash", "--full"}, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	var response output.CommandsResponse
	decodeValue(t, &stdout, &response)
	if response.Total != 1 || response.Commands[0].Tool != "Bash" || response.Commands[0].Output != "no such table: messages" {
		t.Fatalf("response = %+v", response)
	}
	args, ok := response.Commands[0].Arguments.(map[string]any)
	if !ok || args["command"] != "docker compose up" {
		t.Fatalf("arguments = %#v", response.Commands[0].Arguments)
	}
}

func TestContextDeduplicatesOverlappingWindows(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"user","uuid":"m1","sessionId":"session-a","timestamp":"2026-08-25T00:00:01Z","message":{"content":"before"}}
{"type":"user","uuid":"m2","sessionId":"session-a","timestamp":"2026-08-25T00:00:02Z","message":{"content":"needle one"}}
{"type":"user","uuid":"m3","sessionId":"session-a","timestamp":"2026-08-25T00:00:03Z","message":{"content":"between"}}
{"type":"user","uuid":"m4","sessionId":"session-a","timestamp":"2026-08-25T00:00:04Z","message":{"content":"needle two"}}
{"type":"user","uuid":"m5","sessionId":"session-a","timestamp":"2026-08-25T00:00:05Z","message":{"content":"after"}}
`
	if err := os.WriteFile(filepath.Join(root, "session-a.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{root}}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"context", "needle", "--hits", "2", "--before", "1", "--after", "1"}, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	var response output.ContextResponse
	decodeValue(t, &stdout, &response)
	if response.Query != "needle" || response.TotalHits != 2 || response.Total != 5 {
		t.Fatalf("response totals = %+v", response)
	}
	if len(response.Results[1].HitIDs) != 1 || len(response.Results[2].HitIDs) != 2 {
		t.Fatalf("provenance = %+v", response.Results)
	}
}

func TestActivityCommandsExposeStatusAndFullLinkage(t *testing.T) {
	root := t.TempDir()
	body := `{"type":"session","id":"parent-session","timestamp":"2026-08-25T00:00:00Z","cwd":"/work"}
{"type":"message","id":"call-1","timestamp":"2026-08-25T00:00:01Z","message":{"role":"assistant","content":"launch"}}
{"type":"custom","customType":"pi:activity-started","id":"start-1","parentId":"call-1","timestamp":"2026-08-25T00:00:02Z","data":{"activityId":"activity-1","status":"running","startedAt":1787625242000,"title":"Explore","description":"inspect code","model":"model","effort":"high"}}
{"type":"custom","customType":"pi:activity-terminal","id":"terminal-1","parentId":"call-1","timestamp":"2026-08-25T00:00:03Z","data":{"activityId":"activity-1","status":"failed","completedAt":1787625243000,"resultSummary":"failed","toolUses":4}}
`
	if err := os.WriteFile(filepath.Join(root, "parent.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{root}}
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"activity", "activity-1", "--full"}, cfg, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	var activity output.Activity
	decodeValue(t, &stdout, &activity)
	if activity.Status != "failed" || activity.Title != "Explore" || activity.ParentSessionID != "parent-session" || activity.ToolUses != 4 || activity.Description != "inspect code" {
		t.Fatalf("activity = %+v", activity)
	}
}
