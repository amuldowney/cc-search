package transcript

import (
	"strings"
	"testing"
)

func TestParseLineExtractsUserStringContent(t *testing.T) {
	line := []byte(`{"type":"user","uuid":"9d66de75","sessionId":"62de7038",
		"timestamp":"2026-07-23T03:51:29.975Z",
		"message":{"role":"user","content":"implement the conversation cli"}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.ID != "9d66de75" {
		t.Errorf("ID = %q, want %q", msg.ID, "9d66de75")
	}
	if msg.SessionID != "62de7038" {
		t.Errorf("SessionID = %q, want %q", msg.SessionID, "62de7038")
	}
	if msg.Type != "user" {
		t.Errorf("Type = %q, want %q", msg.Type, "user")
	}
	if msg.Content != "implement the conversation cli" {
		t.Errorf("Content = %q", msg.Content)
	}
	if msg.Timestamp != 1784778689975 {
		t.Errorf("Timestamp = %d, want 1784778689975", msg.Timestamp)
	}
	if msg.CharCount != len("implement the conversation cli") {
		t.Errorf("CharCount = %d", msg.CharCount)
	}
}

func TestParseLineJoinsMultipleContentBlocks(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"c14e020e","sessionId":"62de7038",
		"timestamp":"2026-07-23T03:51:36.301Z",
		"message":{"role":"assistant","content":[
			{"type":"thinking","thinking":"weighing the options"},
			{"type":"text","text":"Here is the answer"}]}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	want := "weighing the options\nHere is the answer"
	if msg.Content != want {
		t.Errorf("Content = %q, want %q", msg.Content, want)
	}
}

func TestParseLineRendersToolUseBlock(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"a1","sessionId":"s1",
		"timestamp":"2026-07-23T03:51:36.301Z",
		"message":{"content":[
			{"type":"tool_use","name":"Bash","input":{"command":"ls -la"}}]}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	want := `[tool: Bash] {"command":"ls -la"}`
	if msg.Content != want {
		t.Errorf("Content = %q, want %q", msg.Content, want)
	}
}

func TestParseLineExtractsToolResultContent(t *testing.T) {
	line := []byte(`{"type":"user","uuid":"a2","sessionId":"s1",
		"timestamp":"2026-07-23T03:51:36.301Z",
		"message":{"content":[
			{"type":"tool_result","tool_use_id":"t1","content":"total 108"}]}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.Content != "total 108" {
		t.Errorf("Content = %q, want %q", msg.Content, "total 108")
	}
}

func TestParseLineExtractsToolResultBlockList(t *testing.T) {
	line := []byte(`{"type":"user","uuid":"a3","sessionId":"s1",
		"timestamp":"2026-07-23T03:51:36.301Z",
		"message":{"content":[
			{"type":"tool_result","content":[{"type":"text","text":"nested result"}]}]}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.Content != "nested result" {
		t.Errorf("Content = %q, want %q", msg.Content, "nested result")
	}
}

func TestParseLineExtractsSystemTopLevelContent(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"bridge_status","uuid":"054f6bfb",
		"sessionId":"2177e4a4","timestamp":"2026-07-21T23:20:42.267Z",
		"content":"/remote-control is active"}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.Type != "system" {
		t.Errorf("Type = %q, want %q", msg.Type, "system")
	}
	if msg.Content != "/remote-control is active" {
		t.Errorf("Content = %q", msg.Content)
	}
}

func TestParseLineSkipsMetadataRecords(t *testing.T) {
	cases := map[string]string{
		"no uuid":      `{"type":"last-prompt","lastPrompt":"hello","sessionId":"s1"}`,
		"no timestamp": `{"type":"user","uuid":"u1","sessionId":"s1","message":{"content":"hi"}}`,
		"empty content": `{"type":"assistant","uuid":"u2","sessionId":"s1",
			"timestamp":"2026-07-23T03:51:36.301Z","message":{"content":[]}}`,
		"corrupt json": `{"type":"user","uuid":`,
		"blank line":   ``,
	}

	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			if _, ok := ParseLine([]byte(line)); ok {
				t.Errorf("expected line to be skipped")
			}
		})
	}
}

func TestParseLineFlagsAssistantRecap(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"r1","sessionId":"s1",
		"timestamp":"2026-07-23T03:51:36.301Z",
		"message":{"content":[{"type":"text","text":"To Recap: we built the index"}]}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if !msg.IsRecap {
		t.Error("IsRecap = false, want true for assistant message containing 'recap'")
	}
}

func TestParseLineSeparatesProseFromToolBlocks(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"p1","sessionId":"s1",
		"timestamp":"2026-07-23T03:51:36.301Z",
		"message":{"content":[
			{"type":"thinking","thinking":"internal deliberation"},
			{"type":"tool_use","name":"Bash","input":{"command":"ls"}},
			{"type":"text","text":"Here is what I found"}]}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.Prose != "Here is what I found" {
		t.Errorf("Prose = %q, want only the text block", msg.Prose)
	}
	if !strings.Contains(msg.Content, "[tool: Bash]") {
		t.Errorf("Content = %q, want the tool call still indexed", msg.Content)
	}
}

func TestParseLineTreatsStringContentAsProse(t *testing.T) {
	line := []byte(`{"type":"user","uuid":"p2","sessionId":"s1",
		"timestamp":"2026-07-23T03:51:36.301Z",
		"message":{"content":"restart the caddy container"}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.Prose != "restart the caddy container" {
		t.Errorf("Prose = %q", msg.Prose)
	}
}

func TestParseLineDoesNotFlagRecapInsideToolBlocks(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"r3","sessionId":"s1",
		"timestamp":"2026-07-23T03:51:36.301Z",
		"message":{"content":[
			{"type":"tool_use","name":"Bash","input":{"command":"cc-search --recaps-only"}},
			{"type":"text","text":"Here are the results"}]}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.IsRecap {
		t.Error("IsRecap = true; a tool call that mentions recaps is not a recap")
	}
}

func TestParseLineDoesNotFlagUserRecap(t *testing.T) {
	line := []byte(`{"type":"user","uuid":"r2","sessionId":"s1",
		"timestamp":"2026-07-23T03:51:36.301Z",
		"message":{"content":"give me a recap"}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.IsRecap {
		t.Error("IsRecap = true, want false for user messages")
	}
}
