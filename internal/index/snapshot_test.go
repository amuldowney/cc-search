package index

import (
	"path/filepath"
	"testing"
	"time"
)

func TestOpenUsesWAL(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.sql.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode=%q, want wal for concurrent snapshots", mode)
	}
}

func TestOpenExistingSchemaDoesNotWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	writer, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	tx, err := writer.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO files(path,mtime,size) VALUES('held',1,1)`); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		db, err := open(path)
		if err == nil {
			err = db.Close()
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		tx.Rollback()
		<-done
		t.Fatal("opening an existing schema waited on a writer: read/open path must not rewrite user_version")
	}
}
