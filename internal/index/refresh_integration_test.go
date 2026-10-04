package index

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestRefreshSupportsReservedIndexPath(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "session", []msg{{"user", "reservedneedle", 1}})
	path := filepath.Join(t.TempDir(), "index ?#+.db")
	if _, err := Refresh(path, []string{root}, RefreshOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReader(path)
	if err != nil {
		t.Fatalf("writer and reader disagree about the requested filename: %v", err)
	}
	defer reader.Close()
	hits, err := reader.Search(SearchOptions{Query: "reservedneedle"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("got %d hits from reserved path", len(hits))
	}
}

func TestReaderRejectsIncompleteCurrentReadModelWithoutRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.sql.Exec(`DROP TABLE message_projections`); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	reader, err := OpenReader(path)
	if reader != nil {
		reader.Close()
	}
	if err == nil || errors.Is(err, ErrNeedsRefresh) {
		t.Fatalf("incomplete current schema must fail clearly, not silently serve or treat sync as a repair: %v", err)
	}
}
