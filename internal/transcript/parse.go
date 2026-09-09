// Package transcript parses Claude Code, pi, and Codex JSONL transcript files into
// indexable messages.
package transcript

import (
	"encoding/json"
	"strings"
	"time"
)

// Message is a single indexable message extracted from a transcript line.
type Message struct {
	ID        string
	EntryID   string // original transcript entry id, before any index disambiguation
	SessionID string
	Timestamp int64 // unix milliseconds
	Type      string
	Content   string
	Prose     string // just what was said: text blocks, no tool calls or results
	CharCount int

	// ToolCalls and ToolResults retain the structured tool records that are
	// otherwise flattened into Content. They let command history pair an exact
	// invocation with its output without reparsing the source transcript.
	ToolCalls   []ToolCall
	ToolResults []ToolResult

	// ActivityID and ActivityRole link parent-session Agent rows and child
	// session messages without turning activity metadata into fake messages.
	ActivityID       string
	ActivityRole     string // start, response, terminal, or child
	ParentActivityID string
	ParentSessionID  string
	ChildSessionID   string
}

// Session describes session metadata seen in a pi or Codex transcript.
// Claude Code transcripts do not have a corresponding header.
type Session struct {
	ID            string
	ParentSession string
	CWD           string
	Visibility    string
}

// ToolCall is one structured tool invocation in a message.
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolResult is the output belonging to a tool invocation.
type ToolResult struct {
	ID      string
	Name    string
	Content string
	IsError bool
}

// ActivityEvent is a durable pi activity marker from a parent session.
type ActivityEvent struct {
	Kind             string
	EntryID          string
	ParentID         string
	Timestamp        int64
	ActivityID       string
	ParentActivityID string
	SessionFile      string
	Status           string
	StartedAt        int64
	CompletedAt      int64
	Title            string
	Description      string
	ActivityKind     string
	Namespace        string
	Model            string
	Effort           string
	ResultSummary    string
	ToolUses         int
}

// record mirrors the subset of a transcript line we care about. Records that
// carry no message (mode, last-prompt, file-history-*, ...) simply leave the
// fields empty and get skipped.
//
// Three schemas are recognised:
//   - Claude Code: {"uuid": ..., "sessionId": ..., "type": "user"|"assistant", ...}
//   - pi: {"id": ..., "type": "message", "message": {"role": ..., "content": ...}}
//     plus a {"type": "session", "id": ...} header line naming the session.
//   - Codex: {"type": "response_item"|"event_msg", "payload": ...} rollout lines.
type record struct {
	UUID          string          `json:"uuid"`
	ID            string          `json:"id"` // pi records
	ParentID      string          `json:"parentId"`
	SessionID     string          `json:"sessionId"`
	Timestamp     string          `json:"timestamp"`
	Type          string          `json:"type"`
	CWD           string          `json:"cwd"`
	ParentSession string          `json:"parentSession"`
	Visibility    string          `json:"visibility"`
	CustomType    string          `json:"customType"`
	Data          json.RawMessage `json:"data"`
	Payload       json.RawMessage `json:"payload"` // Codex rollout records
	Item          json.RawMessage `json:"item"`    // older Codex response_item records
	Content       json.RawMessage `json:"content"` // system/custom_message records
	Details       json.RawMessage `json:"details"`
	Message       *recordMessage  `json:"message"`
}

// recordMessage accepts the object used by Claude/pi transcripts, while also
// retaining a scalar legacy message value used by some Codex event records.
type recordMessage struct {
	Role       string          `json:"role"` // pi records
	Content    json.RawMessage `json:"content"`
	Details    json.RawMessage `json:"details"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	Raw        json.RawMessage `json:"-"`
}

func (m *recordMessage) UnmarshalJSON(data []byte) error {
	m.Raw = append(m.Raw[:0], data...)
	if len(data) == 0 || string(data) == "null" || data[0] != '{' {
		return nil
	}
	type plain recordMessage
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	decoded.Raw = append(decoded.Raw[:0], data...)
	*m = recordMessage(decoded)
	return nil
}

// block is one element of a structured content array.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	ToolUseID string          `json:"tool_use_id"`
	Input     json.RawMessage `json:"input"`
	Arguments json.RawMessage `json:"arguments"` // pi toolCall payload
	Content   json.RawMessage `json:"content"`   // tool_result payload
	IsError   bool            `json:"is_error"`
}

// Parser extracts Messages from the lines of one transcript file. It is
// stateful because pi and Codex transcripts declare session metadata once,
// rather than on every record.
type Parser struct {
	session  Session
	activity []ActivityEvent
	codex    bool

	// Codex emits event_msg mirrors for many response_item messages. Keep the
	// canonical prose seen so far so the mirror can be skipped without making
	// event-only records disappear.
	codexCanonical map[string]struct{}
	codexEvents    map[string]struct{}
}

// Session returns parsed session metadata, if one was seen.
func (p *Parser) Session() Session { return p.session }

// IsCodex reports whether this parser has seen a Codex rollout record. It lets
// the index preserve Codex's globally useful response-item IDs while retaining
// the older Pi filename disambiguation rule.
func (p *Parser) IsCodex() bool { return p.codex }

// ActivityEvents returns a copy of the activity markers seen in this file.
func (p *Parser) ActivityEvents() []ActivityEvent {
	return append([]ActivityEvent(nil), p.activity...)
}

// ParseLine extracts a Message from a single JSONL line. It returns ok=false
// for lines that carry no indexable message (metadata records, blank lines,
// corrupt JSON). It is shorthand for a fresh Parser, so pi session-header
// lines only take effect when one Parser sees a whole file in order.
func ParseLine(line []byte) (Message, bool) {
	return new(Parser).ParseLine(line)
}

// ParseLine extracts a Message from one line, remembering pi/Codex session
// metadata seen so far.
func (p *Parser) ParseLine(line []byte) (Message, bool) {
	var rec record
	if err := json.Unmarshal(line, &rec); err != nil {
		return Message{}, false
	}

	// Codex rollout records use a distinct envelope and must be handled before
	// the Claude/pi shape detection below. In particular, session_meta is
	// useful session metadata but is never a conversational message.
	if isCodexRecord(rec) {
		if msg, ok := p.parseCodexLine(rec, line); ok {
			return msg, true
		}
		// Once a line has the Codex envelope, do not let a metadata record
		// fall through to the Claude/pi parser just because it also has an id
		// or a textual field.
		if len(rec.Payload) > 0 || len(rec.Item) > 0 || rec.Type == "session_meta" ||
			rec.Type == "turn_context" || rec.Type == "compacted" || rec.Type == "token_count" ||
			rec.Type == "event_msg" || rec.Type == "user_message" || rec.Type == "agent_message" {
			return Message{}, false
		}
	}

	// A pi session header names the session every following line belongs to.
	if rec.Type == "session" {
		p.session = Session{
			ID:            rec.ID,
			ParentSession: rec.ParentSession,
			CWD:           rec.CWD,
			Visibility:    rec.Visibility,
		}
		return Message{}, false
	}
	if p.session.ID == "" && rec.SessionID != "" {
		p.session.ID = rec.SessionID
	}
	if p.session.CWD == "" && rec.CWD != "" {
		p.session.CWD = rec.CWD
	}

	if event, ok := parseActivityEvent(rec); ok {
		p.activity = append(p.activity, event)
		return Message{}, false
	}

	id := rec.UUID
	if id == "" {
		id = rec.ID
	}
	if id == "" || rec.Timestamp == "" {
		return Message{}, false
	}

	ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return Message{}, false
	}

	typ := rec.Type
	raw := rec.Content
	messageDetails := rec.Details
	if rec.Message != nil && len(rec.Message.Content) > 0 {
		raw = rec.Message.Content
		messageDetails = rec.Message.Details
		if rec.Type == "message" { // pi: the role carries the message type
			typ = rec.Message.Role
		}
	}
	content := extractContent(raw)
	if content == "" {
		return Message{}, false
	}

	prose := strings.TrimSpace(spokenText(raw))
	if typ == "toolResult" {
		// pi tool results are text blocks, which spokenText would otherwise
		// mistake for something said aloud. They are tool output: searchable
		// with --all, hidden from the default prose view.
		prose = ""
	}

	sessionID := rec.SessionID
	if sessionID == "" {
		sessionID = p.session.ID
	}
	if p.session.ID == "" && sessionID != "" {
		p.session.ID = sessionID
	}
	if p.session.CWD == "" && rec.CWD != "" {
		p.session.CWD = rec.CWD
	}

	calls, results := extractTools(raw, rec.Message)
	activityID, activityRole := activityFromDetails(messageDetails,
		rec.Type == "custom_message" && rec.CustomType == "subagent-notification")
	return Message{
		ID:           id,
		EntryID:      id,
		SessionID:    sessionID,
		Timestamp:    ts.UnixMilli(),
		Type:         typ,
		Content:      content,
		Prose:        prose,
		CharCount:    len(content),
		ToolCalls:    calls,
		ToolResults:  results,
		ActivityID:   activityID,
		ActivityRole: activityRole,
	}, true
}

func extractTools(raw json.RawMessage, message *recordMessage) ([]ToolCall, []ToolResult) {
	var calls []ToolCall
	var results []ToolResult
	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err == nil {
		for _, b := range blocks {
			switch b.Type {
			case "tool_use", "toolCall":
				args := b.Input
				if len(args) == 0 {
					args = b.Arguments
				}
				calls = append(calls, ToolCall{
					ID: firstNonEmpty(b.ID, b.ToolUseID), Name: b.Name, Arguments: string(args),
				})
			case "tool_result":
				results = append(results, ToolResult{
					ID: b.ToolUseID, Content: extractContent(b.Content), IsError: b.IsError,
				})
			}
		}
	}
	if message != nil && message.Role == "toolResult" &&
		(message.ToolCallID != "" || message.ToolName != "") {
		// Pi stores result metadata beside its text content rather than in a
		// tool_result block.
		results = append(results, ToolResult{
			ID: message.ToolCallID, Name: message.ToolName, Content: extractContent(raw),
		})
	}
	return calls, results
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func parseActivityEvent(rec record) (ActivityEvent, bool) {
	if event, ok := parseTaskNotification(rec); ok {
		return event, true
	}
	if rec.Type != "custom" {
		return ActivityEvent{}, false
	}
	if rec.CustomType != "pi:activity-started" &&
		rec.CustomType != "pi:activity-linked" &&
		rec.CustomType != "pi:activity-terminal" {
		return ActivityEvent{}, false
	}

	var data struct {
		ActivityID       string `json:"activityId"`
		ParentActivityID string `json:"parentActivityId"`
		SessionFile      string `json:"sessionFile"`
		Status           string `json:"status"`
		StartedAt        int64  `json:"startedAt"`
		CompletedAt      int64  `json:"completedAt"`
		Title            string `json:"title"`
		Description      string `json:"description"`
		Kind             string `json:"kind"`
		Namespace        string `json:"namespace"`
		Model            string `json:"model"`
		Effort           string `json:"effort"`
		ResultSummary    string `json:"resultSummary"`
		ToolUses         int    `json:"toolUses"`
	}
	if err := json.Unmarshal(rec.Data, &data); err != nil || data.ActivityID == "" {
		return ActivityEvent{}, false
	}

	ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return ActivityEvent{}, false
	}
	if data.StartedAt == 0 {
		data.StartedAt = ts.UnixMilli()
	}
	if data.CompletedAt == 0 && rec.CustomType == "pi:activity-terminal" {
		data.CompletedAt = ts.UnixMilli()
	}
	kind := strings.TrimPrefix(rec.CustomType, "pi:activity-")
	return ActivityEvent{
		Kind:             kind,
		EntryID:          rec.ID,
		ParentID:         rec.ParentID,
		Timestamp:        ts.UnixMilli(),
		ActivityID:       data.ActivityID,
		ParentActivityID: data.ParentActivityID,
		SessionFile:      data.SessionFile,
		Status:           data.Status,
		StartedAt:        data.StartedAt,
		CompletedAt:      data.CompletedAt,
		Title:            data.Title,
		Description:      data.Description,
		ActivityKind:     data.Kind,
		Namespace:        data.Namespace,
		Model:            data.Model,
		Effort:           data.Effort,
		ResultSummary:    data.ResultSummary,
		ToolUses:         data.ToolUses,
	}, true
}

func parseTaskNotification(rec record) (ActivityEvent, bool) {
	if rec.Type != "queue-operation" && rec.Type != "user" && rec.Type != "custom_message" {
		return ActivityEvent{}, false
	}
	raw := rec.Content
	if rec.Message != nil {
		raw = rec.Message.Content
	}
	text := extractContent(raw)
	if !strings.Contains(text, "<task-notification>") {
		return ActivityEvent{}, false
	}
	taskID := taskTag(text, "task-id")
	if taskID == "" {
		return ActivityEvent{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return ActivityEvent{}, false
	}
	status := taskTag(text, "status")
	if status == "" {
		status = "running"
	}
	completed := int64(0)
	if status != "running" {
		completed = ts.UnixMilli()
	}
	summary := taskTag(text, "summary")
	return ActivityEvent{
		Kind: "task-notification", EntryID: firstNonEmpty(rec.UUID, rec.ID, taskID),
		ParentID: rec.ParentID, Timestamp: ts.UnixMilli(), ActivityID: taskID,
		Status: status, StartedAt: ts.UnixMilli(), CompletedAt: completed,
		Title: summary, Description: taskTag(text, "output-file"),
		ActivityKind: "task", Namespace: "claude", ResultSummary: summary,
	}, true
}

func taskTag(text, name string) string {
	start := strings.Index(text, "<"+name+">")
	if start < 0 {
		return ""
	}
	start += len(name) + 2
	end := strings.Index(text[start:], "</"+name+">")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

func activityFromDetails(raw json.RawMessage, allowGenericID bool) (string, string) {
	if len(raw) == 0 {
		return "", ""
	}
	var details struct {
		AgentID string `json:"agentId"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(raw, &details); err != nil {
		return "", ""
	}
	if details.AgentID != "" {
		return details.AgentID, "response"
	}
	if allowGenericID && details.ID != "" {
		return details.ID, "response"
	}
	return "", ""
}

// spokenText returns only the prose of a content field: the whole thing when
// it is a plain string, otherwise just its text blocks.
func spokenText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}

	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, b := range blocks {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// extractContent flattens a content field that may be a plain string or an
// array of typed blocks into one searchable string.
func extractContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}

	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}

	var blocks []block
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}

	var parts []string
	for _, b := range blocks {
		if part := renderBlock(b); part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "\n")
}

func renderBlock(b block) string {
	switch b.Type {
	case "text":
		return strings.TrimSpace(b.Text)
	case "thinking":
		return strings.TrimSpace(b.Thinking)
	case "tool_use":
		if len(b.Input) == 0 {
			return "[tool: " + b.Name + "]"
		}
		return "[tool: " + b.Name + "] " + string(b.Input)
	case "toolCall": // pi
		if len(b.Arguments) == 0 {
			return "[tool: " + b.Name + "]"
		}
		return "[tool: " + b.Name + "] " + string(b.Arguments)
	case "tool_result":
		return extractContent(b.Content)
	default:
		return ""
	}
}
