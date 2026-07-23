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

// DefaultTranscriptDir is where Claude Code keeps this project's transcripts.
const DefaultTranscriptDir = ".claude/projects/-home-andrew-Projects"

// DefaultIndexPath is where the search index lives.
const DefaultIndexPath = ".claude/search-index.db"

// defaultLastCount is how many messages `last` returns without an argument.
const defaultLastCount = 10

const usage = `usage: cc-search <command> [options]

commands:
  last [N] [--hours H] [--session ID] [--type TYPE]
                                        most recent messages (default N=10)
  search PATTERN [options]              full-text search across transcripts
  rebuild [--session ID]                discard and rebuild the index

search options:
  --limit N             maximum results returned
  --window-messages M   only search the newest M messages
  --window-hours H      only search the last H hours
  --session ID          restrict to one session
  --type TYPE           restrict to user, assistant, system, ...
  --prefer-recaps       sort recap messages first
  --prose               match only spoken text, not tool arguments or output
  --recaps-only         return only recap messages

output options (last and search):
  --preview-length N    preview size in characters (default 100)
  --full                include the complete message content
  --index PATH          index database to use
  --transcripts DIR     transcript directory to index
`

// Config supplies the paths the commands operate on. Empty fields fall back to
// the user's default locations.
type Config struct {
	IndexPath     string
	TranscriptDir string
}

func (c Config) withDefaults() Config {
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	if c.IndexPath == "" {
		c.IndexPath = filepath.Join(home, DefaultIndexPath)
	}
	if c.TranscriptDir == "" {
		c.TranscriptDir = filepath.Join(home, DefaultTranscriptDir)
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

// commonFlags are shared by every command that reads or writes the index.
type commonFlags struct {
	set           *flag.FlagSet
	session       string
	previewLength int
	full          bool
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
	return f
}

// resolve merges path overrides from flags into the config.
func (f *commonFlags) resolve(cfg Config) Config {
	if f.indexPath != "" {
		cfg.IndexPath = f.indexPath
	}
	if f.transcriptDir != "" {
		cfg.TranscriptDir = f.transcriptDir
	}
	return cfg.withDefaults()
}

func (f *commonFlags) outputOptions(truncated bool) output.Options {
	return output.Options{
		PreviewLength: f.previewLength,
		Full:          f.full,
		Truncated:     truncated,
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
	if _, err := db.Sync(cfg.TranscriptDir); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			fmt.Fprintf(stderr, "cc-search: warning: %v\n", err)
		} else {
			db.Close()
			return nil, err
		}
	}
	return db, nil
}

func runLast(args []string, cfg Config, stdout, stderr io.Writer) error {
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
	defer db.Close()

	msgs, err := db.Last(index.LastOptions{
		N: count, Hours: *hours, SessionID: f.session, Type: *msgType})
	if err != nil {
		return err
	}
	return emit(stdout, msgs, f.outputOptions(false))
}

func runSearch(args []string, cfg Config, stdout, stderr io.Writer) error {
	f := newFlagSet("search", stderr)
	limit := f.set.Int("limit", 0, "maximum results returned")
	windowMessages := f.set.Int("window-messages", 0, "only search the newest M messages")
	windowHours := f.set.Int("window-hours", 0, "only search the last H hours")
	msgType := f.set.String("type", "", "restrict to a message type")
	preferRecaps := f.set.Bool("prefer-recaps", false, "sort recap messages first")
	prose := f.set.Bool("prose", false, "match only what was said, not tool arguments or output")
	recapsOnly := f.set.Bool("recaps-only", false, "return only recap messages")

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

	cfg = f.resolve(cfg)
	db, err := openIndex(cfg, stderr)
	if err != nil {
		return err
	}
	defer db.Close()

	// Ask for one more than requested so we can tell whether the limit cut
	// results off, then drop the extra before rendering.
	queryLimit := *limit
	if queryLimit > 0 {
		queryLimit++
	}

	msgs, err := db.Search(index.SearchOptions{
		Query:          pattern,
		Limit:          queryLimit,
		WindowMessages: *windowMessages,
		WindowHours:    *windowHours,
		SessionID:      f.session,
		Type:           *msgType,
		PreferRecaps:   *preferRecaps,
		ProseOnly:      *prose,
		RecapsOnly:     *recapsOnly,
	})
	if err != nil {
		return err
	}

	truncated := *limit > 0 && len(msgs) > *limit
	if truncated {
		msgs = msgs[:*limit]
	}
	return emit(stdout, msgs, f.outputOptions(truncated))
}

func runRebuild(args []string, cfg Config, stdout, stderr io.Writer) error {
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
		cfg.TranscriptDir = *transcriptDir
	}
	cfg = cfg.withDefaults()

	db, err := index.Open(cfg.IndexPath)
	if err != nil {
		return err
	}
	defer db.Close()

	stats, err := db.Rebuild(cfg.TranscriptDir, *session)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "indexed %d messages from %d sessions\n",
		stats.MessagesIndexed, stats.SessionsIndexed)
	return nil
}

func emit(stdout io.Writer, msgs []transcript.Message, opts output.Options) error {
	encoded, err := json.Marshal(output.Format(msgs, opts))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(encoded))
	return err
}
