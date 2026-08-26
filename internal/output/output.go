// Package output renders index results as the compact JSON that agents parse.
package output

import (
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/andrewmuldowney/cc-search/internal/transcript"
)

// Result is one message as it appears in CLI output.
type Result struct {
	ID               string `json:"id"`
	SessionID        string `json:"sessionId"`
	Timestamp        string `json:"timestamp"`
	Type             string `json:"type"`
	ActivityID       string `json:"activityId,omitempty"`
	ActivityRole     string `json:"activityRole,omitempty"`
	ParentActivityID string `json:"parentActivityId,omitempty"`
	ParentSessionID  string `json:"parentSessionId,omitempty"`
	ChildSessionID   string `json:"childSessionId,omitempty"`
	Preview          string `json:"preview,omitempty"`
	Content          string `json:"content,omitempty"`
	CharCount        int    `json:"charCount"`
}

// Response is the top-level JSON document.
type Response struct {
	Results   []Result `json:"results"`
	Total     int      `json:"total"`
	Truncated bool     `json:"truncated"`
	// Relaxed reports that no message contained every term, so the search fell
	// back to matching any of them. These are related hits, not exact ones.
	Relaxed bool   `json:"relaxed"`
	Budget  Budget `json:"budget"`
}

// SessionsResponse is the browse result for the sessions command.
type SessionsResponse struct {
	Sessions []Session `json:"sessions"`
	Total    int       `json:"total"`
}

type Session struct {
	SessionID       string `json:"sessionId"`
	CWD             string `json:"cwd"`
	Visibility      string `json:"visibility"`
	ParentSessionID string `json:"parentSessionId"`
	LastTimestamp   string `json:"lastTimestamp"`
	LastPreview     string `json:"lastPreview"`
	MessageCount    int    `json:"messageCount"`
}

// ActivitiesResponse is the list result for the activities command.
type ActivitiesResponse struct {
	Activities []Activity `json:"activities"`
	Total      int        `json:"total"`
}

type Activity struct {
	ActivityID       string `json:"activityId"`
	Status           string `json:"status"`
	Title            string `json:"title"`
	Model            string `json:"model"`
	Effort           string `json:"effort"`
	ToolUses         int    `json:"toolUses"`
	StartedAt        string `json:"startedAt"`
	CompletedAt      string `json:"completedAt"`
	ParentSessionID  string `json:"parentSessionId"`
	ChildSessionID   string `json:"childSessionId"`
	ParentActivityID string `json:"parentActivityId"`
	ResultSummary    string `json:"resultSummary"`
	Description      string `json:"description,omitempty"`
	Kind             string `json:"kind,omitempty"`
	Namespace        string `json:"namespace,omitempty"`
	StartEntryID     string `json:"startEntryId,omitempty"`
	StartParentID    string `json:"startParentId,omitempty"`
	LinkedEntryID    string `json:"linkedEntryId,omitempty"`
	TerminalEntryID  string `json:"terminalEntryId,omitempty"`
	TerminalParentID string `json:"terminalParentId,omitempty"`
}

// CommandsResponse is the list result for the commands command.
type CommandsResponse struct {
	Commands  []Command `json:"commands"`
	Total     int       `json:"total"`
	Truncated bool      `json:"truncated"`
}

type Command struct {
	Tool      string `json:"tool"`
	Arguments any    `json:"arguments"`
	SessionID string `json:"sessionId"`
	MessageID string `json:"messageId"`
	Timestamp string `json:"timestamp"`
	Output    string `json:"output,omitempty"`
}

// ContextResponse contains the selected search hits and one deduplicated
// expansion of all of their surrounding windows.
type ContextResponse struct {
	Query     string          `json:"query"`
	Hits      []Result        `json:"hits"`
	Results   []ContextResult `json:"results"`
	TotalHits int             `json:"totalHits"`
	Total     int             `json:"total"`
	Truncated bool            `json:"truncated"`
	Relaxed   bool            `json:"relaxed"`
	Budget    Budget          `json:"budget"`
}

type ContextResult struct {
	Result
	HitIDs []string `json:"hitIds,omitempty"`
}

// InfoResponse reports index and executable diagnostics.
type InfoResponse struct {
	IndexPath      string   `json:"indexPath"`
	SchemaVersion  int      `json:"schemaVersion"`
	MessageCount   int      `json:"messageCount"`
	SessionCount   int      `json:"sessionCount"`
	ActivityCount  int      `json:"activityCount"`
	FileCount      int      `json:"fileCount"`
	Sources        []Source `json:"sources"`
	BinaryPath     string   `json:"binaryPath"`
	BinaryVersion  string   `json:"binaryVersion"`
	CurrentSession string   `json:"currentSession"`
	IndexHealthy   bool     `json:"indexHealthy"`
	LockHealthy    bool     `json:"lockHealthy"`
}

type Source struct {
	Path         string `json:"path"`
	MessageCount int    `json:"messageCount"`
	SessionCount int    `json:"sessionCount"`
}

type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

type DoctorResponse struct {
	InfoResponse
	OK     bool          `json:"ok"`
	Checks []DoctorCheck `json:"checks"`
}

// Budget tells the caller what the character budget cost it, so it can decide
// whether to widen the query or spend more.
type Budget struct {
	Limit   int  `json:"limit"` // 0 means unlimited
	Spent   int  `json:"spent"`
	Dropped int  `json:"dropped"`
	Shrunk  bool `json:"shrunk"`
}

// Hit reports that the budget, rather than a result limit, shaped the output.
func (b Budget) Hit() bool { return b.Dropped > 0 || b.Shrunk }

// Options controls how messages are rendered.
type Options struct {
	PreviewLength int
	Full          bool
	Truncated     bool
	Relaxed       bool
	// UseProse renders each result from what was said rather than from the
	// full message, which also contains tool calls and their output.
	UseProse bool
	// Budget caps the total characters of rendered body across all results.
	// Zero means unlimited. At least one result is always returned, clipped
	// if it alone exceeds the budget.
	Budget int
}

// DefaultPreviewLength is the preview size when none is requested.
const DefaultPreviewLength = 100

// MinPreviewLength is the shortest preview worth returning. Results are
// dropped rather than shrunk below it.
const MinPreviewLength = 40

// Format renders messages into a Response.
//
// When Options.Budget is set it shapes the result set rather than merely
// truncating it. Previews shrink so that every result still fits, down to
// MinPreviewLength, and only then are results dropped from the end. Under
// Full the opposite is right: bodies were asked for whole, so they are
// emitted whole until the budget runs out.
func Format(msgs []transcript.Message, opts Options) Response {
	length := opts.PreviewLength
	if length <= 0 {
		length = DefaultPreviewLength
	}

	bodies := make([]string, len(msgs))
	natural := make([]int, len(msgs))
	for i, m := range msgs {
		bodies[i] = m.Content
		if opts.UseProse {
			bodies[i] = m.Prose
		}
		if opts.Full {
			natural[i] = utf8.RuneCountInString(bodies[i])
		} else {
			natural[i] = utf8.RuneCountInString(preview(bodies[i], length))
		}
	}

	keep, alloc := plan(natural, opts.Budget, opts.Full)

	resp := Response{
		Results:   []Result{},
		Truncated: opts.Truncated,
		Relaxed:   opts.Relaxed,
		Budget:    Budget{Limit: opts.Budget, Dropped: len(msgs) - keep},
	}
	if resp.Budget.Dropped > 0 {
		resp.Truncated = true
	}

	for i := 0; i < keep; i++ {
		m := msgs[i]
		body := render(bodies[i], alloc[i], natural[i], length, opts.Full)
		if alloc[i] < natural[i] {
			resp.Budget.Shrunk = true
		}
		resp.Budget.Spent += utf8.RuneCountInString(body)

		r := Result{
			ID:               m.ID,
			SessionID:        m.SessionID,
			Timestamp:        time.UnixMilli(m.Timestamp).UTC().Format(time.RFC3339),
			Type:             m.Type,
			ActivityID:       m.ActivityID,
			ActivityRole:     m.ActivityRole,
			ParentActivityID: m.ParentActivityID,
			ParentSessionID:  m.ParentSessionID,
			ChildSessionID:   m.ChildSessionID,
			CharCount:        m.CharCount,
		}
		if opts.UseProse {
			r.CharCount = len(m.Prose)
		}
		if opts.Full {
			r.Content = body
		} else {
			r.Preview = body
		}
		resp.Results = append(resp.Results, r)
	}
	resp.Total = len(resp.Results)
	return resp
}

// plan decides how many results survive the budget and how many characters
// each may spend.
func plan(natural []int, budget int, full bool) (keep int, alloc []int) {
	alloc = make([]int, len(natural))
	if len(natural) == 0 {
		return 0, alloc
	}
	if budget <= 0 {
		copy(alloc, natural)
		return len(natural), alloc
	}

	if full {
		// Fill in rank order with whole bodies. The first result is clipped
		// rather than dropped, so a query never returns nothing.
		spent := 0
		for i, n := range natural {
			if spent+n > budget {
				if i == 0 {
					alloc[0] = budget
					return 1, alloc
				}
				return i, alloc
			}
			alloc[i] = n
			spent += n
		}
		return len(natural), alloc
	}

	keep = len(natural)
	if max := budget / MinPreviewLength; keep > max {
		keep = max
	}
	if keep < 1 {
		keep = 1
	}
	waterFill(natural[:keep], alloc[:keep], budget)
	return keep, alloc
}

// waterFill gives every result an equal share of the budget, handing the
// surplus from results that need less to those that can use more.
func waterFill(natural, alloc []int, budget int) {
	order := make([]int, len(natural))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return natural[order[a]] < natural[order[b]] })

	remaining, left := budget, len(natural)
	for _, i := range order {
		share := remaining / left
		take := natural[i]
		if take > share {
			take = share
		}
		alloc[i] = take
		remaining -= take
		left--
	}
}

// render emits a body within its allocation. Shrinking leaves room for the
// ellipsis so the emitted length never exceeds what was allocated.
func render(body string, alloc, natural, length int, full bool) string {
	if alloc >= natural {
		if full {
			return body
		}
		return preview(body, length)
	}
	if full {
		return clip(body, alloc)
	}
	if alloc < 2 {
		alloc = 2
	}
	return preview(body, alloc-1)
}

// clip cuts a body to at most limit characters on a rune boundary.
func clip(body string, limit int) string {
	runes := []rune(body)
	if len(runes) <= limit {
		return body
	}
	return string(runes[:limit])
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
