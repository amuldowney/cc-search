// Package cli wires the cc-search subcommands to the index.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/andrewmuldowney/cc-search/internal/index"
	"github.com/andrewmuldowney/cc-search/internal/output"
	"github.com/andrewmuldowney/cc-search/internal/transcript"
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
  rebuild [--session ID]                discard and rebuild the index

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

output options (last, search and read):
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
	case "rebuild":
		err = runRebuild(rest, cfg, stdout, stderr)
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
