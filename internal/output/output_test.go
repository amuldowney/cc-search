package output

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/andrewmuldowney/cc-search/internal/transcript"
)

func TestFormatRendersMessageFields(t *testing.T) {
	msgs := []transcript.Message{{
		ID:        "msg_01ABC",
		SessionID: "fd97266a",
		Timestamp: 1784847600000, // 2026-07-23T23:00:00Z
		Type:      "assistant",
		Content:   "the index rebuild completed",
		CharCount: 27,
		IsRecap:   true,
	}}

	got := Format(msgs, Options{})

	if len(got.Results) != 1 {
		t.Fatalf("got %d results, want 1", len(got.Results))
	}
	r := got.Results[0]
	if r.ID != "msg_01ABC" || r.SessionID != "fd97266a" || r.Type != "assistant" {
		t.Errorf("result = %+v", r)
	}
	if r.Timestamp != "2026-07-23T23:00:00Z" {
		t.Errorf("Timestamp = %q, want RFC3339 UTC", r.Timestamp)
	}
	if !r.IsRecap {
		t.Error("IsRecap = false, want true")
	}
	if r.CharCount != 27 {
		t.Errorf("CharCount = %d, want 27", r.CharCount)
	}
	if got.Total != 1 {
		t.Errorf("Total = %d, want 1", got.Total)
	}
	if got.RecapCount != 1 {
		t.Errorf("RecapCount = %d, want 1", got.RecapCount)
	}
}

func TestFormatTruncatesPreviewToDefaultLength(t *testing.T) {
	long := strings.Repeat("a", 250)

	got := Format([]transcript.Message{{Content: long, CharCount: len(long)}}, Options{})

	preview := got.Results[0].Preview
	if len([]rune(preview)) != DefaultPreviewLength+1 {
		t.Fatalf("preview length = %d, want %d plus an ellipsis",
			len([]rune(preview)), DefaultPreviewLength)
	}
	if !strings.HasSuffix(preview, "…") {
		t.Errorf("preview does not end with an ellipsis: %q", preview)
	}
	if got.Results[0].CharCount != 250 {
		t.Errorf("CharCount = %d, want the full length 250", got.Results[0].CharCount)
	}
}

func TestFormatHonorsPreviewLength(t *testing.T) {
	got := Format([]transcript.Message{{Content: strings.Repeat("b", 50)}},
		Options{PreviewLength: 10})

	if got.Results[0].Preview != strings.Repeat("b", 10)+"…" {
		t.Errorf("Preview = %q", got.Results[0].Preview)
	}
}

func TestFormatLeavesShortPreviewUntouched(t *testing.T) {
	got := Format([]transcript.Message{{Content: "short"}}, Options{})

	if got.Results[0].Preview != "short" {
		t.Errorf("Preview = %q, want %q", got.Results[0].Preview, "short")
	}
}

func TestFormatCollapsesWhitespaceInPreview(t *testing.T) {
	got := Format([]transcript.Message{{Content: "first line\n\n  second\tline"}}, Options{})

	if got.Results[0].Preview != "first line second line" {
		t.Errorf("Preview = %q", got.Results[0].Preview)
	}
}

func TestFormatOmitsContentUnlessFull(t *testing.T) {
	msgs := []transcript.Message{{Content: "the full body"}}

	compact, err := json.Marshal(Format(msgs, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(compact), `"content"`) {
		t.Errorf("compact output contains a content field: %s", compact)
	}

	full := Format(msgs, Options{Full: true})
	if full.Results[0].Content != "the full body" {
		t.Errorf("Content = %q, want the unmodified body", full.Results[0].Content)
	}
}

func TestFormatEmitsOnlyOneBodyField(t *testing.T) {
	msgs := []transcript.Message{{Content: "line one\nline two"}}

	compact, err := json.Marshal(Format(msgs, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(compact), `"content"`) {
		t.Errorf("compact output contains content: %s", compact)
	}
	if !strings.Contains(string(compact), `"preview":"line one line two"`) {
		t.Errorf("compact output lacks preview: %s", compact)
	}

	full, err := json.Marshal(Format(msgs, Options{Full: true}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(full), `"preview"`) {
		t.Errorf("full output contains preview: %s", full)
	}
	if !strings.Contains(string(full), `"content":"line one\nline two"`) {
		t.Errorf("full output lacks exact content: %s", full)
	}
}

func TestFormatUseProseRendersProsePreview(t *testing.T) {
	msgs := []transcript.Message{{
		Content:   `[tool: Bash] {"command":"ls"}` + "\n" + "here is the listing",
		Prose:     "here is the listing",
		CharCount: 48,
	}}

	got := Format(msgs, Options{UseProse: true})

	if got.Results[0].Preview != "here is the listing" {
		t.Errorf("Preview = %q, want the prose", got.Results[0].Preview)
	}
	if got.Results[0].CharCount != len("here is the listing") {
		t.Errorf("CharCount = %d, want the prose length", got.Results[0].CharCount)
	}
}

func TestFormatUseProseWithFullEmitsProse(t *testing.T) {
	msgs := []transcript.Message{{
		Content: "everything including tools",
		Prose:   "just the words",
	}}

	got := Format(msgs, Options{UseProse: true, Full: true})

	if got.Results[0].Content != "just the words" {
		t.Errorf("Content = %q, want the prose under --full", got.Results[0].Content)
	}
}

func TestFormatCountsOnlyRecaps(t *testing.T) {
	got := Format([]transcript.Message{
		{Content: "one", IsRecap: true},
		{Content: "two"},
		{Content: "three", IsRecap: true},
	}, Options{})

	if got.Total != 3 {
		t.Errorf("Total = %d, want 3", got.Total)
	}
	if got.RecapCount != 2 {
		t.Errorf("RecapCount = %d, want 2", got.RecapCount)
	}
}

func TestFormatMarshalsEmptyResultsAsArray(t *testing.T) {
	encoded, err := json.Marshal(Format(nil, Options{}))
	if err != nil {
		t.Fatal(err)
	}

	want := `{"results":[],"total":0,"recapCount":0,"truncated":false,"relaxed":false,` +
		`"budget":{"limit":0,"spent":0,"dropped":0,"shrunk":false}}`
	if string(encoded) != want {
		t.Errorf("got %s, want %s", encoded, want)
	}
}

func TestFormatPropagatesTruncatedFlag(t *testing.T) {
	got := Format([]transcript.Message{{Content: "one"}}, Options{Truncated: true})

	if !got.Truncated {
		t.Error("Truncated = false, want true")
	}
}

func TestFormatBudgetDropsResultsThatDoNotFit(t *testing.T) {
	msgs := []transcript.Message{
		{Content: strings.Repeat("a", 40), CharCount: 40},
		{Content: strings.Repeat("b", 40), CharCount: 40},
		{Content: strings.Repeat("c", 40), CharCount: 40},
	}

	got := Format(msgs, Options{Budget: 90})

	if got.Total != 2 {
		t.Fatalf("Total = %d, want 2 — the third does not fit in 90 chars", got.Total)
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true")
	}
	if !got.Budget.Hit() {
		t.Error("BudgetHit = false, want true")
	}
}

func TestFormatBudgetClipsAnOversizedFirstResult(t *testing.T) {
	msgs := []transcript.Message{{Content: strings.Repeat("a", 500), CharCount: 500}}

	got := Format(msgs, Options{Budget: 50, Full: true})

	if got.Total != 1 {
		t.Fatalf("Total = %d, want 1 — never return nothing because one message is huge", got.Total)
	}
	if len([]rune(got.Results[0].Content)) > 50 {
		t.Errorf("content is %d runes, want it clipped to the 50 char budget",
			len([]rune(got.Results[0].Content)))
	}
	if !got.Budget.Hit() {
		t.Error("BudgetHit = false, want true")
	}
}

func TestFormatBudgetZeroMeansUnlimited(t *testing.T) {
	msgs := []transcript.Message{
		{Content: strings.Repeat("a", 400), CharCount: 400},
		{Content: strings.Repeat("b", 400), CharCount: 400},
	}

	got := Format(msgs, Options{Budget: 0, Full: true})

	if got.Total != 2 || got.Budget.Hit() {
		t.Errorf("Total = %d, BudgetHit = %v; want everything with no budget",
			got.Total, got.Budget.Hit())
	}
}

func TestFormatBudgetCountsFullContentNotPreview(t *testing.T) {
	msgs := []transcript.Message{
		{Content: strings.Repeat("a", 300), CharCount: 300},
		{Content: strings.Repeat("b", 300), CharCount: 300},
	}

	// Previews are ~100 chars so both would fit; full bodies are 300 each.
	got := Format(msgs, Options{Budget: 400, Full: true})

	if got.Total != 1 {
		t.Errorf("Total = %d, want 1 — the budget must measure what is emitted", got.Total)
	}
}

func TestFormatBudgetCountsCharactersNotBytes(t *testing.T) {
	// 100 three-byte runes: 100 characters, 300 bytes.
	body := strings.Repeat("字", 100)
	msgs := []transcript.Message{
		{Content: body, CharCount: len(body)},
		{Content: body, CharCount: len(body)},
	}

	got := Format(msgs, Options{Budget: 250, Full: true})

	if got.Total != 2 {
		t.Fatalf("Total = %d, want 2 — 200 characters fits a 250 character budget", got.Total)
	}
	if got.Budget.Hit() {
		t.Error("BudgetHit = true; the budget was counted in bytes, not characters")
	}
}

func longMsgs(n, chars int) []transcript.Message {
	msgs := make([]transcript.Message, n)
	for i := range msgs {
		body := strings.Repeat(string(rune('a'+i%26)), chars)
		msgs[i] = transcript.Message{Content: body, CharCount: chars}
	}
	return msgs
}

func TestFormatBudgetShrinksPreviewsRatherThanDroppingResults(t *testing.T) {
	// 10 results at 200 chars each is 2000; the budget only allows 1000.
	got := Format(longMsgs(10, 500), Options{PreviewLength: 200, Budget: 1000})

	if got.Total != 10 {
		t.Fatalf("Total = %d, want all 10 — previews should shrink before results drop", got.Total)
	}
	if got.Budget.Dropped != 0 {
		t.Errorf("Dropped = %d, want 0", got.Budget.Dropped)
	}
	if !got.Budget.Shrunk {
		t.Error("Shrunk = false, want true")
	}
	if got.Budget.Spent > 1000 {
		t.Errorf("Spent = %d, over the 1000 budget", got.Budget.Spent)
	}
	for i, r := range got.Results {
		if len([]rune(r.Preview)) > 110 {
			t.Errorf("result %d preview is %d runes, want ~100 after shrinking",
				i, len([]rune(r.Preview)))
		}
	}
}

func TestFormatBudgetRedistributesSurplusFromShortResults(t *testing.T) {
	msgs := []transcript.Message{
		{Content: "tiny", CharCount: 4},
		{Content: strings.Repeat("b", 500), CharCount: 500},
	}

	got := Format(msgs, Options{PreviewLength: 200, Budget: 204})

	if got.Total != 2 {
		t.Fatalf("Total = %d, want 2", got.Total)
	}
	if got.Results[0].Preview != "tiny" {
		t.Errorf("short result = %q, want it untouched", got.Results[0].Preview)
	}
	// The short result used 4 of its 102 share; the surplus goes to the long one.
	if n := len([]rune(got.Results[1].Preview)); n < 150 {
		t.Errorf("long preview is %d runes, want it to absorb the unused share", n)
	}
}

func TestFormatBudgetDropsResultsOnlyWhenMinimumPreviewsDoNotFit(t *testing.T) {
	// A 100 char budget cannot give 10 results a useful preview each.
	got := Format(longMsgs(10, 500), Options{PreviewLength: 200, Budget: 100})

	if got.Total >= 10 {
		t.Fatalf("Total = %d, want results dropped when the floor cannot be met", got.Total)
	}
	if got.Total < 1 {
		t.Fatal("Total = 0, want at least one result")
	}
	if got.Budget.Dropped != 10-got.Total {
		t.Errorf("Dropped = %d, want %d", got.Budget.Dropped, 10-got.Total)
	}
	if !got.Truncated {
		t.Error("Truncated = false, want true")
	}
	for _, r := range got.Results {
		if n := len([]rune(r.Preview)); n < MinPreviewLength {
			t.Errorf("preview is %d runes, below the %d floor", n, MinPreviewLength)
		}
	}
}

func TestFormatFullModeKeepsBodiesWholeAndDropsTheRest(t *testing.T) {
	got := Format(longMsgs(5, 300), Options{Budget: 700, Full: true})

	if got.Total != 2 {
		t.Fatalf("Total = %d, want 2 whole bodies rather than 5 clipped ones", got.Total)
	}
	for i, r := range got.Results {
		if len([]rune(r.Content)) != 300 {
			t.Errorf("result %d is %d runes, want the complete 300", i, len([]rune(r.Content)))
		}
	}
	if got.Budget.Dropped != 3 {
		t.Errorf("Dropped = %d, want 3", got.Budget.Dropped)
	}
}

func TestFormatReportsBudgetAccounting(t *testing.T) {
	got := Format(longMsgs(3, 50), Options{PreviewLength: 100, Budget: 5000})

	if got.Budget.Limit != 5000 {
		t.Errorf("Limit = %d, want 5000", got.Budget.Limit)
	}
	if got.Budget.Spent != 150 {
		t.Errorf("Spent = %d, want 150 (3 x 50 chars)", got.Budget.Spent)
	}
	if got.Budget.Dropped != 0 || got.Budget.Shrunk {
		t.Errorf("budget = %+v, want nothing dropped or shrunk", got.Budget)
	}
}

func TestFormatReportsSpentWithNoBudget(t *testing.T) {
	got := Format(longMsgs(2, 50), Options{Budget: 0})

	if got.Budget.Limit != 0 {
		t.Errorf("Limit = %d, want 0 meaning unlimited", got.Budget.Limit)
	}
	if got.Budget.Spent != 100 {
		t.Errorf("Spent = %d, want 100 — accounting still reports usage", got.Budget.Spent)
	}
}

func TestFormatHandlesNoResultsUnderABudget(t *testing.T) {
	got := Format(nil, Options{Budget: 1000})

	if got.Total != 0 || len(got.Results) != 0 {
		t.Errorf("got %+v, want an empty result set", got.Results)
	}
	if got.Budget.Dropped != 0 || got.Budget.Spent != 0 {
		t.Errorf("budget = %+v, want zeroed accounting", got.Budget)
	}
}
