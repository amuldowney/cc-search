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
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andrewmuldowney/cc-search/internal/index"
	"github.com/andrewmuldowney/cc-search/internal/output"
	"github.com/andrewmuldowney/cc-search/internal/transcript"
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
	mux.HandleFunc("/v1/sessions", s.handleSessions)
	mux.HandleFunc("/v1/activities", s.handleActivities)
	mux.HandleFunc("/v1/activity", s.handleActivity)
	mux.HandleFunc("/v1/context", s.handleContext)
	mux.HandleFunc("/v1/commands", s.handleCommands)
	mux.HandleFunc("/v1/info", s.handleInfo)
	mux.HandleFunc("/v1/doctor", s.handleDoctor)
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

func (s *Server) handleSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	q := r.URL.Query()
	limit, err := nonNegativeInt(q, "limit", 20)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	any, err := boolParam(q, "any")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	var rows []index.SessionSummary
	err = s.withDB(func(db *index.DB) error {
		rows, err = db.Sessions(index.SessionOptions{Query: strings.TrimSpace(q.Get("pattern")), Limit: limit, CWD: q.Get("cwd"), Any: any})
		return err
	})
	if err != nil {
		writeDBError(w, err)
		return
	}
	if len(rows) == 0 && q.Get("pattern") != "" && !any && index.TermCount(q.Get("pattern")) > 1 {
		err = s.withDB(func(db *index.DB) error {
			rows, err = db.Sessions(index.SessionOptions{Query: q.Get("pattern"), Limit: limit, CWD: q.Get("cwd"), Any: true})
			return err
		})
		if err != nil {
			writeDBError(w, err)
			return
		}
	}
	response := output.SessionsResponse{Sessions: make([]output.Session, 0, len(rows)), Total: len(rows)}
	for _, row := range rows {
		response.Sessions = append(response.Sessions, output.Session{SessionID: row.SessionID, CWD: row.CWD,
			Visibility: row.Visibility, ParentSessionID: row.ParentSessionID, LastTimestamp: timestamp(row.LastTimestamp),
			LastPreview: compactPreview(row.LastPreview), MessageCount: row.MessageCount})
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) handleActivities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	q := r.URL.Query()
	limit, err := nonNegativeInt(q, "limit", 20)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	var rows []index.ActivityRecord
	err = s.withDB(func(db *index.DB) error {
		rows, err = db.Activities(index.ActivityOptions{SessionID: q.Get("session"), Status: q.Get("status"), Limit: limit})
		return err
	})
	if err != nil {
		writeDBError(w, err)
		return
	}
	activities := make([]output.Activity, 0, len(rows))
	full, err := boolParam(q, "full")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	for _, row := range rows {
		activities = append(activities, activityOutput(row, full))
	}
	writeJSON(w, http.StatusOK, output.ActivitiesResponse{Activities: activities, Total: len(activities)})
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
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
	var row index.ActivityRecord
	err := s.withDB(func(db *index.DB) error { var e error; row, e = db.Activity(id); return e })
	if err != nil {
		status, code := http.StatusInternalServerError, "internal_error"
		if errors.Is(err, index.ErrNoSuchActivity) {
			status, code = http.StatusNotFound, "not_found"
		}
		if errors.Is(err, index.ErrAmbiguousActivity) {
			status, code = http.StatusConflict, "ambiguous_id"
		}
		writeError(w, status, code, err.Error())
		return
	}
	full, err := boolParam(q, "full")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, activityOutput(row, full))
}

func (s *Server) handleContext(w http.ResponseWriter, r *http.Request) {
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
	hits, err := nonNegativeInt(q, "hits", 3)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	before, err := nonNegativeInt(q, "before", 3)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	after, err := nonNegativeInt(q, "after", 8)
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
	if any && raw {
		writeError(w, http.StatusBadRequest, "invalid_input", "any has no meaning for a raw query")
		return
	}
	options, err := renderOptions(q)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	var selected []transcript.Message
	relaxed := false
	err = s.withDB(func(db *index.DB) error {
		limit := hits
		if limit > 0 {
			limit++
		}
		exclude := ""
		if q.Get("session") == "" && !includeCurrent {
			exclude = currentSessionID()
		}
		search := index.SearchOptions{Query: pattern, Limit: limit, SessionID: q.Get("session"), ExcludeSessionID: exclude,
			Type: q.Get("type"), ProseOnly: !options.All, Any: any, Raw: raw}
		var e error
		selected, e = db.Search(search)
		if e != nil {
			return e
		}
		if len(selected) == 0 && !any && !raw && index.TermCount(pattern) > 1 {
			search.Any = true
			selected, e = db.Search(search)
			if e == nil {
				relaxed = len(selected) > 0
			}
		}
		return e
	})
	if err != nil {
		if errors.Is(err, index.ErrBadQuery) {
			writeError(w, http.StatusBadRequest, "invalid_query", err.Error())
		} else {
			writeDBError(w, err)
		}
		return
	}
	truncated := hits > 0 && len(selected) > hits
	if truncated {
		selected = selected[:hits]
	}
	var expanded []index.ContextMessage
	err = s.withDB(func(db *index.DB) error {
		var e error
		expanded, e = db.ExpandContext(selected, before, after, !options.All)
		return e
	})
	if err != nil {
		writeDBError(w, err)
		return
	}
	contextMessages := make([]transcript.Message, 0, len(expanded))
	for _, item := range expanded {
		contextMessages = append(contextMessages, item.Message)
	}
	options.Options.Truncated, options.Options.Relaxed = truncated, relaxed
	contextResponse := output.Format(contextMessages, options.Options)
	hitOptions := options.Options
	hitOptions.Budget = 0
	hitResponse := output.Format(selected, hitOptions)
	results := make([]output.ContextResult, 0, len(contextResponse.Results))
	for i, result := range contextResponse.Results {
		results = append(results, output.ContextResult{Result: result, HitIDs: expanded[i].HitIDs})
	}
	writeJSON(w, http.StatusOK, output.ContextResponse{Query: pattern, Hits: hitResponse.Results, Results: results,
		TotalHits: len(selected), Total: len(results), Truncated: contextResponse.Truncated, Relaxed: relaxed, Budget: contextResponse.Budget})
}

func (s *Server) handleCommands(w http.ResponseWriter, r *http.Request) {
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
	limit, err := nonNegativeInt(q, "limit", 20)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	full, err := boolParam(q, "full")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	includeOutput, err := boolParam(q, "include_output")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_input", err.Error())
		return
	}
	var rows []index.CommandRecord
	queryLimit := limit
	if queryLimit > 0 {
		queryLimit++
	}
	err = s.withDB(func(db *index.DB) error {
		var e error
		rows, e = db.Commands(index.CommandOptions{Query: pattern, Tool: q.Get("tool"), SessionID: q.Get("session"), Limit: queryLimit, IncludeOutput: full || includeOutput})
		return e
	})
	if err != nil {
		writeDBError(w, err)
		return
	}
	truncated := limit > 0 && len(rows) > limit
	if truncated {
		rows = rows[:limit]
	}
	commands := make([]output.Command, 0, len(rows))
	for _, row := range rows {
		commands = append(commands, output.Command{Tool: row.Tool, Arguments: commandArguments(row.Arguments), SessionID: row.SessionID, MessageID: row.MessageID, Timestamp: timestamp(row.Timestamp), Output: row.Output})
	}
	writeJSON(w, http.StatusOK, output.CommandsResponse{Commands: commands, Total: len(commands), Truncated: truncated})
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	var info output.InfoResponse
	if err := s.withDB(func(db *index.DB) error { var e error; info, e = makeInfo(db, false); return e }); err != nil {
		writeDBError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleDoctor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	var info output.InfoResponse
	if err := s.withDB(func(db *index.DB) error { var e error; info, e = makeInfo(db, true); return e }); err != nil {
		writeDBError(w, err)
		return
	}
	checks := []output.DoctorCheck{{Name: "index", OK: info.IndexHealthy, Detail: info.IndexPath}, {Name: "schema", OK: info.SchemaVersion == index.SchemaVersion, Detail: fmt.Sprintf("version %d", info.SchemaVersion)}, {Name: "lock", OK: info.LockHealthy, Detail: "lifecycle lock acquired"}}
	ok := true
	for _, check := range checks {
		ok = ok && check.OK
	}
	writeJSON(w, http.StatusOK, output.DoctorResponse{InfoResponse: info, OK: ok, Checks: checks})
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

func activityOutput(row index.ActivityRecord, full bool) output.Activity {
	activity := output.Activity{ActivityID: row.ActivityID, Status: row.Status, Title: row.Title, Model: row.Model,
		Effort: row.Effort, ToolUses: row.ToolUses, StartedAt: timestamp(row.StartedAt), CompletedAt: timestamp(row.CompletedAt),
		ParentSessionID: row.ParentSessionID, ChildSessionID: row.ChildSessionID, ParentActivityID: row.ParentActivityID, ResultSummary: row.ResultSummary}
	if full {
		activity.Description, activity.Kind, activity.Namespace = row.Description, row.Kind, row.Namespace
		activity.StartEntryID, activity.StartParentID = row.StartEntryID, row.StartParentID
		activity.LinkedEntryID, activity.TerminalEntryID, activity.TerminalParentID = row.LinkedEntryID, row.TerminalEntryID, row.TerminalParentID
	}
	return activity
}

func commandArguments(raw string) any {
	if raw == "" || raw == "null" {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err == nil {
		return value
	}
	return raw
}

func timestamp(milliseconds int64) string {
	if milliseconds == 0 {
		return ""
	}
	return time.UnixMilli(milliseconds).UTC().Format(time.RFC3339)
}

func compactPreview(content string) string {
	flat := strings.Join(strings.Fields(content), " ")
	runes := []rune(flat)
	if len(runes) > output.DefaultPreviewLength {
		return string(runes[:output.DefaultPreviewLength]) + "…"
	}
	return flat
}

func makeInfo(db *index.DB, verify bool) (output.InfoResponse, error) {
	stats, err := db.Info()
	if err != nil {
		return output.InfoResponse{}, err
	}
	if verify {
		stats.IndexHealthy = db.Check() == nil
	}
	binaryPath, _ := os.Executable()
	if resolved, resolveErr := filepath.EvalSymlinks(binaryPath); resolveErr == nil {
		binaryPath = resolved
	}
	binaryVersion := "dev"
	if build, ok := debug.ReadBuildInfo(); ok && build.Main.Version != "" && build.Main.Version != "(devel)" {
		binaryVersion = build.Main.Version
	}
	info := output.InfoResponse{IndexPath: stats.IndexPath, SchemaVersion: stats.SchemaVersion,
		MessageCount: stats.MessageCount, SessionCount: stats.SessionCount, ActivityCount: stats.ActivityCount,
		FileCount: stats.FileCount, Sources: make([]output.Source, 0, len(stats.Sources)), BinaryPath: binaryPath,
		BinaryVersion: binaryVersion, CurrentSession: currentSessionID(), IndexHealthy: stats.IndexHealthy, LockHealthy: true}
	for _, source := range stats.Sources {
		info.Sources = append(info.Sources, output.Source{Path: source.Path, MessageCount: source.MessageCount, SessionCount: source.SessionCount})
	}
	return info, nil
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
