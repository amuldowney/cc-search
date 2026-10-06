package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amuldowney/cc-search/internal/index"
	"github.com/amuldowney/cc-search/internal/output"
)

func TestRedactApplyRebuildsAffectedSession(t *testing.T) {
	root := t.TempDir()
	secret := "supersecretneedle"
	t.Setenv("CC_SEARCH_TEST_TOKEN", secret)
	line := fmt.Sprintf(
		`{"type":"user","uuid":"m1","sessionId":"session-a","timestamp":%q,"message":{"content":"leaked %s"}}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano), secret)
	path := filepath.Join(root, "session-a.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{root}}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"redact", "--hours", "48", "--apply"}, cfg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr.String())
	}
	var response output.RedactResponse
	decodeValue(t, &stdout, &response)
	if !response.Applied || response.FilesRedacted != 1 || response.SessionsRebuilt != 1 ||
		response.IndexMatches != 0 || response.RemainingMatches != 0 {
		t.Fatalf("response = %+v", response)
	}
	redacted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(redacted), secret) {
		t.Fatal("source transcript still contains the secret")
	}

	// Open without syncing: this proves redact rebuilt SQLite/FTS rather than
	// relying on a later query to notice the changed transcript.
	db, err := index.Open(cfg.IndexPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	matches, err := db.Search(index.SearchOptions{Query: secret, ProseOnly: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("SQLite still contains %d secret matches", len(matches))
	}
}

func TestDoctorSecretsCheckIsOptInAndReadOnly(t *testing.T) {
	root := t.TempDir()
	secret := "doctor-secret-value"
	secretFile := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(secretFile, []byte("API_TOKEN="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf(
		`{"type":"user","uuid":"m1","sessionId":"session-a","timestamp":%q,"message":{"content":"api_key=%s"}}`+"\n",
		time.Now().UTC().Format(time.RFC3339Nano), secret)
	path := filepath.Join(root, "session-a.jsonl")
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{IndexPath: filepath.Join(t.TempDir(), "index.db"), TranscriptDirs: []string{root}}

	var defaultOut, defaultErr bytes.Buffer
	if code := Run([]string{"doctor"}, cfg, &defaultOut, &defaultErr); code != 0 {
		t.Fatalf("default doctor exit code = %d, stderr = %s", code, defaultErr.String())
	}

	var stdout, stderr bytes.Buffer
	code := Run([]string{"doctor", "--secrets", "--hours", "48", "--secret-file", secretFile}, cfg, &stdout, &stderr)
	if code == 0 {
		t.Fatal("doctor --secrets passed despite a finding")
	}
	var response output.DoctorResponse
	decodeValue(t, &stdout, &response)
	var found bool
	for _, check := range response.Checks {
		if check.Name == "secrets" {
			found = true
			if check.OK || !strings.Contains(check.Detail, "1") {
				t.Fatalf("secrets check = %+v", check)
			}
		}
	}
	if !found {
		t.Fatalf("checks = %+v, missing secrets check", response.Checks)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unchanged), secret) {
		t.Fatal("doctor modified the source transcript")
	}
}
