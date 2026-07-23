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

	want := `{"results":[],"total":0,"recapCount":0,"truncated":false}`
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
