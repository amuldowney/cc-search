package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/amuldowney/cc-search/internal/index"
)

type transcriptRoots []string

func (r *transcriptRoots) String() string         { return fmt.Sprint([]string(*r)) }
func (r *transcriptRoots) Set(value string) error { *r = append(*r, value); return nil }

// StartBackgroundRefresh is the executable-only launcher. Library users of Run
// can inject their own scheduler through Config rather than reexecuting an
// unknown host application (notably the Go test binary).
func StartBackgroundRefresh(path string, roots []string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	return startRefreshProcess(executable, path, roots)
}

func startRefreshProcess(executable, path string, roots []string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	args := []string{"refresh", "--background", "--index", path}
	for _, root := range roots {
		root, err = filepath.Abs(root)
		if err != nil {
			return err
		}
		args = append(args, "--transcripts", root)
	}
	command := exec.Command(executable, args...)
	detachRefresh(command)
	// nil stdio is /dev/null, not the caller's pipes. An indexing child must
	// not hold a tool invocation open after the search response is written.
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

func runRefresh(args []string, cfg Config, stdout, stderr io.Writer) error {
	set := flag.NewFlagSet("refresh", flag.ContinueOnError)
	set.SetOutput(stderr)
	path := set.String("index", "", "index database to refresh")
	background := set.Bool("background", false, "skip if fresh or another writer is active")
	var roots transcriptRoots
	set.Var(&roots, "transcripts", "transcript root (repeatable)")
	if err := set.Parse(args); err != nil {
		return errors.Join(errUsage, err)
	}
	if set.NArg() != 0 {
		return fmt.Errorf("%w: refresh accepts only flags", errUsage)
	}
	if *path != "" {
		cfg.IndexPath = *path
	}
	if len(roots) > 0 {
		cfg.TranscriptDirs = roots
	}
	cfg = cfg.withDefaults()
	stats, err := index.Refresh(cfg.IndexPath, cfg.TranscriptDirs, index.RefreshOptions{Force: !*background, Background: *background})
	if err != nil {
		return err
	}
	return writeJSONLine(stdout, map[string]any{"version": 1, "sessionsIndexed": stats.SessionsIndexed, "messagesIndexed": stats.MessagesIndexed, "filesSkipped": stats.FilesSkipped})
}
