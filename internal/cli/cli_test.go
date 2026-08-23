package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmuldowney/cc-search/internal/index"
	"github.com/andrewmuldowney/cc-search/internal/output"
)

type msg struct {
	typ        string
	content    string
	minutesAgo int
}

func fixture(t *testing.T, msgs []msg) Config {
	t.Helper()
	dir := t.TempDir()
	var body string
	for i, m := range msgs {
		ts := time.Now().Add(-time.Duration(m.minutesAgo) * time.Minute).UTC()
		body += fmt.Sprintf(
			`{"type":%q,"uuid":"u-%d","sessionId":"session-a","timestamp":%q,"message":{"content":%q}}`+"\n",
			m.typ, i, ts.Format(time.RFC3339Nano), m.content)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-a.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Config{
		IndexPath:      filepath.Join(t.TempDir(), "index.db"),
		TranscriptDirs: []string{dir},
	}
}

// run executes the CLI and decodes its JSON output.
func run(t *testing.T, cfg Config, args ...string) (output.Response, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, cfg, &stdout, &stderr)

	var resp output.Response
	if stdout.Len() > 0 {
		if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
			t.Fatalf("stdout is not valid JSON: %v\n%s", err, stdout.String())
		}
	}
	return resp, stderr.String(), code
}

func TestServeAcceptsOnlyLoopbackHosts(t *testing.T) {
	for _, host := range []string{"127.0.0.1", "::1", "localhost"} {
		if !isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"0.0.0.0", "192.0.2.1", "example.test"} {
		if isLoopbackHost(host) {
			t.Errorf("isLoopbackHost(%q) = true, want false", host)
		}
	}
}

func TestLastReturnsRequestedCount(t *testing.T) {
	cfg := fixture(t, []msg{
		{"user", "oldest", 30},
		{"user", "middle", 20},
		{"user", "newest", 10},
	})

	resp, stderr, code := run(t, cfg, "last", "2")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 2 {
		t.Fatalf("Total = %d, want 2", resp.Total)
	}
	if resp.Results[0].Preview != "newest" {
		t.Errorf("first result = %q, want newest", resp.Results[0].Preview)
	}
}

func TestLastDefaultsToTenMessages(t *testing.T) {
	var msgs []msg
	for i := 0; i < 15; i++ {
		msgs = append(msgs, msg{"user", fmt.Sprintf("message %d", i), 60 - i})
	}
	cfg := fixture(t, msgs)

	resp, stderr, code := run(t, cfg, "last")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 10 {
		t.Errorf("Total = %d, want the default of 10", resp.Total)
	}
}

func TestLastHoursFlag(t *testing.T) {
	cfg := fixture(t, []msg{
		{"user", "long ago", 60 * 6},
		{"user", "just now", 5},
	})

	resp, stderr, code := run(t, cfg, "last", "--hours", "2")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 1 || resp.Results[0].Preview != "just now" {
		t.Errorf("results = %+v, want only the recent message", resp.Results)
	}
}

func TestSearchFindsMatch(t *testing.T) {
	cfg := fixture(t, []msg{
		{"user", "the waveform table lives in display.cpp", 20},
		{"user", "unrelated", 10},
	})

	resp, stderr, code := run(t, cfg, "search", "waveform")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 1 {
		t.Fatalf("Total = %d, want 1", resp.Total)
	}
	if !strings.Contains(resp.Results[0].Preview, "waveform") {
		t.Errorf("Preview = %q", resp.Results[0].Preview)
	}
}

func TestSearchExcludesCurrentPiSessionByDefault(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "waveform current", 10}})
	history := filepath.Join(cfg.TranscriptDirs[0], "history.jsonl")
	line := fmt.Sprintf(
		`{"type":"user","uuid":"history-1","sessionId":"history","timestamp":%q,"message":{"content":"waveform history"}}`,
		time.Now().Add(-20*time.Minute).UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(history, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PI_SESSION_ID", "session-a")
	resp, stderr, code := run(t, cfg, "search", "waveform")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 1 || resp.Results[0].SessionID != "history" {
		t.Fatalf("results = %+v, want only the history session", resp.Results)
	}
}

func TestSearchIncludesCurrentSessionWhenRequested(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "waveform current", 10}})
	t.Setenv("PI_SESSION_ID", "session-a")

	resp, stderr, code := run(t, cfg, "search", "waveform", "--include-current")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 1 || resp.Results[0].SessionID != "session-a" {
		t.Fatalf("results = %+v, want the current session", resp.Results)
	}
}

func TestSearchExplicitSessionIncludesCurrentSession(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "waveform current", 10}})
	t.Setenv("PI_SESSION_ID", "session-a")

	resp, stderr, code := run(t, cfg, "search", "waveform", "--session", "session-a")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 1 || resp.Results[0].SessionID != "session-a" {
		t.Fatalf("results = %+v, want the explicitly selected current session", resp.Results)
	}
}

func TestSearchKeepsAllSessionsOutsidePi(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "waveform current", 10}})
	t.Setenv("PI_SESSION_ID", "session-a")

	resp, stderr, code := run(t, cfg, "search", "waveform")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 0 {
		t.Fatalf("Total = %d, want 0 when no historical session matches", resp.Total)
	}
}

func TestSearchTruncatedWhenMoreResultsExist(t *testing.T) {
	cfg := fixture(t, []msg{
		{"user", "grayscale one", 30},
		{"user", "grayscale two", 20},
		{"user", "grayscale three", 10},
	})

	resp, _, _ := run(t, cfg, "search", "grayscale", "--limit", "2")

	if resp.Total != 2 {
		t.Fatalf("Total = %d, want 2", resp.Total)
	}
	if !resp.Truncated {
		t.Error("Truncated = false, want true when results were cut off")
	}
}

func TestSearchNotTruncatedWhenLimitNotReached(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "grayscale one", 30}})

	resp, _, _ := run(t, cfg, "search", "grayscale", "--limit", "5")

	if resp.Truncated {
		t.Error("Truncated = true, want false when every match was returned")
	}
}

func TestSearchTreatsRecapLikeTextNormally(t *testing.T) {
	cfg := fixture(t, []msg{
		{"assistant", "recap: the proxy moved", 30},
		{"assistant", "the proxy moved", 20},
	})

	resp, stderr, code := run(t, cfg, "search", "proxy")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 2 {
		t.Fatalf("Total = %d, want 2", resp.Total)
	}
}

func TestSearchRejectsRemovedRecapFlags(t *testing.T) {
	cfg := fixture(t, []msg{{"assistant", "proxy note", 10}})
	for _, removed := range []string{"--prefer-recaps", "--recaps-only"} {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"search", "proxy", removed}, cfg, &stdout, &stderr)
		if code != 2 {
			t.Errorf("flag %s exit code = %d, want 2", removed, code)
		}
	}
}

// toolAndProseFixture seeds one tool-only message and one spoken message,
// both mentioning "caddy".
func toolAndProseFixture(t *testing.T) Config {
	t.Helper()
	dir := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{"type":"assistant","uuid":"c1","sessionId":"session-a","timestamp":%q,`+
		`"message":{"content":[{"type":"tool_use","name":"Edit","input":{"file_path":"caddy.json"}}]}}`+"\n"+
		`{"type":"assistant","uuid":"c2","sessionId":"session-a","timestamp":%q,`+
		`"message":{"content":[{"type":"text","text":"the caddy config moved"}]}}`+"\n", ts, ts)
	if err := os.WriteFile(filepath.Join(dir, "session-a.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{dir}}
}

func TestSearchIgnoresToolCallsByDefault(t *testing.T) {
	cfg := toolAndProseFixture(t)

	resp, stderr, code := run(t, cfg, "search", "caddy")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 1 {
		t.Fatalf("Total = %d, want 1 — tool calls are ignored by default", resp.Total)
	}
	if !strings.Contains(resp.Results[0].Preview, "moved") {
		t.Errorf("Preview = %q, want the spoken message", resp.Results[0].Preview)
	}
}

func TestSearchAllFlagIncludesToolCalls(t *testing.T) {
	cfg := toolAndProseFixture(t)

	resp, stderr, code := run(t, cfg, "search", "caddy", "--all")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2 with --all", resp.Total)
	}
}

func TestLastIgnoresToolCallsByDefault(t *testing.T) {
	cfg := toolAndProseFixture(t)

	resp, stderr, code := run(t, cfg, "last", "10")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 1 {
		t.Fatalf("Total = %d, want 1 — tool-only messages are skipped", resp.Total)
	}
}

func TestLastAllFlagIncludesToolCalls(t *testing.T) {
	cfg := toolAndProseFixture(t)

	resp, _, _ := run(t, cfg, "last", "10", "--all")

	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2 with --all", resp.Total)
	}
}

func TestPreviewShowsProseNotToolCallByDefault(t *testing.T) {
	dir := t.TempDir()
	ts := time.Now().UTC().Format(time.RFC3339Nano)
	body := fmt.Sprintf(`{"type":"assistant","uuid":"m1","sessionId":"session-a","timestamp":%q,`+
		`"message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"grep caddy"}},`+
		`{"type":"text","text":"found the caddy entry"}]}}`+"\n", ts)
	if err := os.WriteFile(filepath.Join(dir, "session-a.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{dir}}

	resp, _, _ := run(t, cfg, "last", "1")

	if resp.Results[0].Preview != "found the caddy entry" {
		t.Errorf("Preview = %q, want the prose rather than the tool call", resp.Results[0].Preview)
	}
}

func TestSearchTypeFilter(t *testing.T) {
	cfg := fixture(t, []msg{
		{"user", "deploy the server", 30},
		{"assistant", "deploy finished", 20},
	})

	resp, _, _ := run(t, cfg, "search", "deploy", "--type", "user")

	if resp.Total != 1 || resp.Results[0].Type != "user" {
		t.Errorf("results = %+v, want one user message", resp.Results)
	}
}

func TestFullFlagIncludesContent(t *testing.T) {
	body := strings.Repeat("x", 300)
	cfg := fixture(t, []msg{{"user", "portfolio " + body, 10}})

	resp, _, _ := run(t, cfg, "search", "portfolio", "--full")

	if resp.Results[0].Content != "portfolio "+body {
		t.Errorf("Content = %q, want the whole message body", resp.Results[0].Content)
	}
}

func TestPreviewLengthFlag(t *testing.T) {
	cfg := fixture(t, []msg{{"user", strings.Repeat("y", 200), 10}})

	resp, _, _ := run(t, cfg, "last", "1", "--preview-length", "20")

	if len([]rune(resp.Results[0].Preview)) != 21 {
		t.Errorf("preview = %q, want 20 runes plus an ellipsis", resp.Results[0].Preview)
	}
}

func TestSessionFlagFiltersResults(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "from a", 10}})
	other := filepath.Join(cfg.TranscriptDirs[0], "session-b.jsonl")
	line := fmt.Sprintf(
		`{"type":"user","uuid":"b-1","sessionId":"session-b","timestamp":%q,"message":{"content":"from b"}}`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err := os.WriteFile(other, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, _, _ := run(t, cfg, "last", "10", "--session", "session-b")

	if resp.Total != 1 || resp.Results[0].Preview != "from b" {
		t.Errorf("results = %+v, want only session-b", resp.Results)
	}
}

func TestOpenIndexJoinsSyncAndCloseErrors(t *testing.T) {
	root := t.TempDir()
	transcripts := filepath.Join(root, "transcripts")
	if err := os.Mkdir(transcripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcripts, "oversized.jsonl"),
		[]byte(strings.Repeat("x", 16*1024*1024+1)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	closeErr := errors.New("release lifecycle lock: injected test failure")
	previous := closeIndex
	closeIndex = func(db *index.DB) error {
		return errors.Join(db.Close(), closeErr)
	}
	t.Cleanup(func() { closeIndex = previous })

	_, err := openIndex(Config{
		IndexPath: filepath.Join(root, "index.db"), TranscriptDirs: []string{transcripts},
	}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("openIndex succeeded despite an oversized transcript")
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("openIndex error = %v, want release error too", err)
	}
	if !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("openIndex error = %v, want sync error too", err)
	}
}

func TestRunRebuildJoinsRebuildAndCloseErrors(t *testing.T) {
	root := t.TempDir()
	transcripts := filepath.Join(root, "transcripts")
	if err := os.Mkdir(transcripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transcripts, "oversized.jsonl"),
		[]byte(strings.Repeat("x", 16*1024*1024+1)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	closeErr := errors.New("release lifecycle lock: injected test failure")
	previous := closeIndex
	closeIndex = func(db *index.DB) error {
		return errors.Join(db.Close(), closeErr)
	}
	t.Cleanup(func() { closeIndex = previous })

	var stdout, stderr bytes.Buffer
	err := runRebuild([]string{
		"--index", filepath.Join(root, "index.db"), "--transcripts", transcripts,
	}, Config{}, &stdout, &stderr)
	if err == nil {
		t.Fatal("runRebuild succeeded despite an oversized transcript")
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("runRebuild error = %v, want release error too", err)
	}
	if !strings.Contains(err.Error(), "token too long") {
		t.Fatalf("runRebuild error = %v, want rebuild error too", err)
	}
}

func TestConcurrentCLIProcessesWorker(t *testing.T) {
	if os.Getenv("CC_SEARCH_CONCURRENT_WORKER") != "1" {
		return
	}

	ready := filepath.Join(os.Getenv("CC_SEARCH_CONCURRENT_READY_DIR"), fmt.Sprint(os.Getpid()))
	if err := os.WriteFile(ready, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(os.Getenv("CC_SEARCH_CONCURRENT_RELEASE")); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"search", "needle",
		"--index", os.Getenv("CC_SEARCH_CONCURRENT_INDEX"),
		"--transcripts", os.Getenv("CC_SEARCH_CONCURRENT_TRANSCRIPTS"),
	}, Config{}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("CLI exited %d: stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response output.Response
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("CLI returned invalid JSON: %v: %q", err, stdout.String())
	}
	if response.Total != 1 {
		t.Fatalf("CLI returned %d results, want 1", response.Total)
	}
}

func waitConcurrentWorker(cmd *exec.Cmd, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		select {
		case err := <-done:
			return fmt.Errorf("worker exceeded %s and was killed: %w", timeout, err)
		case <-time.After(time.Second):
			return fmt.Errorf("worker exceeded %s and did not exit after being killed", timeout)
		}
	}
}

func TestConcurrentCLIProcesses(t *testing.T) {
	root := t.TempDir()
	transcripts := filepath.Join(root, "transcripts")
	if err := os.Mkdir(transcripts, 0o755); err != nil {
		t.Fatal(err)
	}
	var transcript strings.Builder
	for i := 0; i < 10000; i++ {
		content := "filler"
		if i == 0 {
			content = "needle"
		}
		fmt.Fprintf(&transcript,
			`{"type":"user","uuid":"message-%d","sessionId":"session-a","timestamp":"2026-08-05T00:00:00Z","message":{"content":%q}}`+"\n", i, content)
	}
	if err := os.WriteFile(filepath.Join(transcripts, "session-a.jsonl"), []byte(transcript.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(root, "shared", "index.db")
	readyDir := filepath.Join(root, "ready")
	if err := os.Mkdir(readyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	release := filepath.Join(root, "release")

	const processCount = 11
	commands := make([]*exec.Cmd, processCount)
	started := make([]bool, processCount)
	waited := make([]bool, processCount)
	outputs := make([]struct {
		stdout bytes.Buffer
		stderr bytes.Buffer
	}, processCount)
	t.Cleanup(func() {
		if err := os.WriteFile(release, nil, 0o644); err != nil {
			for i, cmd := range commands {
				if started[i] && cmd.Process != nil {
					_ = cmd.Process.Kill()
				}
			}
		}
		for i, cmd := range commands {
			if started[i] && !waited[i] {
				_ = waitConcurrentWorker(cmd, 2*time.Second)
				waited[i] = true
			}
		}
	})
	for i := range commands {
		cmd := exec.Command(os.Args[0], "-test.run=TestConcurrentCLIProcessesWorker", "-test.v")
		cmd.Env = append(os.Environ(),
			"CC_SEARCH_CONCURRENT_WORKER=1",
			"CC_SEARCH_CONCURRENT_READY_DIR="+readyDir,
			"CC_SEARCH_CONCURRENT_RELEASE="+release,
			"CC_SEARCH_CONCURRENT_INDEX="+indexPath,
			"CC_SEARCH_CONCURRENT_TRANSCRIPTS="+transcripts,
		)
		cmd.Stdout = &outputs[i].stdout
		cmd.Stderr = &outputs[i].stderr
		commands[i] = cmd
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		started[i] = true
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		entries, err := os.ReadDir(readyDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) == processCount {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d worker processes reached the barrier", len(entries), processCount)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range commands {
		err := waitConcurrentWorker(cmd, 15*time.Second)
		waited[i] = true
		if err != nil {
			t.Errorf("worker %d failed: %v\nstdout=%s\nstderr=%s", i, err,
				outputs[i].stdout.String(), outputs[i].stderr.String())
		}
	}
}

func TestRebuildReportsIndexedCounts(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "hello", 10}})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"rebuild"}, cfg, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1") {
		t.Errorf("rebuild output does not report a count: %q", stdout.String())
	}
}

func TestIndexPersistsBetweenInvocations(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "persisted message", 10}})

	if _, _, code := run(t, cfg, "last", "1"); code != 0 {
		t.Fatal("first invocation failed")
	}
	if err := os.RemoveAll(cfg.TranscriptDirs[0]); err != nil {
		t.Fatal(err)
	}

	// The transcripts are gone but the index should still answer.
	var stdout, stderr bytes.Buffer
	code := Run([]string{"last", "1"}, cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	var resp output.Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Total != 1 {
		t.Errorf("Total = %d, want 1 from the persisted index", resp.Total)
	}
}

func TestNoMatchReturnsEmptyResultsAndZeroExit(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "hello", 10}})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"search", "nonexistentterm"}, cfg, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 for an empty result", code)
	}
	want := `{"results":[],"total":0,"truncated":false,"relaxed":false,` + `"budget":{"limit":60000,"spent":0,"dropped":0,"shrunk":false}}`
	if strings.TrimSpace(stdout.String()) != want {
		t.Errorf("got %s, want %s", stdout.String(), want)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	cfg := fixture(t, nil)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"frobnicate"}, cfg, &stdout, &stderr)

	if code == 0 {
		t.Error("exit code = 0, want non-zero for an unknown command")
	}
	if stderr.Len() == 0 {
		t.Error("nothing written to stderr for an unknown command")
	}
}

func TestNoArgsPrintsUsage(t *testing.T) {
	cfg := fixture(t, nil)

	var stdout, stderr bytes.Buffer
	code := Run(nil, cfg, &stdout, &stderr)

	if code == 0 {
		t.Error("exit code = 0, want non-zero when no command is given")
	}
	if !strings.Contains(stderr.String(), "usage") {
		t.Errorf("stderr does not contain usage text: %q", stderr.String())
	}
}

func TestSearchRejectsFlagInPatternPosition(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "caddy proxy notes", 10}})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"search", "--prose", "caddy"}, cfg, &stdout, &stderr)

	if code == 0 {
		t.Fatalf("exit code = 0; a flag in the pattern position silently searches for the flag: %s",
			stdout.String())
	}
	if !strings.Contains(stderr.String(), "pattern") {
		t.Errorf("stderr = %q, want an explanation that the pattern comes first", stderr.String())
	}
}

func TestSearchRejectsStrayArgumentAfterFlags(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "caddy proxy notes", 10}})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"search", "caddy", "--limit", "2", "proxy"}, cfg, &stdout, &stderr)

	if code == 0 {
		t.Errorf("exit code = 0; a stray argument was silently ignored: %s", stdout.String())
	}
}

func TestLastFiltersByType(t *testing.T) {
	cfg := fixture(t, []msg{
		{"user", "what is broken", 30},
		{"assistant", "the proxy is broken", 20},
	})

	resp, stderr, code := run(t, cfg, "last", "10", "--type", "user")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 1 || resp.Results[0].Type != "user" {
		t.Errorf("results = %+v, want only the user message", resp.Results)
	}
}

func TestUnknownFlagExitsWithUsageCode(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "hello", 10}})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"search", "hello", "--nosuchflag"}, cfg, &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code = %d, want 2 for a usage error", code)
	}
}

func TestSearchRequiresPattern(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "hello", 10}})

	var stdout, stderr bytes.Buffer
	code := Run([]string{"search"}, cfg, &stdout, &stderr)

	if code == 0 {
		t.Error("exit code = 0, want non-zero when no pattern is given")
	}
}

func TestMissingTranscriptDirIsNotFatal(t *testing.T) {
	cfg := Config{
		IndexPath:      filepath.Join(t.TempDir(), "index.db"),
		TranscriptDirs: []string{filepath.Join(t.TempDir(), "does-not-exist")},
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"last", "5"}, cfg, &stdout, &stderr)

	if code != 0 {
		t.Fatalf("exit code = %d, want 0 with a warning, stderr = %s", code, stderr.String())
	}
	if stderr.Len() == 0 {
		t.Error("expected a warning on stderr about the missing transcript dir")
	}
}

// conversation writes a numbered exchange in one session.
func conversation(t *testing.T, n int) Config {
	t.Helper()
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour)
	var body string
	for i := 0; i < n; i++ {
		ts := base.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339Nano)
		body += fmt.Sprintf(
			`{"type":"user","uuid":"id%02d","sessionId":"session-a","timestamp":%q,`+
				`"message":{"content":"message %d"}}`+"\n", i, ts, i)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-a.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{dir}}
}

func TestReadReturnsMessageWithSurroundingContext(t *testing.T) {
	cfg := conversation(t, 10)

	resp, stderr, code := run(t, cfg, "read", "id05", "--before", "2", "--after", "2")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 5 {
		t.Fatalf("Total = %d, want 5", resp.Total)
	}
	if resp.Results[0].Preview != "message 3" {
		t.Errorf("first = %q, want message 3 — read is chronological", resp.Results[0].Preview)
	}
	if resp.Results[4].Preview != "message 7" {
		t.Errorf("last = %q, want message 7", resp.Results[4].Preview)
	}
}

func TestReadDefaultsToSurroundingFive(t *testing.T) {
	cfg := conversation(t, 30)

	resp, _, code := run(t, cfg, "read", "id15")

	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if resp.Total != 11 {
		t.Errorf("Total = %d, want 11 (5 before + target + 5 after)", resp.Total)
	}
}

func TestReadAcceptsIDPrefix(t *testing.T) {
	cfg := conversation(t, 3)

	resp, stderr, code := run(t, cfg, "read", "id0", "--before", "0", "--after", "0")

	if code == 0 {
		t.Fatalf("exit code = 0 for an ambiguous prefix: %+v", resp.Results)
	}
	if !strings.Contains(stderr, "ambiguous") {
		t.Errorf("stderr = %q, want it to say the prefix is ambiguous", stderr)
	}
}

func TestReadRejectsUnknownID(t *testing.T) {
	cfg := conversation(t, 3)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"read", "deadbeef"}, cfg, &stdout, &stderr)

	if code == 0 {
		t.Error("exit code = 0, want non-zero for an unknown id")
	}
}

func TestReadRequiresAnID(t *testing.T) {
	cfg := conversation(t, 3)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"read"}, cfg, &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
}

func TestBudgetFlagLimitsOutputAndWarns(t *testing.T) {
	var msgs []msg
	for i := 0; i < 10; i++ {
		msgs = append(msgs, msg{"user", strings.Repeat("x", 200), 60 - i})
	}
	cfg := fixture(t, msgs)

	resp, stderr, code := run(t, cfg, "last", "10", "--full", "--budget", "500")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total >= 10 {
		t.Errorf("Total = %d, want fewer than 10 under a 500 char budget", resp.Total)
	}
	if !resp.Truncated {
		t.Error("Truncated = false, want true")
	}
	if !strings.Contains(stderr, "budget") {
		t.Errorf("stderr = %q, want a warning that the budget bit", stderr)
	}
}

func TestDefaultBudgetProtectsAgainstHugeFullOutput(t *testing.T) {
	cfg := fixture(t, []msg{{"user", strings.Repeat("y", 300000), 10}})

	resp, stderr, code := run(t, cfg, "last", "1", "--full")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if len(resp.Results[0].Content) > DefaultBudget {
		t.Errorf("content is %d chars with no --budget; want the default cap of %d",
			len(resp.Results[0].Content), DefaultBudget)
	}
	if !strings.Contains(stderr, "budget") {
		t.Errorf("stderr = %q, want a warning", stderr)
	}
}

func TestBudgetZeroDisablesTheCap(t *testing.T) {
	cfg := fixture(t, []msg{{"user", strings.Repeat("z", 120000), 10}})

	resp, _, code := run(t, cfg, "last", "1", "--full", "--budget", "0")

	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if len(resp.Results[0].Content) != 120000 {
		t.Errorf("content is %d chars, want the full 120000 with --budget 0",
			len(resp.Results[0].Content))
	}
}

func termFixture(t *testing.T) Config {
	return fixture(t, []msg{
		{"user", "alpha and beta together", 30},
		{"user", "gamma on its own", 20},
		{"user", "nothing relevant here", 10},
	})
}

func TestSearchRelaxesToAnyWhenNothingMatchesAllTerms(t *testing.T) {
	cfg := termFixture(t)

	resp, stderr, code := run(t, cfg, "search", "alpha gamma")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 2 {
		t.Fatalf("Total = %d, want 2 — an empty AND query should relax to OR", resp.Total)
	}
	if !resp.Relaxed {
		t.Error("Relaxed = false; the caller must be able to tell this was not an exact match")
	}
	if !strings.Contains(stderr, "relax") {
		t.Errorf("stderr = %q, want a note that the query was relaxed", stderr)
	}
}

func TestSearchDoesNotRelaxWhenAllTermsMatch(t *testing.T) {
	cfg := termFixture(t)

	resp, _, code := run(t, cfg, "search", "alpha beta")

	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if resp.Total != 1 || resp.Relaxed {
		t.Errorf("Total = %d, Relaxed = %v; want the strict match kept", resp.Total, resp.Relaxed)
	}
}

func TestSearchDoesNotRelaxASingleTerm(t *testing.T) {
	cfg := termFixture(t)

	resp, _, code := run(t, cfg, "search", "nonexistentterm")

	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if resp.Total != 0 {
		t.Errorf("Total = %d, want 0", resp.Total)
	}
	if resp.Relaxed {
		t.Error("Relaxed = true; there is nothing to relax in a one word query")
	}
}

func TestAnyFlagIsNotReportedAsRelaxed(t *testing.T) {
	cfg := termFixture(t)

	resp, _, code := run(t, cfg, "search", "alpha gamma", "--any")

	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	if resp.Total != 2 {
		t.Fatalf("Total = %d, want 2", resp.Total)
	}
	if resp.Relaxed {
		t.Error("Relaxed = true; OR was asked for, not fallen back to")
	}
}

func TestRelaxedWarningCountsWhatWasReturned(t *testing.T) {
	cfg := termFixture(t)

	resp, stderr, _ := run(t, cfg, "search", "alpha gamma", "--limit", "1")

	if resp.Total != 1 {
		t.Fatalf("Total = %d, want 1", resp.Total)
	}
	if strings.Contains(stderr, "2 result") {
		t.Errorf("stderr = %q; it reports the over-fetch count, not what was returned", stderr)
	}
	if !strings.Contains(stderr, "1 result") {
		t.Errorf("stderr = %q, want it to report 1 result", stderr)
	}
}

func TestRawFlagSupportsBooleanOperators(t *testing.T) {
	cfg := termFixture(t)

	resp, stderr, code := run(t, cfg, "search", "alpha OR gamma", "--raw")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 2 {
		t.Errorf("Total = %d, want 2", resp.Total)
	}
}

func TestRawSyntaxErrorIsAUsageError(t *testing.T) {
	cfg := termFixture(t)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"search", "display.cpp", "--raw"}, cfg, &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code = %d, want 2 for a bad query", code)
	}
	if !strings.Contains(stderr.String(), "quot") {
		t.Errorf("stderr = %q, want it to suggest quoting punctuation", stderr.String())
	}
}

func TestRawQueriesAreNeverRelaxed(t *testing.T) {
	cfg := termFixture(t)

	resp, stderr, code := run(t, cfg, "search", "alpha AND gamma", "--raw")

	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 0 {
		t.Fatalf("Total = %d, want 0", resp.Total)
	}
	if resp.Relaxed {
		t.Error("Relaxed = true; an explicit boolean query must not be rewritten")
	}
}

func TestRawAndAnyTogetherIsRejected(t *testing.T) {
	cfg := termFixture(t)

	var stdout, stderr bytes.Buffer
	code := Run([]string{"search", "alpha gamma", "--raw", "--any"}, cfg, &stdout, &stderr)

	if code != 2 {
		t.Errorf("exit code = %d, want 2 — --any has no meaning for a raw query", code)
	}
}
