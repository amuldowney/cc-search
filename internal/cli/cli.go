// Package cli wires the cc-search subcommands to the index.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/amuldowney/cc-search/internal/index"
	"github.com/amuldowney/cc-search/internal/output"
	"github.com/amuldowney/cc-search/internal/server"
	"github.com/amuldowney/cc-search/internal/transcript"
)

// DefaultTranscriptDir is where Claude Code keeps transcripts, one folder per
// project. It is walked recursively, so every project is indexed.
const DefaultTranscriptDir = ".claude/projects"

// DefaultPiSessionsDir is where pi keeps session transcripts, one folder per
// working directory. It is indexed alongside the Claude Code transcripts.
const DefaultPiSessionsDir = ".pi/agent/sessions"

// DefaultIndexPath is where the search index lives.
const DefaultIndexPath = ".claude/search-index.db"

// defaultLastCount is how many messages `last` returns without an argument.
const defaultLastCount = 10

// defaultReadContext is how many messages `read` shows on each side.
const defaultReadContext = 5

// DefaultBudget caps output characters when no --budget is given. It is large
// enough that ordinary queries never reach it, and small enough that --full on
// a huge tool result cannot flood a context window.
const DefaultBudget = 60000

const usage = `usage: cc-search <command> [options]

commands:
  last [N] [--hours H] [--session ID] [--type TYPE]
                                        most recent messages (default N=10)
  search PATTERN [options]              full-text search across transcripts
  read ID [--before N] [--after N]      one message plus its neighbours
  sessions [QUERY] [options]             browse sessions to resume
  activities [options]                   inspect subagent activity records
  activity ID [--full]                  inspect one subagent activity
  context PATTERN [options]             search hits with surrounding context
  commands PATTERN [options]            retrieve exact historical tool calls
  info                                  show index and installation details
  doctor                                check index and installation health
  rebuild [--session ID]                discard and rebuild the index
  serve [--host 127.0.0.1] [--port N]  serve the local OpenAPI HTTP API
       [--index PATH] [--transcripts DIR]

search options:
  --limit N             maximum results returned
  --window-messages M   only search the newest M messages
  --window-hours H      only search the last H hours
  --session ID          restrict to one session
  --include-current     include the current session in search results
  --type TYPE           restrict to user, assistant, system, ...
  --any                 match any term (default: every term must appear)
  --raw                 treat the pattern as an FTS5 boolean expression
                        (OR, NOT, NEAR, parens, "quoted phrases")

read options:
  --before N            messages of context before it (default 5)
  --after N             messages of context after it (default 5)
  ID may be any unique prefix of a message id, as returned by search.

sessions options:
  --limit N             maximum sessions returned (default 20)
  --cwd PATH            exact working-directory filter
  --any                match any query term instead of all terms

activities options:
  --session ID          parent or child session
  --status STATUS       running, completed, failed, ...
  --limit N             maximum activities returned (default 20)
  --full                include diagnostic linkage fields

context options:
  --hits N              number of search hits to expand (default 3)
  --before N            messages before each hit (default 3)
  --after N             messages after each hit (default 8)

commands options:
  --tool NAME           restrict to one tool (for example Bash)
  --session ID          restrict to one session
  --limit N             maximum commands returned (default 20; 0 = unlimited)
  --full                include paired tool output
  --include-output      include paired tool output (same as --full)

output options (last, search, read and context):
  --all                 include tool calls and their output (default: only
                        what was actually said)
  --preview-length N    preview size in characters (default 100)
  --full                include the complete message content
  --budget N            cap total output characters (default 60000, 0 = off)
  --index PATH          index database to use
  --transcripts DIR     transcript directory to index
`

// Config supplies the paths the commands operate on. Empty fields fall back to
// the user's default locations.
type Config struct {
	IndexPath      string
	TranscriptDirs []string
}

func (c Config) withDefaults() Config {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	if c.IndexPath == "" {
		c.IndexPath = filepath.Join(home, DefaultIndexPath)
	}
	if len(c.TranscriptDirs) == 0 {
		c.TranscriptDirs = []string{
			filepath.Join(home, DefaultTranscriptDir),
			filepath.Join(home, DefaultPiSessionsDir),
		}
	}
	return c
}

// Run executes one cc-search invocation and returns the process exit code.
func Run(args []string, cfg Config, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	command, rest := args[0], args[1:]
	var err error
	switch command {
	case "last":
		err = runLast(rest, cfg, stdout, stderr)
	case "search":
		err = runSearch(rest, cfg, stdout, stderr)
	case "read":
		err = runRead(rest, cfg, stdout, stderr)
	case "sessions":
		err = runSessions(rest, cfg, stdout, stderr)
	case "activities":
		err = runActivities(rest, cfg, stdout, stderr)
	case "activity":
		err = runActivity(rest, cfg, stdout, stderr)
	case "context":
		err = runContext(rest, cfg, stdout, stderr)
	case "commands":
		err = runCommands(rest, cfg, stdout, stderr)
	case "info":
		err = runInfo(rest, cfg, stdout, stderr)
	case "doctor":
		err = runDoctor(rest, cfg, stdout, stderr)
	case "rebuild":
		err = runRebuild(rest, cfg, stdout, stderr)
	case "serve":
		err = runServe(rest, cfg, stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "cc-search: unknown command %q\n\n%s", command, usage)
		return 2
	}

	if err != nil {
		fmt.Fprintf(stderr, "cc-search: %v\n", err)
		if errors.Is(err, errUsage) || errors.Is(err, flag.ErrHelp) {
			return 2
		}
		return 1
	}
	return 0
}

// errUsage marks errors caused by how the command was invoked, which exit 2.
var errUsage = errors.New("usage")

var closeIndex = func(db *index.DB) error {
	return db.Close()
}

func closeIndexWithError(db *index.DB, operationErr error) error {
	return errors.Join(operationErr, closeIndex(db))
}

// commonFlags are shared by every command that reads or writes the index.
type commonFlags struct {
	set           *flag.FlagSet
	session       string
	previewLength int
	full          bool
	all           bool
	budget        int
	indexPath     string
	transcriptDir string
}

func newFlagSet(name string, stderr io.Writer) *commonFlags {
	set := flag.NewFlagSet(name, flag.ContinueOnError)
	set.SetOutput(stderr)
	f := &commonFlags{set: set}
	set.StringVar(&f.session, "session", "", "restrict to one session")
	set.IntVar(&f.previewLength, "preview-length", output.DefaultPreviewLength, "preview size in characters")
	set.BoolVar(&f.full, "full", false, "include the complete message content")
	set.StringVar(&f.indexPath, "index", "", "index database to use")
	set.StringVar(&f.transcriptDir, "transcripts", "", "transcript directory to index")
	set.BoolVar(&f.all, "all", false, "include tool calls and their output")
	set.IntVar(&f.budget, "budget", DefaultBudget, "cap total output characters (0 = unlimited)")
	return f
}

// resolve merges path overrides from flags into the config.
func (f *commonFlags) resolve(cfg Config) Config {
	if f.indexPath != "" {
		cfg.IndexPath = f.indexPath
	}
	if f.transcriptDir != "" {
		cfg.TranscriptDirs = []string{f.transcriptDir}
	}
	return cfg.withDefaults()
}

func (f *commonFlags) outputOptions(truncated bool) output.Options {
	return output.Options{
		PreviewLength: f.previewLength,
		Full:          f.full,
		Truncated:     truncated,
		UseProse:      !f.all,
		Budget:        f.budget,
	}
}

// openIndex opens the index and brings it up to date with the transcripts on
// disk. A missing or unreadable transcript directory is a warning, not an
// error — a previously built index can still answer queries.
func openIndex(cfg Config, stderr io.Writer) (*index.DB, error) {
	db, err := index.Open(cfg.IndexPath)
	if err != nil {
		return nil, err
	}
	for _, dir := range cfg.TranscriptDirs {
		if _, err := db.Sync(dir); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintf(stderr, "cc-search: warning: %v\n", err)
			} else {
				return nil, closeIndexWithError(db, err)
			}
		}
	}
	if err := db.ReleaseLifecycleLock(); err != nil {
		return nil, closeIndexWithError(db, err)
	}
	return db, nil
}

func runLast(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	f := newFlagSet("last", stderr)
	hours := f.set.Int("hours", 0, "only messages from the last H hours")
	msgType := f.set.String("type", "", "restrict to a message type")

	// A bare count may precede the flags: `cc-search last 10 --session X`.
	count := 0
	if len(args) > 0 {
		if n, err := strconv.Atoi(args[0]); err == nil {
			count, args = n, args[1:]
		}
	}
	if err := f.set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, f.set.Arg(0))
	}
	if count == 0 && *hours == 0 {
		count = defaultLastCount
	}

	cfg = f.resolve(cfg)
	db, err := openIndex(cfg, stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeIndex(db)) }()

	msgs, err := db.Last(index.LastOptions{
		N: count, Hours: *hours, SessionID: f.session, Type: *msgType, ProseOnly: !f.all})
	if err != nil {
		return err
	}
	return emit(stdout, stderr, msgs, f.outputOptions(false))
}

func runSearch(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	f := newFlagSet("search", stderr)
	limit := f.set.Int("limit", 0, "maximum results returned")
	windowMessages := f.set.Int("window-messages", 0, "only search the newest M messages")
	windowHours := f.set.Int("window-hours", 0, "only search the last H hours")
	msgType := f.set.String("type", "", "restrict to a message type")
	any := f.set.Bool("any", false, "match any term rather than all of them")
	raw := f.set.Bool("raw", false, "pass the pattern to FTS5 as a boolean expression")
	includeCurrent := f.set.Bool("include-current", false, "include the current session in search results")

	if len(args) == 0 {
		return fmt.Errorf("%w: search needs a pattern", errUsage)
	}
	if strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("%w: the search pattern must come before the flags, "+
			"got %q in the pattern position", errUsage, args[0])
	}
	pattern, args := args[0], args[1:]
	if err := f.set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q — quote multi-word patterns",
			errUsage, f.set.Arg(0))
	}

	if *raw && *any {
		return fmt.Errorf("%w: --any has no meaning for a --raw query; write OR yourself", errUsage)
	}

	cfg = f.resolve(cfg)
	db, err := openIndex(cfg, stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeIndex(db)) }()

	// Ask for one more than requested so we can tell whether the limit cut
	// results off, then drop the extra before rendering.
	queryLimit := *limit
	if queryLimit > 0 {
		queryLimit++
	}

	excludeSession := ""
	if f.session == "" && !*includeCurrent {
		excludeSession = currentSessionID()
	}
	search := index.SearchOptions{
		Query:            pattern,
		Limit:            queryLimit,
		WindowMessages:   *windowMessages,
		WindowHours:      *windowHours,
		SessionID:        f.session,
		ExcludeSessionID: excludeSession,
		Type:             *msgType,
		ProseOnly:        !f.all,
		Any:              *any,
		Raw:              *raw,
	}
	msgs, err := db.Search(search)
	if err != nil {
		if errors.Is(err, index.ErrBadQuery) {
			return fmt.Errorf("%w: %w\n  quote punctuation in a raw query, "+
				`e.g. "display.cpp" NOT proxy`, errUsage, err)
		}
		return err
	}

	// A multi-term query that matches nothing is usually over-constrained
	// rather than genuinely absent, so fall back to matching any term and say
	// so rather than reporting the topic was never discussed.
	relaxed := false
	// A raw query says exactly what was meant, so it is never rewritten.
	if len(msgs) == 0 && !*any && !*raw && index.TermCount(pattern) > 1 {
		search.Any = true
		if msgs, err = db.Search(search); err != nil {
			return err
		}
		relaxed = len(msgs) > 0
	}

	truncated := *limit > 0 && len(msgs) > *limit
	if truncated {
		msgs = msgs[:*limit]
	}
	if relaxed {
		fmt.Fprintf(stderr, "cc-search: no message contained every term; relaxed to any "+
			"term (%d results). These are related, not exact.\n", len(msgs))
	}

	opts := f.outputOptions(truncated)
	opts.Relaxed = relaxed
	return emit(stdout, stderr, msgs, opts)
}

const (
	defaultSessionLimit  = 20
	defaultActivityLimit = 20
	defaultContextHits   = 3
	defaultCommandLimit  = 20
)

func runSessions(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	f := newFlagSet("sessions", stderr)
	limit := f.set.Int("limit", defaultSessionLimit, "maximum sessions returned")
	cwd := f.set.String("cwd", "", "exact working-directory filter")
	any := f.set.Bool("any", false, "match any query term")
	pattern := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pattern, args = args[0], args[1:]
	}
	if err := f.set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, f.set.Arg(0))
	}
	if *limit < 0 {
		return fmt.Errorf("%w: --limit must be non-negative", errUsage)
	}
	cfg = f.resolve(cfg)
	db, err := openIndex(cfg, stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeIndex(db)) }()

	rows, err := db.Sessions(index.SessionOptions{Query: pattern, Limit: *limit, CWD: *cwd, Any: *any})
	if err != nil {
		return err
	}
	if len(rows) == 0 && pattern != "" && !*any && index.TermCount(pattern) > 1 {
		rows, err = db.Sessions(index.SessionOptions{Query: pattern, Limit: *limit, CWD: *cwd, Any: true})
		if err != nil {
			return err
		}
	}
	response := output.SessionsResponse{Sessions: make([]output.Session, 0, len(rows)), Total: len(rows)}
	for _, row := range rows {
		response.Sessions = append(response.Sessions, output.Session{
			SessionID: row.SessionID, CWD: row.CWD, Visibility: row.Visibility,
			ParentSessionID: row.ParentSessionID, LastTimestamp: formatTimestamp(row.LastTimestamp),
			LastPreview: compactPreview(row.LastPreview), MessageCount: row.MessageCount,
		})
	}
	return writeJSONLine(stdout, response)
}

func runActivities(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	f := newFlagSet("activities", stderr)
	status := f.set.String("status", "", "activity status")
	limit := f.set.Int("limit", defaultActivityLimit, "maximum activities returned")
	if err := f.set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, f.set.Arg(0))
	}
	if *limit < 0 {
		return fmt.Errorf("%w: --limit must be non-negative", errUsage)
	}
	cfg = f.resolve(cfg)
	db, err := openIndex(cfg, stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeIndex(db)) }()
	rows, err := db.Activities(index.ActivityOptions{SessionID: f.session, Status: *status, Limit: *limit})
	if err != nil {
		return err
	}
	activities := make([]output.Activity, 0, len(rows))
	for _, row := range rows {
		activities = append(activities, activityOutput(row, f.full))
	}
	return writeJSONLine(stdout, output.ActivitiesResponse{Activities: activities, Total: len(activities)})
}

func runActivity(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	f := newFlagSet("activity", stderr)
	if len(args) == 0 {
		return fmt.Errorf("%w: activity needs an activity id", errUsage)
	}
	if strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("%w: the activity id must come before the flags, got %q", errUsage, args[0])
	}
	id, args := args[0], args[1:]
	if err := f.set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, f.set.Arg(0))
	}
	cfg = f.resolve(cfg)
	db, err := openIndex(cfg, stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeIndex(db)) }()
	row, err := db.Activity(id)
	if err != nil {
		return err
	}
	return writeJSONLine(stdout, activityOutput(row, f.full))
}

func runContext(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	f := newFlagSet("context", stderr)
	hits := f.set.Int("hits", defaultContextHits, "number of search hits to expand")
	before := f.set.Int("before", 3, "messages before each hit")
	after := f.set.Int("after", 8, "messages after each hit")
	any := f.set.Bool("any", false, "match any term")
	raw := f.set.Bool("raw", false, "pass the pattern to FTS5 as a boolean expression")
	msgType := f.set.String("type", "", "restrict to a message type")
	includeCurrent := f.set.Bool("include-current", false, "include the current session")
	if len(args) == 0 {
		return fmt.Errorf("%w: context needs a pattern", errUsage)
	}
	if strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("%w: the context pattern must come before the flags, got %q", errUsage, args[0])
	}
	pattern, args := args[0], args[1:]
	if err := f.set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, f.set.Arg(0))
	}
	if *hits < 0 || *before < 0 || *after < 0 {
		return fmt.Errorf("%w: --hits, --before and --after must be non-negative", errUsage)
	}
	if *raw && *any {
		return fmt.Errorf("%w: --any has no meaning for a --raw query; write OR yourself", errUsage)
	}
	cfg = f.resolve(cfg)
	db, err := openIndex(cfg, stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeIndex(db)) }()

	queryLimit := *hits
	if queryLimit > 0 {
		queryLimit++
	}
	excludeSession := ""
	if f.session == "" && !*includeCurrent {
		excludeSession = currentSessionID()
	}
	search := index.SearchOptions{Query: pattern, Limit: queryLimit, SessionID: f.session,
		ExcludeSessionID: excludeSession, Type: *msgType,
		ProseOnly: !f.all, Any: *any, Raw: *raw}
	selected, err := db.Search(search)
	if err != nil {
		if errors.Is(err, index.ErrBadQuery) {
			return fmt.Errorf("%w: %w", errUsage, err)
		}
		return err
	}
	relaxed := false
	if len(selected) == 0 && !*any && !*raw && index.TermCount(pattern) > 1 {
		search.Any = true
		selected, err = db.Search(search)
		if err != nil {
			return err
		}
		relaxed = len(selected) > 0
	}
	truncated := *hits > 0 && len(selected) > *hits
	if truncated {
		selected = selected[:*hits]
	}
	if relaxed {
		fmt.Fprintf(stderr, "cc-search: no message contained every term; relaxed to any term (%d results). These are related, not exact.\n", len(selected))
	}

	expanded, err := db.ExpandContext(selected, *before, *after, !f.all)
	if err != nil {
		return err
	}
	contextMessages := make([]transcript.Message, 0, len(expanded))
	for _, item := range expanded {
		contextMessages = append(contextMessages, item.Message)
	}
	contextOptions := f.outputOptions(truncated)
	contextOptions.Relaxed = relaxed
	contextResponse := output.Format(contextMessages, contextOptions)
	hitOptions := f.outputOptions(truncated)
	hitOptions.Budget = 0
	hitResponse := output.Format(selected, hitOptions)
	results := make([]output.ContextResult, 0, len(contextResponse.Results))
	for i, result := range contextResponse.Results {
		results = append(results, output.ContextResult{Result: result, HitIDs: expanded[i].HitIDs})
	}
	response := output.ContextResponse{Query: pattern, Hits: hitResponse.Results, Results: results,
		TotalHits: len(selected), Total: len(results), Truncated: contextResponse.Truncated,
		Relaxed: relaxed, Budget: contextResponse.Budget}
	if response.Budget.Dropped > 0 {
		fmt.Fprintf(stderr, "cc-search: warning: the %d character budget dropped %d context messages (%d spent); raise or disable it with --budget\n", response.Budget.Limit, response.Budget.Dropped, response.Budget.Spent)
	} else if response.Budget.Shrunk {
		fmt.Fprintf(stderr, "cc-search: warning: context previews were shortened to fit the %d character budget (%d spent); raise or disable it with --budget\n", response.Budget.Limit, response.Budget.Spent)
	}
	return writeJSONLine(stdout, response)
}

func runCommands(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	f := newFlagSet("commands", stderr)
	tool := f.set.String("tool", "", "restrict to one tool")
	limit := f.set.Int("limit", defaultCommandLimit, "maximum commands returned (0 = unlimited)")
	includeOutput := f.set.Bool("include-output", false, "include paired tool output")
	outputFlag := f.set.Bool("output", false, "include paired tool output (alias)")
	if len(args) == 0 {
		return fmt.Errorf("%w: commands needs a pattern", errUsage)
	}
	if strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("%w: the command pattern must come before the flags, got %q", errUsage, args[0])
	}
	pattern, args := args[0], args[1:]
	if err := f.set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, f.set.Arg(0))
	}
	if *limit < 0 {
		return fmt.Errorf("%w: --limit must be non-negative", errUsage)
	}
	cfg = f.resolve(cfg)
	db, err := openIndex(cfg, stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeIndex(db)) }()
	queryLimit := *limit
	if queryLimit > 0 {
		queryLimit++
	}
	rows, err := db.Commands(index.CommandOptions{Query: pattern, Tool: *tool, SessionID: f.session, Limit: queryLimit, IncludeOutput: f.full || *includeOutput || *outputFlag})
	if err != nil {
		return err
	}
	truncated := *limit > 0 && len(rows) > *limit
	if truncated {
		rows = rows[:*limit]
	}
	commands := make([]output.Command, 0, len(rows))
	for _, row := range rows {
		commands = append(commands, output.Command{Tool: row.Tool, Arguments: commandArguments(row.Arguments), SessionID: row.SessionID, MessageID: row.MessageID, Timestamp: formatTimestamp(row.Timestamp), Output: row.Output})
	}
	return writeJSONLine(stdout, output.CommandsResponse{Commands: commands, Total: len(commands), Truncated: truncated})
}

func runInfo(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	f := newFlagSet("info", stderr)
	if err := f.set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, f.set.Arg(0))
	}
	cfg = f.resolve(cfg)
	info, err := diagnosticInfo(cfg, stderr, false)
	if err != nil {
		return err
	}
	return writeJSONLine(stdout, info)
}

func runDoctor(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	f := newFlagSet("doctor", stderr)
	if err := f.set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, f.set.Arg(0))
	}
	cfg = f.resolve(cfg)
	info, err := diagnosticInfo(cfg, stderr, true)
	if err != nil {
		return err
	}
	checks := []output.DoctorCheck{
		{Name: "index", OK: info.IndexHealthy, Detail: info.IndexPath},
		{Name: "schema", OK: info.SchemaVersion == index.SchemaVersion, Detail: fmt.Sprintf("version %d", info.SchemaVersion)},
		{Name: "lock", OK: info.LockHealthy, Detail: "lifecycle lock acquired"},
	}
	ok := true
	for _, check := range checks {
		ok = ok && check.OK
	}
	if err := writeJSONLine(stdout, output.DoctorResponse{InfoResponse: info, OK: ok, Checks: checks}); err != nil {
		return err
	}
	if !ok {
		return errors.New("doctor found an unhealthy index")
	}
	return nil
}

func diagnosticInfo(cfg Config, stderr io.Writer, verify bool) (info output.InfoResponse, err error) {
	db, err := openIndex(cfg, stderr)
	if err != nil {
		return info, err
	}
	defer func() { err = errors.Join(err, closeIndex(db)) }()
	stats, err := db.Info()
	if err != nil {
		return info, err
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
	info = output.InfoResponse{IndexPath: stats.IndexPath, SchemaVersion: stats.SchemaVersion,
		MessageCount: stats.MessageCount, SessionCount: stats.SessionCount, ActivityCount: stats.ActivityCount,
		FileCount: stats.FileCount, Sources: make([]output.Source, 0, len(stats.Sources)),
		BinaryPath: binaryPath, BinaryVersion: binaryVersion, CurrentSession: currentSessionID(),
		IndexHealthy: stats.IndexHealthy, LockHealthy: true}
	for _, source := range stats.Sources {
		info.Sources = append(info.Sources, output.Source{Path: source.Path, MessageCount: source.MessageCount, SessionCount: source.SessionCount})
	}
	return info, nil
}

func activityOutput(row index.ActivityRecord, full bool) output.Activity {
	activity := output.Activity{ActivityID: row.ActivityID, Status: row.Status, Title: row.Title, Model: row.Model,
		Effort: row.Effort, ToolUses: row.ToolUses, StartedAt: formatTimestamp(row.StartedAt), CompletedAt: formatTimestamp(row.CompletedAt),
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

func compactPreview(content string) string {
	flat := strings.Join(strings.Fields(content), " ")
	runes := []rune(flat)
	if len(runes) > output.DefaultPreviewLength {
		return string(runes[:output.DefaultPreviewLength]) + "…"
	}
	return flat
}

func formatTimestamp(milliseconds int64) string {
	if milliseconds == 0 {
		return ""
	}
	return time.UnixMilli(milliseconds).UTC().Format(time.RFC3339)
}

func writeJSONLine(stdout io.Writer, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(encoded))
	return err
}

func runRebuild(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	set := flag.NewFlagSet("rebuild", flag.ContinueOnError)
	set.SetOutput(stderr)
	session := set.String("session", "", "rebuild a single session")
	indexPath := set.String("index", "", "index database to use")
	transcriptDir := set.String("transcripts", "", "transcript directory to index")
	if err := set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if *indexPath != "" {
		cfg.IndexPath = *indexPath
	}
	if *transcriptDir != "" {
		cfg.TranscriptDirs = []string{*transcriptDir}
	}
	cfg = cfg.withDefaults()

	db, err := index.Open(cfg.IndexPath)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeIndex(db)) }()

	var stats index.SyncStats
	for _, dir := range cfg.TranscriptDirs {
		s, err := db.Rebuild(dir, *session)
		if err != nil {
			return err
		}
		stats.SessionsIndexed += s.SessionsIndexed
		stats.MessagesIndexed += s.MessagesIndexed
	}
	if err := db.ReleaseLifecycleLock(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "indexed %d messages from %d sessions\n",
		stats.MessagesIndexed, stats.SessionsIndexed)
	return nil
}

func runServe(args []string, cfg Config, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("serve", flag.ContinueOnError)
	set.SetOutput(stderr)
	host := set.String("host", "127.0.0.1", "loopback host to bind")
	port := set.Int("port", 8765, "TCP port to bind (0 chooses an available port)")
	indexPath := set.String("index", "", "index database to use")
	transcriptDir := set.String("transcripts", "", "transcript directory to index")
	if err := set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, set.Arg(0))
	}
	if !isLoopbackHost(*host) {
		return fmt.Errorf("%w: --host must be localhost or a loopback address", errUsage)
	}
	if *port < 0 || *port > 65535 {
		return fmt.Errorf("%w: --port must be between 0 and 65535", errUsage)
	}

	if *indexPath != "" {
		cfg.IndexPath = *indexPath
	}
	if *transcriptDir != "" {
		cfg.TranscriptDirs = []string{*transcriptDir}
	}
	resolved := cfg.withDefaults()
	api, err := server.New(server.Config{
		IndexPath:      resolved.IndexPath,
		TranscriptDirs: resolved.TranscriptDirs,
	})
	if err != nil {
		return err
	}
	defer api.Close()

	listener, err := net.Listen("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		return err
	}
	defer listener.Close()
	address := listener.Addr().String()
	fmt.Fprintf(stdout, "cc-search API listening on http://%s\n", address)

	httpServer := &http.Server{Handler: api.Handler()}
	if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// currentSessionID identifies the pi conversation running this command. An
// unset ID means the caller is not running inside pi (or has not exposed its
// session), so search keeps its historical all-sessions behaviour.
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

func emit(stdout, stderr io.Writer, msgs []transcript.Message, opts output.Options) error {
	resp := output.Format(msgs, opts)
	encoded, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	switch b := resp.Budget; {
	case b.Dropped > 0:
		fmt.Fprintf(stderr, "cc-search: warning: the %d character budget dropped %d of %d "+
			"messages (%d spent); raise or disable it with --budget\n",
			b.Limit, b.Dropped, len(msgs), b.Spent)
	case b.Shrunk:
		fmt.Fprintf(stderr, "cc-search: warning: previews were shortened to fit the %d "+
			"character budget (%d spent); raise or disable it with --budget\n",
			b.Limit, b.Spent)
	}
	_, err = fmt.Fprintln(stdout, string(encoded))
	return err
}

func runRead(args []string, cfg Config, stdout, stderr io.Writer) (err error) {
	f := newFlagSet("read", stderr)
	before := f.set.Int("before", defaultReadContext, "messages of context before")
	after := f.set.Int("after", defaultReadContext, "messages of context after")

	if len(args) == 0 {
		return fmt.Errorf("%w: read needs a message id", errUsage)
	}
	if strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("%w: the message id must come before the flags, got %q",
			errUsage, args[0])
	}
	id, args := args[0], args[1:]
	if err := f.set.Parse(args); err != nil {
		return fmt.Errorf("%w: %w", errUsage, err)
	}
	if f.set.NArg() > 0 {
		return fmt.Errorf("%w: unexpected argument %q", errUsage, f.set.Arg(0))
	}

	cfg = f.resolve(cfg)
	db, err := openIndex(cfg, stderr)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeIndex(db)) }()

	msgs, err := db.Around(id, *before, *after, !f.all)
	if err != nil {
		return err
	}
	return emit(stdout, stderr, msgs, f.outputOptions(false))
}
