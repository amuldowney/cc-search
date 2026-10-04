package cli

import (
	"bytes"
	"testing"
)

func TestWriterOpenerRemainsWritableForMutationCommands(t *testing.T) {
	cfg := fixture(t, []msg{{"user", "writerneedle", 1}})
	db, err := openIndex(cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.WithLifecycleLock(func() error { _, err := db.Rebuild(cfg.TranscriptDirs[0], "session-a"); return err }); err != nil {
		t.Fatalf("writer opener returned a pinned read-only handle: %v", err)
	}
}
