package cli

import (
	"flag"
	"fmt"
	"io"

	"github.com/amuldowney/cc-search/internal/output"
)

// parse validates common output and count options before opening an index.
func (f *commonFlags) parse(args []string) error {
	if err := f.set.Parse(args); err != nil {
		return err
	}
	if f.format != "json" && f.format != "text" {
		return fmt.Errorf("--format must be json or text")
	}
	for _, name := range []string{"limit", "hours", "window-hours", "window-messages", "hits", "before", "after", "per-session", "preview-length", "budget"} {
		if opt := f.set.Lookup(name); opt != nil {
			if value, ok := opt.Value.(flag.Getter); ok {
				if n, ok := value.Get().(int); ok && n < 0 {
					return fmt.Errorf("--%s must be non-negative", name)
				}
			}
		}
	}
	return nil
}

func (f *commonFlags) visited(name string) bool {
	found := false
	f.set.Visit(func(v *flag.Flag) {
		if v.Name == name {
			found = true
		}
	})
	return found
}

func (f *commonFlags) writeResponse(w io.Writer, value any) error {
	if f.format == "text" {
		return output.WriteText(w, value)
	}
	return writeJSONLine(w, value)
}

type retrievalFlags struct {
	cwd                                            string
	hours, windowHours, windowMessages, perSession int
	reduceNoise                                    bool
}

func newRetrievalFlags(f *commonFlags, relevance bool) *retrievalFlags {
	q := &retrievalFlags{}
	f.set.StringVar(&q.cwd, "cwd", "", "exact session working-directory filter")
	f.set.IntVar(&q.hours, "hours", 0, "only the last H hours (alias for --window-hours)")
	f.set.IntVar(&q.windowHours, "window-hours", 0, "only the last H hours")
	if relevance {
		f.set.IntVar(&q.windowMessages, "window-messages", 0, "only search the newest M messages in the selected scope")
		f.set.IntVar(&q.perSession, "per-session", 0, "maximum hits per session before the overall limit (0 = unlimited)")
		f.set.BoolVar(&q.reduceNoise, "reduce-noise", false, "demote copied skill documents and echoed search-result JSON")
	}
	return q
}

func (q *retrievalFlags) resolve(f *commonFlags) error {
	if f.visited("hours") && f.visited("window-hours") && q.hours != q.windowHours {
		return fmt.Errorf("%w: --hours and --window-hours disagree", errUsage)
	}
	if !f.visited("hours") {
		q.hours = q.windowHours
	}
	return nil
}
