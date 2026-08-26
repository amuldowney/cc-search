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
	if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Name != "Bash" || msg.ToolCalls[0].Arguments != `{"command":"ls -la"}` {
		t.Errorf("ToolCalls = %+v", msg.ToolCalls)
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
	if len(msg.ToolResults) != 1 || msg.ToolResults[0].ID != "t1" || msg.ToolResults[0].Content != "total 108" {
		t.Errorf("ToolResults = %+v", msg.ToolResults)
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

func TestParseLinePreservesRecapTextAsOrdinaryMessage(t *testing.T) {
	line := []byte(`{"type":"assistant","uuid":"m1","sessionId":"s1",` +
		`"timestamp":"2026-08-14T00:00:00Z","message":{"content":"recap: the index is healthy"}}`)

	msg, ok := ParseLine(line)
	if !ok {
		t.Fatal("ParseLine returned ok = false")
	}
	if msg.Content != "recap: the index is healthy" || msg.Prose != msg.Content {
		t.Fatalf("message = %+v, want ordinary content and prose", msg)
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

func TestParseLineParsesPiUserMessage(t *testing.T) {
	line := []byte(`{"type":"message","id":"f85d4231","parentId":"cf9b145e",
		"timestamp":"2026-07-31T23:34:18.152Z",
		"message":{"role":"user","content":[{"type":"text","text":"fix the hub retry loop"}]}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.ID != "f85d4231" {
		t.Errorf("ID = %q, want %q", msg.ID, "f85d4231")
	}
	if msg.Type != "user" {
		t.Errorf("Type = %q, want %q", msg.Type, "user")
	}
	if msg.Content != "fix the hub retry loop" {
		t.Errorf("Content = %q", msg.Content)
	}
	if msg.Prose != "fix the hub retry loop" {
		t.Errorf("Prose = %q", msg.Prose)
	}
}

func TestParserTakesSessionIDFromPiHeader(t *testing.T) {
	p := new(Parser)
	header := []byte(`{"type":"session","version":3,"id":"019fba87-2873",
		"timestamp":"2026-07-31T23:34:07.731Z","cwd":"/home/andrew/Projects/pi-remote"}`)
	if _, ok := p.ParseLine(header); ok {
		t.Fatal("session header should not produce a message")
	}

	line := []byte(`{"type":"message","id":"f85d4231",
		"timestamp":"2026-07-31T23:34:18.152Z",
		"message":{"role":"assistant","content":[{"type":"text","text":"done"}]}}`)
	msg, ok := p.ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.SessionID != "019fba87-2873" {
		t.Errorf("SessionID = %q, want %q", msg.SessionID, "019fba87-2873")
	}
}

func TestParserReadsAttachedActivityLinks(t *testing.T) {
	p := new(Parser)
	lines := []string{
		`{"type":"session","version":3,"id":"child-session","timestamp":"2026-08-25T02:34:48.858Z","cwd":"/repo","parentSession":"/repo/parent.jsonl","visibility":"attached"}`,
		`{"type":"message","id":"call-1","parentId":null,"timestamp":"2026-08-25T02:34:49Z","message":{"role":"assistant","content":"launching"}}`,
		`{"type":"custom","customType":"pi:activity-started","id":"start-1","parentId":"call-1","timestamp":"2026-08-25T02:34:49Z","data":{"activityId":"activity-1","parentActivityId":"parent-activity","kind":"agent","namespace":"test","status":"running","startedAt":1787625289000,"title":"Explore","description":"inspect code"}}`,
		`{"type":"custom","customType":"pi:activity-linked","id":"link-1","parentId":"start-1","timestamp":"2026-08-25T02:34:50Z","data":{"activityId":"activity-1","sessionFile":"/repo/child.jsonl"}}`,
		`{"type":"message","id":"result-1","parentId":"link-1","timestamp":"2026-08-25T02:34:51Z","message":{"role":"toolResult","details":{"agentId":"activity-1"},"content":[{"type":"text","text":"started"}]}}`,
	}
	for _, line := range lines {
		_, _ = p.ParseLine([]byte(line))
	}

	session := p.Session()
	if session.ID != "child-session" || session.ParentSession != "/repo/parent.jsonl" || session.Visibility != "attached" {
		t.Fatalf("session = %+v", session)
	}
	events := p.ActivityEvents()
	if len(events) != 2 || events[0].Kind != "started" || events[1].Kind != "linked" {
		t.Fatalf("events = %+v", events)
	}
	if events[0].ParentID != "call-1" || events[0].ParentActivityID != "parent-activity" {
		t.Fatalf("start event = %+v", events[0])
	}
	if events[1].SessionFile != "/repo/child.jsonl" {
		t.Fatalf("linked event = %+v", events[1])
	}

	msg, ok := p.ParseLine([]byte(`{"type":"message","id":"result-2","timestamp":"2026-08-25T02:34:52Z","message":{"role":"toolResult","details":{"agentId":"activity-1"},"content":[{"type":"text","text":"done"}]}}`))
	if !ok || msg.ActivityID != "activity-1" || msg.ActivityRole != "response" {
		t.Fatalf("message = %+v, ok = %v", msg, ok)
	}
}

func TestParseLineRendersPiToolCallBlock(t *testing.T) {
	line := []byte(`{"type":"message","id":"a1",
		"timestamp":"2026-07-31T23:34:19.197Z",
		"message":{"role":"assistant","content":[
			{"type":"toolCall","id":"call_1","name":"bash","arguments":{"command":"ls -la"}}]}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	want := `[tool: bash] {"command":"ls -la"}`
	if msg.Content != want {
		t.Errorf("Content = %q, want %q", msg.Content, want)
	}
	if msg.Prose != "" {
		t.Errorf("Prose = %q, want empty", msg.Prose)
	}
}

func TestParseLineHidesPiToolResultFromProse(t *testing.T) {
	line := []byte(`{"type":"message","id":"t1",
		"timestamp":"2026-07-31T23:34:20.000Z",
		"message":{"role":"toolResult","toolCallId":"call_1","toolName":"bash",
			"content":[{"type":"text","text":"file.go\nother.go"}]}}`)

	msg, ok := ParseLine(line)

	if !ok {
		t.Fatal("expected line to parse")
	}
	if msg.Type != "toolResult" {
		t.Errorf("Type = %q, want %q", msg.Type, "toolResult")
	}
	if msg.Content != "file.go\nother.go" {
		t.Errorf("Content = %q", msg.Content)
	}
	if msg.Prose != "" {
		t.Errorf("Prose = %q, want empty so --all is required to see it", msg.Prose)
	}
}

func TestParseTaskNotificationProducesActivityEvent(t *testing.T) {
	p := new(Parser)
	line := []byte(`{"type":"queue-operation","operation":"remove","sessionId":"session-a","timestamp":"2026-08-25T03:00:00Z","content":"<task-notification>\n<task-id>task-1</task-id>\n<output-file>/tmp/task.output</output-file>\n<status>failed</status>\n<summary>Agent \"Explore\" stopped</summary>\n</task-notification>"}`)
	if _, ok := p.ParseLine(line); ok {
		t.Fatal("task notification should not become a searchable message")
	}
	events := p.ActivityEvents()
	if len(events) != 1 || events[0].ActivityID != "task-1" || events[0].Status != "failed" || events[0].Title != `Agent "Explore" stopped` || events[0].Description != "/tmp/task.output" {
		t.Fatalf("events = %+v", events)
	}
}

func TestParseLineSkipsPiMetadataRecords(t *testing.T) {
	lines := []string{
		`{"type":"model_change","id":"03ad8c20","timestamp":"2026-07-31T23:34:07.815Z","modelId":"glm-5.2"}`,
		`{"type":"thinking_level_change","id":"cf9b145e","timestamp":"2026-07-31T23:34:07.815Z","thinkingLevel":"medium"}`,
	}
	for _, line := range lines {
		if _, ok := ParseLine([]byte(line)); ok {
			t.Errorf("expected %s to be skipped", line)
		}
	}
}
