// Package output renders index results as the compact JSON that agents parse.
package output

import (
	"strings"
	"time"

	"github.com/andrewmuldowney/cc-search/internal/transcript"
)

// Result is one message as it appears in CLI output.
type Result struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Timestamp string `json:"timestamp"`
	Type      string `json:"type"`
	IsRecap   bool   `json:"isRecap"`
	Preview   string `json:"preview"`
	Content   string `json:"content,omitempty"`
	CharCount int    `json:"charCount"`
}

// Response is the top-level JSON document.
type Response struct {
	Results    []Result `json:"results"`
	Total      int      `json:"total"`
	RecapCount int      `json:"recapCount"`
	Truncated  bool     `json:"truncated"`
}

// Options controls how messages are rendered.
type Options struct {
	PreviewLength int
	Full          bool
	Truncated     bool
}

// DefaultPreviewLength is the preview size when none is requested.
const DefaultPreviewLength = 100

// Format renders messages into a Response.
func Format(msgs []transcript.Message, opts Options) Response {
	length := opts.PreviewLength
	if length <= 0 {
		length = DefaultPreviewLength
	}

	resp := Response{Results: []Result{}, Truncated: opts.Truncated}
	for _, m := range msgs {
		r := Result{
			ID:        m.ID,
			SessionID: m.SessionID,
			Timestamp: time.UnixMilli(m.Timestamp).UTC().Format(time.RFC3339),
			Type:      m.Type,
			IsRecap:   m.IsRecap,
			Preview:   preview(m.Content, length),
			CharCount: m.CharCount,
		}
		if opts.Full {
			r.Content = m.Content
		}
		resp.Results = append(resp.Results, r)
		if m.IsRecap {
			resp.RecapCount++
		}
	}
	resp.Total = len(resp.Results)
	return resp
}

// preview collapses a message to a single line of at most length runes so
// results stay compact in an agent's context window.
func preview(content string, length int) string {
	flat := strings.Join(strings.Fields(content), " ")
	runes := []rune(flat)
	if len(runes) <= length {
		return flat
	}
	return string(runes[:length]) + "…"
}
