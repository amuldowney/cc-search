// Package transcript parses Claude Code JSONL transcript files into indexable
// messages.
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
	CharCount int
	IsRecap   bool
}

// record mirrors the subset of a transcript line we care about. Records that
// carry no message (mode, last-prompt, file-history-*, ...) simply leave the
// fields empty and get skipped.
type record struct {
	UUID      string          `json:"uuid"`
	SessionID string          `json:"sessionId"`
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Content   json.RawMessage `json:"content"` // system records
	Message   *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// block is one element of a structured content array.
type block struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Content  json.RawMessage `json:"content"` // tool_result payload
}

// ParseLine extracts a Message from a single JSONL line. It returns ok=false
// for lines that carry no indexable message (metadata records, blank lines,
// corrupt JSON).
func ParseLine(line []byte) (Message, bool) {
	var rec record
	if err := json.Unmarshal(line, &rec); err != nil {
		return Message{}, false
	}
	if rec.UUID == "" || rec.Timestamp == "" {
		return Message{}, false
	}

	ts, err := time.Parse(time.RFC3339Nano, rec.Timestamp)
	if err != nil {
		return Message{}, false
	}

	raw := rec.Content
	if rec.Message != nil {
		raw = rec.Message.Content
	}
	content := extractContent(raw)
	if content == "" {
		return Message{}, false
	}

	// A recap is something the assistant *said*, so only its prose counts —
	// a tool call whose arguments mention recaps is not one.
	isRecap := rec.Type == "assistant" &&
		strings.Contains(strings.ToLower(spokenText(raw)), "recap")

	return Message{
		ID:        rec.UUID,
		SessionID: rec.SessionID,
		Timestamp: ts.UnixMilli(),
		Type:      rec.Type,
		Content:   content,
		CharCount: len(content),
		IsRecap:   isRecap,
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
	case "tool_result":
		return extractContent(b.Content)
	default:
		return ""
	}
}
