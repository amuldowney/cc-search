// Package transcript parses Claude Code and pi JSONL transcript files into
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
	SessionID string
	Timestamp int64 // unix milliseconds
	Type      string
	Content   string
	Prose     string // just what was said: text blocks, no tool calls or results
	CharCount int
}

// record mirrors the subset of a transcript line we care about. Records that
// carry no message (mode, last-prompt, file-history-*, ...) simply leave the
// fields empty and get skipped.
//
// Two schemas are recognised:
//   - Claude Code: {"uuid": ..., "sessionId": ..., "type": "user"|"assistant", ...}
//   - pi: {"id": ..., "type": "message", "message": {"role": ..., "content": ...}}
//     plus a {"type": "session", "id": ...} header line naming the session.
type record struct {
	UUID      string          `json:"uuid"`
	ID        string          `json:"id"` // pi records
	SessionID string          `json:"sessionId"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"` // system records
	Message   *struct {
		Role    string          `json:"role"` // pi records
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// block is one element of a structured content array.
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Arguments json.RawMessage `json:"arguments"` // pi toolCall payload
	Content   json.RawMessage `json:"content"`   // tool_result payload
}

// Parser extracts Messages from the lines of one transcript file. It is
// stateful because pi transcripts declare their session id once, in a header
// line, rather than on every record.
type Parser struct {
	sessionID string
}

// ParseLine extracts a Message from a single JSONL line. It returns ok=false
// for lines that carry no indexable message (metadata records, blank lines,
// corrupt JSON). It is shorthand for a fresh Parser, so pi session-header
// lines only take effect when one Parser sees a whole file in order.
func ParseLine(line []byte) (Message, bool) {
	return new(Parser).ParseLine(line)
}

// ParseLine extracts a Message from one line, remembering any pi session
// header seen so far.
func (p *Parser) ParseLine(line []byte) (Message, bool) {
	var rec record
	if err := json.Unmarshal(line, &rec); err != nil {
		return Message{}, false
	}

	// A pi session header names the session every following line belongs to.
	if rec.Type == "session" {
		if rec.ID != "" {
			p.sessionID = rec.ID
		}
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
	if rec.Message != nil {
		raw = rec.Message.Content
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
		sessionID = p.sessionID
	}

	return Message{
		ID:        id,
		SessionID: sessionID,
		Timestamp: ts.UnixMilli(),
		Type:      typ,
		Content:   content,
		Prose:     prose,
		CharCount: len(content),
	}, true
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
