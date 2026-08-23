// Package server exposes cc-search through a local, OpenAPI-described HTTP API.
package server

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/andrewmuldowney/cc-search/internal/index"
	"github.com/andrewmuldowney/cc-search/internal/output"
)

// Config supplies the derived-data index and transcript roots used by the API.
type Config struct {
	IndexPath      string
	TranscriptDirs []string
}

// Server owns an index and serves requests against it. Operations are
// serialized so a request cannot query while a transcript replacement or
// rebuild is in progress.
type Server struct {
	db             *index.DB
	transcriptDirs []string
	mu             sync.Mutex
	closeOnce      sync.Once
	closeErr       error
}

//go:embed openapi.json
var openAPIDocument []byte

// New opens the index and performs an initial synchronization.
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.IndexPath) == "" {
		return nil, errors.New("index path is required")
	}
	db, err := index.Open(cfg.IndexPath)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Server, error) {
		return nil, errors.Join(err, db.Close())
	}
	for _, dir := range cfg.TranscriptDirs {
		if _, err := db.Sync(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fail(err)
		}
	}
	if err := db.ReleaseLifecycleLock(); err != nil {
		return fail(err)
	}
	return &Server{db: db, transcriptDirs: append([]string(nil), cfg.TranscriptDirs...)}, nil
}

// Close closes the index. It is safe to call more than once.
func (s *Server) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

// Handler returns the HTTP handler for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/openapi.json", s.handleOpenAPI)
	mux.HandleFunc("/v1/health", s.handleHealth)
	mux.HandleFunc("/v1/search", s.handleSearch)
	mux.HandleFunc("/v1/last", s.handleLast)
	mux.HandleFunc("/v1/read", s.handleRead)
	mux.HandleFunc("/v1/rebuild", s.handleRebuild)
	return mux
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(openAPIDocument)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": 1, "status": "ok"})
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	q := r.URL.Query()
	pattern := strings.TrimSpace(q.Get("pattern"))
	if pattern == "" {
		writeError(w, http.StatusBadRequest, "invalid_input", "pattern is required")
		return
	}
	limit, err := nonNegativeInt(q, "limit", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	windowMessages, err := nonNegativeInt(q, "window_messages", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	windowHours, err := nonNegativeInt(q, "window_hours", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	any, err := boolParam(q, "any")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	raw, err := boolParam(q, "raw")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	includeCurrent, err := boolParam(q, "include_current")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	if raw && any {
		writeError(w, http.StatusBadRequest, "invalid_input", "any has no meaning for a raw query")
		return
	}
	options, err := renderOptions(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}

	if err := s.withDB(func(db *index.DB) error {
		queryLimit := limit
		if queryLimit > 0 {
			queryLimit++
		}
		excludeSession := ""
		if q.Get("session") == "" && !includeCurrent {
			excludeSession = currentSessionID()
		}
		search := index.SearchOptions{
			Query:            pattern,
			Limit:            queryLimit,
			WindowMessages:   windowMessages,
			WindowHours:      windowHours,
			SessionID:        q.Get("session"),
			ExcludeSessionID: excludeSession,
			Type:             q.Get("type"),
			ProseOnly:        !options.All,
			Any:              any,
			Raw:              raw,
		}
		msgs, err := db.Search(search)
		if err != nil {
			if errors.Is(err, index.ErrBadQuery) {
				return apiError{status: http.StatusBadRequest, code: "invalid_query", message: err.Error()}
			}
			return err
		}
		relaxed := false
		if len(msgs) == 0 && !any && !raw && index.TermCount(pattern) > 1 {
			search.Any = true
			msgs, err = db.Search(search)
			if err != nil {
				return err
			}
			relaxed = len(msgs) > 0
		}
		truncated := limit > 0 && len(msgs) > limit
		if truncated {
			msgs = msgs[:limit]
		}
		resultOptions := options.Options
		resultOptions.Truncated = truncated
		resultOptions.Relaxed = relaxed
		writeJSON(w, http.StatusOK, output.Format(msgs, resultOptions))
		return nil
	}); err != nil {
		writeDBError(w, err)
	}
}

func (s *Server) handleLast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	q := r.URL.Query()
	count, err := nonNegativeInt(q, "count", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	hours, err := nonNegativeInt(q, "hours", 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	if q.Get("hours") == "" && q.Get("count") == "" {
		count = 10
	}
	options, err := renderOptions(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	if err := s.withDB(func(db *index.DB) error {
		msgs, err := db.Last(index.LastOptions{
			N:         count,
			Hours:     hours,
			SessionID: q.Get("session"),
			Type:      q.Get("type"),
			ProseOnly: !options.All,
		})
		if err != nil {
			return err
		}
		writeJSON(w, http.StatusOK, output.Format(msgs, options.Options))
		return nil
	}); err != nil {
		writeDBError(w, err)
	}
}

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "invalid_input", "id is required")
		return
	}
	before, err := nonNegativeInt(q, "before", 5)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	after, err := nonNegativeInt(q, "after", 5)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	options, err := renderOptions(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	if err := s.withDB(func(db *index.DB) error {
		msgs, err := db.Around(id, before, after, !options.All)
		if err != nil {
			status := http.StatusInternalServerError
			code := "internal_error"
			if errors.Is(err, index.ErrNoSuchID) {
				status, code = http.StatusNotFound, "not_found"
			} else if errors.Is(err, index.ErrAmbiguousID) {
				status, code = http.StatusConflict, "ambiguous_id"
			}
			return apiError{status: status, code: code, message: err.Error()}
		}
		writeJSON(w, http.StatusOK, output.Format(msgs, options.Options))
		return nil
	}); err != nil {
		writeDBError(w, err)
	}
}

func (s *Server) handleRebuild(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	session := r.URL.Query().Get("session")
	var stats index.SyncStats
	if err := s.withDB(func(db *index.DB) error {
		for _, dir := range s.transcriptDirs {
			current, err := db.Rebuild(dir, session)
			if err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			stats.SessionsIndexed += current.SessionsIndexed
			stats.MessagesIndexed += current.MessagesIndexed
			stats.FilesSkipped += current.FilesSkipped
		}
		return nil
	}); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":         1,
		"sessionsIndexed": stats.SessionsIndexed,
		"messagesIndexed": stats.MessagesIndexed,
		"filesSkipped":    stats.FilesSkipped,
	})
}

// outputOptions keeps HTTP query names separate from the CLI flag parser.
type outputOptions struct {
	Options output.Options
	All     bool
}

func renderOptions(q url.Values) (outputOptions, error) {
	previewLength, err := nonNegativeInt(q, "preview_length", output.DefaultPreviewLength)
	if err != nil {
		return outputOptions{}, err
	}
	budget, err := nonNegativeInt(q, "budget", 60000)
	if err != nil {
		return outputOptions{}, err
	}
	full, err := boolParam(q, "full")
	if err != nil {
		return outputOptions{}, err
	}
	all, err := boolParam(q, "all")
	if err != nil {
		return outputOptions{}, err
	}
	return outputOptions{
		All: all,
		Options: output.Options{
			PreviewLength: previewLength,
			Full:          full,
			UseProse:      !all,
			Budget:        budget,
		},
	}, nil
}

func (s *Server) withDB(fn func(*index.DB) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, dir := range s.transcriptDirs {
		if _, err := s.db.Sync(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return fn(s.db)
}

func nonNegativeInt(q url.Values, name string, defaultValue int) (int, error) {
	raw := q.Get(name)
	if raw == "" {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}

func boolParam(q url.Values, name string) (bool, error) {
	raw := q.Get(name)
	if raw == "" {
		return false, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return value, nil
}

func currentSessionID() string {
	if id := strings.TrimSpace(os.Getenv("PI_SESSION_ID")); id != "" {
		return id
	}
	path := strings.TrimSpace(os.Getenv("PI_SESSION_FILE"))
	if path == "" {
		return ""
	}
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if separator := strings.LastIndex(name, "_"); separator >= 0 {
		return name[separator+1:]
	}
	return name
}

type apiError struct {
	status  int
	code    string
	message string
}

func (e apiError) Error() string { return e.message }

func writeDBError(w http.ResponseWriter, err error) {
	var apiErr apiError
	if errors.As(err, &apiErr) {
		writeError(w, apiErr.status, apiErr.code, apiErr.message)
		return
	}
	writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"version": 1, "error": message, "code": code})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
