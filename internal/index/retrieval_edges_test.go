package index

import (
	"strings"
	"testing"
)

func TestMatchingExcerptPreservesPunctuationAndNegation(t *testing.T) {
	for _, body := range []string{"Do not deploy before approval.", "Use PI_REMOTE_GENERATION_ID for this deployment."} {
		query := "deploy"
		if strings.Contains(body, "PI_REMOTE") {
			query = "PI_REMOTE_GENERATION_ID"
		}
		db, _ := newIndex(t, []msg{{"assistant", body, 1}})
		got, err := db.Search(SearchOptions{Query: query, PreviewLength: 180})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].SearchPreview != body {
			t.Fatalf("excerpt must retain a short message verbatim: got %+v want %q", got, body)
		}
	}
	body := strings.Repeat("background words ", 200) + "Do not deploy until approval. " + strings.Repeat("later words ", 100)
	db, _ := newIndex(t, []msg{{"assistant", body, 1}})
	got, err := db.Search(SearchOptions{Query: "deploy", PreviewLength: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].SearchPreview, "Do not deploy until approval.") {
		t.Fatalf("lost immediate context: %+v", got)
	}
}

func TestNoiseDemotesLargeEchoWithoutTrailingMetadataInPrefix(t *testing.T) {
	echo := `{"results":[{"id":"old","sessionId":"old-session","preview":"` + strings.Repeat("needle ", 150) + `"}],"total":1,"relaxed":false,"budget":{"limit":60000}}`
	genuine := strings.Repeat("explanation ", 1000) + "needle genuine decision"
	db, _ := newIndex(t, []msg{{"assistant", echo, 1}, {"assistant", genuine, 2}})
	opts := SearchOptions{Query: "needle", Limit: 2, PreviewLength: 100}
	baseline, err := db.Search(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline) != 2 || baseline[0].ID != "session-a-0" {
		t.Fatal("fixture must rank echo first without noise reduction")
	}
	opts.ReduceNoise = true
	got, err := db.Search(opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "session-a-1" {
		t.Fatalf("large echo should be demoted, not removed: %+v", got)
	}
}
