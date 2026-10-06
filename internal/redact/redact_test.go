package redact

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestSecretQueryReplacesOnlyValue(t *testing.T) {
	input := "https://example.invalid/?token=%2F%2F%2F%2F&keep=yes"
	want := "https://example.invalid/?token=<REDACTED>&keep=yes"

	counts := &matchCounts{}
	got := scrubString(input, nil, counts)
	if got != want {
		t.Fatalf("scrubString() = %q, want %q", got, want)
	}
	if counts.total() != 1 {
		t.Fatalf("match count = %d, want 1", counts.total())
	}

	remaining := &matchCounts{}
	if gotAgain := scrubString(got, nil, remaining); gotAgain != want || remaining.total() != 0 {
		t.Fatalf("second scrub = %q with %d matches, want unchanged output and no matches", gotAgain, remaining.total())
	}
}

func TestScanAndRedactRecentRecordsWithoutTouchingOpaqueDataOrOldRecords(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl")
	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour).Format(time.RFC3339Nano)
	recent := now.Add(-time.Minute).Format(time.RFC3339Nano)
	imageToken := "sk-abcdefghijklmnopqrstuvwxyz"
	first, err := json.Marshal(map[string]any{
		"type": "user", "timestamp": old,
		"message": map[string]any{"content": "old token=old-secret-value"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(map[string]any{
		"type": "user", "timestamp": recent,
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "api_key=super-secret-value"},
				map[string]any{"type": "image", "data": imageToken, "mimeType": "image/png"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append(first, '\n'), append(second, '\n')...), 0o600); err != nil {
		t.Fatal(err)
	}

	opts := Options{Since: now.Add(-48 * time.Hour), SecretValues: []string{"super-secret-value"}}
	report, err := Scan([]string{root}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if report.FilesScanned != 1 || report.RecordsScanned != 2 || report.Matches == 0 || len(report.Findings) != 1 {
		t.Fatalf("scan report = %+v, want one recent finding", report)
	}

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if dry, err := RedactFile(path, root, opts, false); err != nil || dry.Matches == 0 {
		t.Fatalf("dry run = %+v, err = %v", dry, err)
	}
	afterDryRun, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterDryRun) != string(before) {
		t.Fatal("dry run modified transcript")
	}

	if _, err := RedactFile(path, root, opts, true); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(after)
	if strings.Contains(text, "super-secret-value") {
		t.Fatal("recent secret remains in transcript")
	}
	if !strings.Contains(text, "old-secret-value") {
		t.Fatal("record outside the time window was changed")
	}
	if !strings.Contains(text, imageToken) {
		t.Fatal("opaque image data was changed")
	}
	if report, err := Scan([]string{root}, opts); err != nil || report.Matches != 0 {
		t.Fatalf("post-redaction scan = %+v, err = %v", report, err)
	}
}

func TestRedactCompressedTranscript(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "session.jsonl.zst")
	line := []byte(`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"content":"api_key=compressed-secret-value"}}` + "\n")
	var compressed bytes.Buffer
	encoder, err := zstd.NewWriter(&compressed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Write(line); err != nil {
		t.Fatal(err)
	}
	if err := encoder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, compressed.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := Scan([]string{root}, Options{Since: time.Time{}, SecretValues: []string{"compressed-secret-value"}})
	if err != nil || report.Matches == 0 {
		t.Fatalf("scan report = %+v, err = %v", report, err)
	}
	if _, err := RedactFile(path, root, Options{Since: time.Time{}, SecretValues: []string{"compressed-secret-value"}}, true); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	decompressor, err := zstd.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(decompressor)
	decompressor.Close()
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(decoded, []byte("compressed-secret-value")) {
		t.Fatal("compressed transcript still contains the secret")
	}
}

func TestSecretValuesFromFilesReadsCredentialNamedDotenvValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.env")
	if err := os.WriteFile(path, []byte("NORMAL=value\nAPI_TOKEN='file-secret-value'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	values, err := SecretValuesFromFiles([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0] != "file-secret-value" {
		t.Fatalf("values = %q, want only credential value", values)
	}
}
