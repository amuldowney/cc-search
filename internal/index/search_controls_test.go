package index

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/amuldowney/cc-search/internal/output"
	"github.com/amuldowney/cc-search/internal/transcript"
)

func TestSearchPreviewIncludesMatchesBeyondCachedPrefix(t *testing.T) {
	body := strings.Repeat("普通 context ", 120) + "late needle 雪 matches here " + strings.Repeat("tail 雪 ", 40)
	db, _ := newIndex(t, []msg{{"assistant", body, 10}})

	got, err := db.Search(SearchOptions{Query: "needle", PreviewLength: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d matches, want 1", len(got))
	}
	formatted := output.Format(got, output.Options{PreviewLength: 100})
	if preview := formatted.Results[0].Preview; !strings.Contains(preview, "needle") || !strings.Contains(preview, "…") {
		t.Fatalf("search preview %q must retain the late match and mark omitted context", preview)
	}
	if got := utf8.RuneCountInString(formatted.Results[0].Preview); got > 100 {
		t.Fatalf("search preview has %d runes, want at most 100", got)
	}
	if got[0].CharCount != len(strings.TrimSpace(body)) {
		t.Fatalf("search charCount = %d, want original indexed byte count %d", got[0].CharCount, len(strings.TrimSpace(body)))
	}
	for _, marker := range []string{"\x01cc-search-hit-start\x02", "\x01cc-search-hit-end\x02"} {
		if strings.Contains(formatted.Results[0].Preview, marker) {
			t.Fatalf("output leaked internal marker %q", marker)
		}
	}
	budgeted := output.Format(got, output.Options{PreviewLength: 100, Budget: 40})
	if !strings.Contains(budgeted.Results[0].Preview, "needle") || budgeted.Budget.Spent > 40 {
		t.Fatalf("budgeted output = %+v, want match visible within budget 40", budgeted)
	}

	full, err := db.Search(SearchOptions{Query: "needle"})
	if err != nil {
		t.Fatal(err)
	}
	fullOutput := output.Format(full, output.Options{Full: true})
	if fullOutput.Results[0].Content != strings.TrimSpace(body) {
		t.Fatalf("full output differs from indexed body: got %d bytes, want %d", len(fullOutput.Results[0].Content), len(strings.TrimSpace(body)))
	}
}

func searchOptions(query string) SearchOptions {
	return SearchOptions{Query: query, Limit: 20}
}

func TestSearchCWDIsExactAndAppliedBeforeMessageWindow(t *testing.T) {
	root := t.TempDir()
	writeRawTranscript(t, root, "session-a", []string{
		`{"type":"session","version":3,"id":"session-a","timestamp":"2026-01-01T00:00:00Z","cwd":"/work/a"}`,
		`{"type":"message","id":"a-old","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":"windowneedle"}}`,
	})
	writeRawTranscript(t, root, "session-b", []string{
		`{"type":"session","version":3,"id":"session-b","timestamp":"2026-01-01T00:00:00Z","cwd":"/work/b"}`,
		`{"type":"message","id":"b-new","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":"newest unrelated"}}`,
	})
	db, err := Open(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}

	opts := searchOptions("windowneedle")
	opts.WindowMessages = 1
	opts.CWD = "/work/a"
	got, err := db.Search(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "a-old" {
		t.Fatalf("cwd/window hits = %+v, want the message selected inside /work/a", got)
	}

	lastOpts := LastOptions{N: 10}
	lastOpts.CWD = "/work/a"
	last, err := db.Last(lastOpts)
	if err != nil {
		t.Fatal(err)
	}
	if len(last) != 1 || last[0].ID != "a-old" {
		t.Fatalf("cwd Last() = %+v, want only /work/a", last)
	}
	wrongCWD := opts
	wrongCWD.CWD = "/work"
	wrong, err := db.Search(wrongCWD)
	if err != nil {
		t.Fatal(err)
	}
	if len(wrong) != 0 {
		t.Fatalf("non-exact CWD matched: %+v", wrong)
	}
	withoutWindow := opts
	withoutWindow.WindowMessages = 0
	unwindowed, err := db.Search(withoutWindow)
	if err != nil {
		t.Fatal(err)
	}
	if len(unwindowed) != 1 || unwindowed[0].ID != "a-old" {
		t.Fatalf("unwindowed CWD search = %+v, want only exact /work/a", unwindowed)
	}
}

func TestSearchPerSessionCapsBeforeOverallLimit(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "session-a", []msg{
		{"assistant", "shared needle excellent match", 1},
		{"assistant", "shared needle ordinary match", 2},
	})
	writeTranscript(t, root, "session-b", []msg{{"assistant", "shared needle other session", 3}})
	db, err := Open(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}

	opts := searchOptions("shared needle")
	opts.Limit = 2
	opts.PerSession = 1
	got, err := db.Search(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SessionID == got[1].SessionID {
		t.Fatalf("per-session search sessions = %+v, want one hit from each session", got)
	}
}

func TestSearchControlsDefaultAndNoiseReductionAreOptIn(t *testing.T) {
	db, _ := newIndex(t, []msg{
		{"assistant", "alpha beta original useful prose", 1},
		{"assistant", "alpha beta original second prose", 2},
	})
	baseline, err := db.Search(searchOptions("alpha beta"))
	if err != nil {
		t.Fatal(err)
	}
	withControls := searchOptions("alpha beta")
	withControls.PerSession = 0
	withControls.ReduceNoise = false
	got, err := db.Search(withControls)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(baseline) || got[0].ID != baseline[0].ID || got[1].ID != baseline[1].ID {
		t.Fatalf("zero/default controls changed ordering: baseline=%+v controlled=%+v", baseline, got)
	}
	if _, err := db.Search(searchOptions("alpha beta")); err != nil {
		t.Fatal(err)
	}
}

func TestSearchPreviewUsesStemmingRawExpressionsAndProseColumn(t *testing.T) {
	root := t.TempDir()
	writeRawTranscript(t, root, "session-a", []string{
		`{"type":"assistant","uuid":"mixed","sessionId":"session-a","timestamp":"2026-01-01T00:00:01Z","message":{"content":[{"type":"text","text":"We deployed the release."},{"type":"tool_use","name":"Bash","input":{"command":"run toolwordneedle now"}}]}}`,
	})
	db, err := Open(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}

	stemmed, err := db.Search(SearchOptions{Query: "deploy", ProseOnly: true, PreviewLength: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(stemmed) != 1 || !strings.Contains(stemmed[0].SearchPreview, "deployed") || strings.Contains(stemmed[0].SearchPreview, "toolwordneedle") {
		t.Fatalf("prose stem preview = %+v, want deployed prose without tool words", stemmed)
	}
	raw, err := db.Search(SearchOptions{Query: `"deployed the release"`, Raw: true, PreviewLength: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 1 || !strings.Contains(raw[0].SearchPreview, "deployed") {
		t.Fatalf("raw phrase preview = %+v, want the FTS phrase match", raw)
	}
	tool, err := db.Search(SearchOptions{Query: "toolwordneedle", PreviewLength: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(tool) != 1 || !strings.Contains(tool[0].SearchPreview, "toolwordneedle") {
		t.Fatalf("all-column preview = %+v, want tool content for a non-prose search", tool)
	}
}

func TestSearchPreviewUsesAnyAndPunctuatedExpressions(t *testing.T) {
	db, _ := newIndex(t, []msg{{"assistant", "The file is display.cpp and the needle is here", 10}})
	any, err := db.Search(SearchOptions{Query: "missing needle", Any: true, PreviewLength: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(any) != 1 || !strings.Contains(any[0].SearchPreview, "needle") {
		t.Fatalf("ANY preview = %+v, want the matching alternative", any)
	}
	punctuated, err := db.Search(SearchOptions{Query: "display.cpp", PreviewLength: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(punctuated) != 1 || !strings.Contains(punctuated[0].SearchPreview, "display.cpp") {
		t.Fatalf("punctuated preview = %+v, want display.cpp context", punctuated)
	}
}

func TestSearchReduceNoiseDemotesButDoesNotExcludeCopiedBodies(t *testing.T) {
	root := t.TempDir()
	original := "plain original prose has needle " + strings.Repeat("ordinary context ", 60)
	writeTranscript(t, root, "session-a", []msg{
		{"assistant", "---\nname: copied-skill\ndescription: copied guidance\nneedle needle needle", 1},
		{"assistant", original, 2},
	})
	writeTranscript(t, root, "session-b", []msg{{"assistant", `{"results":[{"preview":"needle"}],"total":1,"truncated":false,"relaxed":false,"budget":{"limit":0}}`, 3}})
	db, err := Open(t.TempDir() + "/index.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Sync(root); err != nil {
		t.Fatal(err)
	}

	all, err := db.Search(SearchOptions{Query: "needle", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	demoted, err := db.Search(SearchOptions{Query: "needle", Limit: 10, ReduceNoise: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || len(demoted) != 3 {
		t.Fatalf("noise reduction excluded hits: default=%d reduced=%d", len(all), len(demoted))
	}
	position := func(messages []transcript.Message, id string) int {
		for i, message := range messages {
			if message.ID == id {
				return i
			}
		}
		return -1
	}
	originalPosition := position(all, "session-a-1")
	for _, noisyID := range []string{"session-a-0", "session-b-0"} {
		if position(all, noisyID) >= originalPosition || position(demoted, noisyID) <= position(demoted, "session-a-1") {
			t.Fatalf("noise rank for %s was not demoted: default=%+v reduced=%+v", noisyID, all, demoted)
		}
	}
	if demoted[0].ID != "session-a-1" {
		t.Fatalf("noise ranking first hit = %+v, want the original prose message", demoted[0])
	}
	seenSkill, seenEcho, seenOriginal := false, false, false
	for _, m := range demoted {
		seenSkill = seenSkill || strings.Contains(m.Content, "copied-skill")
		seenEcho = seenEcho || strings.Contains(m.Content, `"results"`)
		seenOriginal = seenOriginal || strings.Contains(m.Content, "plain original prose")
	}
	if !seenSkill || !seenEcho || !seenOriginal {
		t.Fatalf("reduce-noise results omitted expected skill/echo/original content: %+v", demoted)
	}

	diverse := SearchOptions{Query: "needle", Limit: 2, PerSession: 1, ReduceNoise: true}
	mixed, err := db.Search(diverse)
	if err != nil {
		t.Fatal(err)
	}
	if len(mixed) != 2 || mixed[0].ID != "session-a-1" || mixed[0].SessionID == mixed[1].SessionID {
		t.Fatalf("noise/diversity winners = %+v, want original prose then another session", mixed)
	}
}

func TestSearchRejectsNegativePerSession(t *testing.T) {
	db, _ := newIndex(t, []msg{{"assistant", "negative cap needle", 1}})
	opts := searchOptions("needle")
	opts.PerSession = -1
	if _, err := db.Search(opts); err == nil {
		t.Fatal("Search accepted a negative PerSession cap")
	}
}
