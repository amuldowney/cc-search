package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Marked examples are executed verbatim with documented placeholders filled in.
// No real transcripts, user index, shell helpers, or installed binary are used.
func TestSkillExamples(t *testing.T) {
	_, source, _, _ := runtime.Caller(0)
	repo := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	skill := filepath.Join(repo, ".claude/skills/cc-search")
	pattern := regexp.MustCompile("(?s)<!-- smoke: ([a-z-]+) -->\\s*```bash\\n(.*?)\\n```")
	var examples [][]string
	for _, name := range []string{"SKILL.md", "references/recipes.md"} {
		data, err := os.ReadFile(filepath.Join(skill, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), `[:8]`) || strings.Contains(string(data), `r[\"id\"]`) {
			t.Fatalf("%s still shortens IDs or contains the broken Python quoting example", name)
		}
		examples = append(examples, pattern.FindAllStringSubmatch(string(data), -1)...)
	}
	if len(examples) < 8 {
		t.Fatalf("want at least 8 executable skill examples, got %d", len(examples))
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "cc-search-real")
	build := exec.Command("go", "build", "-buildvcs=false", "-tags", "sqlite_fts5", "-o", executable, "./cmd/cc-search")
	build.Dir = repo
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	transcripts := filepath.Join(root, "transcripts")
	if err := os.MkdirAll(transcripts, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	fixture := fmt.Sprintf(`{"type":"session","id":"session-a","cwd":%q,"timestamp":%q}
{"type":"message","id":"message-long-id","timestamp":%q,"message":{"role":"user","content":"deployment decision: preserve snapshots for wireguard authentication"}}
{"type":"message","id":"tool-call","timestamp":%q,"message":{"role":"assistant","content":[{"type":"toolCall","id":"t1","name":"bash","arguments":{"command":"docker compose up"}}]}}
{"type":"message","id":"tool-result","timestamp":%q,"message":{"role":"toolResult","toolCallId":"t1","toolName":"bash","content":[{"type":"text","text":"no such table: messages"}]}}
`, root, now, now, now, now)
	if err := os.WriteFile(filepath.Join(transcripts, "fixture.jsonl"), []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	wrapper := "#!/bin/sh\nexec \"$CC_TEST_BINARY\" \"$@\" --index \"$CC_TEST_INDEX\" --transcripts \"$CC_TEST_TRANSCRIPTS\"\n"
	if err := os.WriteFile(filepath.Join(bin, "cc-search"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	for _, example := range examples {
		t.Run(example[1], func(t *testing.T) {
			script := strings.NewReplacer("FULL_MESSAGE_ID", "session-a:message-long-id", "FULL_SESSION_ID", "session-a").Replace(example[2])
			cmd := exec.Command("bash", "-euo", "pipefail", "-c", script)
			cmd.Dir = root
			env := []string{}
			for _, v := range os.Environ() {
				if !strings.HasPrefix(v, "PI_SESSION_") && !strings.HasPrefix(v, "PATH=") && !strings.HasPrefix(v, "CC_TEST_") {
					env = append(env, v)
				}
			}
			cmd.Env = append(env, "PATH="+bin+":"+os.Getenv("PATH"), "CC_TEST_BINARY="+executable, "CC_TEST_INDEX="+filepath.Join(root, "index.db"), "CC_TEST_TRANSCRIPTS="+transcripts)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("example failed: %v\n%s\n%s", err, script, out)
			}
			if len(out) == 0 {
				t.Fatalf("empty example output: %s", script)
			}
			switch example[1] {
			case "survey", "read-hit", "read-context", "recent", "project", "decision", "bundle":
				if !strings.Contains(string(out), "session-a:message-long-id") {
					t.Fatalf("example lost full message ID or found no hit: %s", out)
				}
			case "command-arguments", "command-output":
				if !strings.Contains(string(out), "docker compose up") {
					t.Fatalf("example did not retrieve the invocation: %s", out)
				}
			}
		})
	}
}
