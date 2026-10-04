package server

import (
	"errors"
	"net/http"
	"time"

	"github.com/amuldowney/cc-search/internal/index"
)

func (s *Server) beginOperation() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return apiError{status: http.StatusServiceUnavailable, code: "closed", message: "server is closing"}
	}
	s.operations.Add(1)
	return nil
}

func (s *Server) withDB(r *http.Request, fn func(*index.DB) error) (err error) {
	force, err := boolParam(r.URL.Query(), "refresh")
	if err != nil {
		return apiError{status: http.StatusBadRequest, code: "invalid_input", message: err.Error()}
	}
	if err := s.beginOperation(); err != nil {
		return err
	}
	defer s.operations.Done()
	db, err := index.OpenSnapshot(s.indexPath, s.transcriptDirs, force)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	status, err := db.Freshness(s.transcriptDirs)
	if err != nil {
		return err
	}
	if status.Stale && time.Since(time.UnixMilli(status.LastAttempt)) >= index.RefreshInterval {
		s.requestRefresh()
	}
	return fn(db)
}

func (s *Server) requestRefresh() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.refreshing {
		return
	}
	s.refreshing = true
	s.operations.Add(1)
	go func() {
		defer s.operations.Done()
		defer func() { s.mu.Lock(); s.refreshing = false; s.mu.Unlock() }()
		// Refresh records a sanitized failure while preserving lastSuccess.
		// Background=true skips a busy cross-process writer rather than queues.
		_, _ = index.Refresh(s.indexPath, s.transcriptDirs, index.RefreshOptions{Background: true})
	}()
}

func (s *Server) withWriter(fn func(*index.DB) error) (err error) {
	if err := s.beginOperation(); err != nil {
		return err
	}
	defer s.operations.Done()
	db, err := index.Open(s.indexPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	return fn(db)
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if err := s.beginOperation(); err != nil {
		writeDBError(w, err)
		return
	}
	defer s.operations.Done()
	stats, err := index.Refresh(s.indexPath, s.transcriptDirs, index.RefreshOptions{Force: true})
	if err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": 1, "sessionsIndexed": stats.SessionsIndexed, "messagesIndexed": stats.MessagesIndexed, "filesSkipped": stats.FilesSkipped})
}
