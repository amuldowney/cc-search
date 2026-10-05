package output

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func renderText(t *testing.T, value any) string {
	t.Helper()
	var got bytes.Buffer
	if err := WriteText(&got, value); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	return got.String()
}

func TestWriteTextResponseRendersFullMetadataAndBudgetedBody(t *testing.T) {
	got := renderText(t, Response{
		Results: []Result{{
			ID: "msg-full-copyable-identifier-123456789", SessionID: "session-full-id",
			Timestamp: "2026-07-23T23:00:00Z", Type: "assistant",
			Preview: "pre-budgeted preview…", ActivityID: "activity-1",
			ActivityRole: "child", ParentActivityID: "parent-activity",
			ParentSessionID: "parent-session", ChildSessionID: "child-session",
		}},
		Total: 128,
	})

	for _, want := range []string{
		"Message ID: msg-full-copyable-identifier-123456789",
		"Session ID: session-full-id",
		"Timestamp (UTC): 2026-07-23T23:00:00Z",
		"Type: assistant",
		"Body (preview): pre-budgeted preview…",
		"Activity ID: activity-1",
		"Activity role: child",
		"Parent activity ID: parent-activity",
		"Parent session ID: parent-session",
		"Child session ID: child-session",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("text output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "128") || strings.Contains(strings.ToLower(got), "total matches") {
		t.Errorf("text output misrepresents Total as available matches:\n%s", got)
	}
}

func TestWriteTextContextSeparatesHitsAndNeighborsWithHitIDs(t *testing.T) {
	got := renderText(t, ContextResponse{
		Query: "needle",
		Hits:  []Result{{ID: "hit-message-id", SessionID: "hit-session", Type: "user", Preview: "matched"}},
		Results: []ContextResult{
			{Result: Result{ID: "neighbor-message-id", SessionID: "neighbor-session", Type: "assistant", Preview: "nearby"}, HitIDs: []string{"hit-message-id"}},
			{Result: Result{ID: "hit-message-id", SessionID: "hit-session", Type: "user", Preview: "matched"}, HitIDs: []string{"hit-message-id"}},
		},
		TotalHits: 1, Total: 999,
	})

	for _, want := range []string{"Hits (1):", "Context messages (2):", "[Context 2 - Hit]", "[Context 1 - Neighbor]", "Message ID: hit-message-id", "Message ID: neighbor-message-id", "Hit IDs: hit-message-id"} {
		if !strings.Contains(got, want) {
			t.Errorf("context output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "999") {
		t.Errorf("context output misrepresents Total as available matches:\n%s", got)
	}
}

func TestWriteTextCommandsSessionsAndActivities(t *testing.T) {
	commands := renderText(t, CommandsResponse{Commands: []Command{{
		Tool: "Bash", Arguments: map[string]any{"command": "printf hello"},
		SessionID: "session-full", MessageID: "message-full", Timestamp: "2026-07-23T23:00:00Z",
		Output: "hello\n",
	}}})
	for _, want := range []string{"Tool: Bash", `"command": "printf hello"`, "Session ID: session-full", "Message ID: message-full", "Output:", "hello"} {
		if !strings.Contains(commands, want) {
			t.Errorf("commands output missing %q:\n%s", want, commands)
		}
	}

	sessions := renderText(t, SessionsResponse{Sessions: []Session{{
		SessionID: "session-full", CWD: "/work/tree", Visibility: "visible", ParentSessionID: "parent",
		LastTimestamp: "2026-07-23T23:00:00Z", LastPreview: "recent message", MessageCount: 7,
	}}})
	for _, want := range []string{"Session ID: session-full", "CWD: /work/tree", "Visibility: visible", "Parent session ID: parent", "Last message (UTC): 2026-07-23T23:00:00Z", "Last preview: recent message", "Messages: 7"} {
		if !strings.Contains(sessions, want) {
			t.Errorf("sessions output missing %q:\n%s", want, sessions)
		}
	}

	activity := Activity{
		ActivityID: "activity-full", Status: "completed", Title: "Review change", Model: "model-x", Effort: "high", ToolUses: 2,
		StartedAt: "2026-07-23T23:00:00Z", CompletedAt: "2026-07-23T23:01:00Z",
		ParentSessionID: "parent-session", ChildSessionID: "child-session", ParentActivityID: "parent-activity",
		ResultSummary: "Reviewed", Description: "A task", Kind: "subagent", Namespace: "agent",
		StartEntryID: "start-entry", StartParentID: "start-parent", LinkedEntryID: "linked-entry",
		TerminalEntryID: "terminal-entry", TerminalParentID: "terminal-parent",
	}
	activities := renderText(t, ActivitiesResponse{Activities: []Activity{activity}})
	for _, want := range []string{"Activity ID: activity-full", "Status: completed", "Title: Review change", "Model: model-x", "Effort: high", "Tool uses: 2", "Started (UTC): 2026-07-23T23:00:00Z", "Completed (UTC): 2026-07-23T23:01:00Z", "Parent session ID: parent-session", "Child session ID: child-session", "Parent activity ID: parent-activity", "Result summary: Reviewed", "Description: A task", "Kind: subagent", "Namespace: agent", "Start entry ID: start-entry", "Terminal parent ID: terminal-parent"} {
		if !strings.Contains(activities, want) {
			t.Errorf("activities output missing %q:\n%s", want, activities)
		}
	}

	single := renderText(t, activity)
	if !strings.Contains(single, "Activity ID: activity-full") {
		t.Errorf("single activity was not rendered:\n%s", single)
	}
}

func TestWriteTextShowsEmptyAndResponseNotices(t *testing.T) {
	emptyCases := []struct {
		name  string
		value any
		wants []string
	}{
		{"results", Response{}, []string{"Results (0):", "None"}},
		{"context", ContextResponse{}, []string{"Hits (0):", "None", "Context messages (0):", "None"}},
		{"commands", CommandsResponse{}, []string{"Commands (0):", "None"}},
		{"sessions", SessionsResponse{}, []string{"Sessions (0):", "None"}},
		{"activities", ActivitiesResponse{}, []string{"Activities (0):", "None"}},
	}
	for _, tc := range emptyCases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderText(t, tc.value)
			for _, want := range tc.wants {
				if !strings.Contains(got, want) {
					t.Errorf("empty output missing %q:\n%s", want, got)
				}
			}
		})
	}

	got := renderText(t, Response{
		Results:   []Result{{ID: "one", SessionID: "s", Type: "user", Preview: "short"}},
		Truncated: true, Relaxed: true,
		Budget: Budget{Limit: 200, Spent: 140, Dropped: 2, Shrunk: true},
	})
	for _, want := range []string{"relaxed", "truncated", "Budget", "200", "140", "dropped 2", "shortened"} {
		if !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
			t.Errorf("response notices missing %q:\n%s", want, got)
		}
	}
}

func TestWriteTextSanitizesControlCharactersWithoutFlatteningBody(t *testing.T) {
	got := renderText(t, Response{Results: []Result{{
		ID: "msg\nforged: true", SessionID: "s\x1b[31m", Timestamp: "time\rforged",
		Type: "assistant\x00", Content: "first\nsecond\tλ\x1b[31mred\x7f",
	}}})
	if strings.ContainsAny(got, "\x1b\x00\x7f\r") {
		t.Errorf("output contains terminal/control characters: %q", got)
	}
	if !strings.Contains(got, `Message ID: msg\nforged: true`) || !strings.Contains(got, "first\nsecond\tλ") {
		t.Errorf("identifiers were not line-safe or body layout was not preserved:\n%q", got)
	}
	lines := strings.Split(got, "\n")
	if len(lines) < 5 || lines[2] != `Message ID: msg\nforged: true` || lines[3] != `Session ID: s\u001B[31m` {
		t.Errorf("metadata injection created a new line:\n%q", got)
	}
}

func TestWriteTextPropagatesWriterAndMarshalErrors(t *testing.T) {
	wantErr := errors.New("output unavailable")
	writer := errorWriter{err: wantErr}
	if err := WriteText(writer, Response{Results: []Result{{ID: "id"}}}); !errors.Is(err, wantErr) {
		t.Errorf("WriteText writer error = %v, want %v", err, wantErr)
	}

	var fallback bytes.Buffer
	if err := WriteText(&fallback, InfoResponse{IndexPath: "diagnostic"}); err != nil {
		t.Errorf("diagnostic JSON fallback returned error: %v", err)
	} else if !strings.Contains(fallback.String(), "{\n  \"indexPath\": \"diagnostic\"") {
		t.Errorf("diagnostic fallback is not pretty JSON: %s", fallback.String())
	}
	if err := WriteText(&bytes.Buffer{}, struct{ Bad chan int }{Bad: make(chan int)}); err == nil {
		t.Error("unsupported value fallback unexpectedly succeeded")
	}
	if err := WriteText(&bytes.Buffer{}, CommandsResponse{Commands: []Command{{Arguments: make(chan int)}}}); err == nil {
		t.Error("unsupported command arguments unexpectedly succeeded")
	}
}

type errorWriter struct{ err error }

func (w errorWriter) Write([]byte) (int, error) { return 0, w.err }
