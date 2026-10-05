package output

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// WriteText renders supported search responses as readable plain text. Other
// values are rendered as indented JSON, preserving diagnostics and future
// response types without changing their schema.
func WriteText(w io.Writer, value any) error {
	var rendered bytes.Buffer
	var err error

	switch v := value.(type) {
	case Response:
		err = renderResponse(&rendered, v)
	case *Response:
		if v == nil {
			return writeJSONText(w, value)
		}
		err = renderResponse(&rendered, *v)
	case ContextResponse:
		err = renderContext(&rendered, v)
	case *ContextResponse:
		if v == nil {
			return writeJSONText(w, value)
		}
		err = renderContext(&rendered, *v)
	case CommandsResponse:
		err = renderCommands(&rendered, v)
	case *CommandsResponse:
		if v == nil {
			return writeJSONText(w, value)
		}
		err = renderCommands(&rendered, *v)
	case SessionsResponse:
		err = renderSessions(&rendered, v)
	case *SessionsResponse:
		if v == nil {
			return writeJSONText(w, value)
		}
		err = renderSessions(&rendered, *v)
	case ActivitiesResponse:
		renderActivities(&rendered, v)
	case *ActivitiesResponse:
		if v == nil {
			return writeJSONText(w, value)
		}
		renderActivities(&rendered, *v)
	case Activity:
		renderActivity(&rendered, v)
	case *Activity:
		if v == nil {
			return writeJSONText(w, value)
		}
		renderActivity(&rendered, *v)
	default:
		return writeJSONText(w, value)
	}
	if err != nil {
		return err
	}
	return writeAll(w, rendered.Bytes())
}

func renderResponse(out *bytes.Buffer, response Response) error {
	writeSearchNotices(out, response.Relaxed, response.Truncated, response.Budget)
	fmt.Fprintf(out, "Results (%d):\n", len(response.Results))
	if len(response.Results) == 0 {
		out.WriteString("  None\n")
		return nil
	}
	for i, result := range response.Results {
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(out, "[%d]\n", i+1)
		renderResult(out, result)
	}
	return nil
}

func renderContext(out *bytes.Buffer, response ContextResponse) error {
	if response.Query != "" {
		writeLine(out, "Query", response.Query)
	}
	writeSearchNotices(out, response.Relaxed, response.Truncated, response.Budget)
	fmt.Fprintf(out, "Hits (%d):\n", len(response.Hits))
	if len(response.Hits) == 0 {
		out.WriteString("  None\n")
	} else {
		for i, hit := range response.Hits {
			if i > 0 {
				out.WriteByte('\n')
			}
			fmt.Fprintf(out, "[Hit %d]\n", i+1)
			renderResult(out, hit)
		}
	}

	out.WriteByte('\n')
	fmt.Fprintf(out, "Context messages (%d):\n", len(response.Results))
	if len(response.Results) == 0 {
		out.WriteString("  None\n")
		return nil
	}
	hitIDs := make(map[string]struct{}, len(response.Hits))
	for _, hit := range response.Hits {
		hitIDs[hit.ID] = struct{}{}
	}
	for i, result := range response.Results {
		if i > 0 {
			out.WriteByte('\n')
		}
		_, isHit := hitIDs[result.ID]
		kind := "Neighbor"
		if isHit {
			kind = "Hit"
		}
		fmt.Fprintf(out, "[Context %d - %s]\n", i+1, kind)
		if len(result.HitIDs) > 0 {
			ids := make([]string, len(result.HitIDs))
			for n, id := range result.HitIDs {
				ids[n] = safeLine(id)
			}
			fmt.Fprintf(out, "Hit IDs: %s\n", strings.Join(ids, ", "))
		}
		renderResult(out, result.Result)
	}
	return nil
}

func renderResult(out *bytes.Buffer, result Result) {
	writeLine(out, "Message ID", result.ID)
	writeLine(out, "Session ID", result.SessionID)
	writeLine(out, "Timestamp (UTC)", result.Timestamp)
	writeLine(out, "Type", result.Type)
	if result.ActivityID != "" {
		writeLine(out, "Activity ID", result.ActivityID)
	}
	if result.ActivityRole != "" {
		writeLine(out, "Activity role", result.ActivityRole)
	}
	if result.ParentActivityID != "" {
		writeLine(out, "Parent activity ID", result.ParentActivityID)
	}
	if result.ParentSessionID != "" {
		writeLine(out, "Parent session ID", result.ParentSessionID)
	}
	if result.ChildSessionID != "" {
		writeLine(out, "Child session ID", result.ChildSessionID)
	}
	if result.CharCount > 0 {
		fmt.Fprintf(out, "Character count: %d\n", result.CharCount)
	}
	if result.Content != "" {
		writeBody(out, "Body", result.Content)
	} else if result.Preview != "" {
		writeBody(out, "Body (preview)", result.Preview)
	} else {
		out.WriteString("Body: (empty)\n")
	}
}

func renderCommands(out *bytes.Buffer, response CommandsResponse) error {
	if response.Truncated {
		out.WriteString("Notice: Command results are truncated.\n")
	}
	fmt.Fprintf(out, "Commands (%d):\n", len(response.Commands))
	if len(response.Commands) == 0 {
		out.WriteString("  None\n")
		return nil
	}
	for i, command := range response.Commands {
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(out, "[Command %d]\n", i+1)
		writeLine(out, "Tool", command.Tool)
		writeLine(out, "Session ID", command.SessionID)
		writeLine(out, "Message ID", command.MessageID)
		writeLine(out, "Timestamp (UTC)", command.Timestamp)
		arguments, err := json.MarshalIndent(command.Arguments, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal command arguments: %w", err)
		}
		out.WriteString("Arguments:\n")
		out.WriteString(safeBody(string(arguments)))
		out.WriteByte('\n')
		if command.Output != "" {
			writeBody(out, "Output", command.Output)
		} else {
			out.WriteString("Output: (empty)\n")
		}
	}
	return nil
}

func renderSessions(out *bytes.Buffer, response SessionsResponse) error {
	fmt.Fprintf(out, "Sessions (%d):\n", len(response.Sessions))
	if len(response.Sessions) == 0 {
		out.WriteString("  None\n")
		return nil
	}
	for i, session := range response.Sessions {
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(out, "[Session %d]\n", i+1)
		writeLine(out, "Session ID", session.SessionID)
		writeLine(out, "CWD", session.CWD)
		writeLine(out, "Visibility", session.Visibility)
		if session.ParentSessionID != "" {
			writeLine(out, "Parent session ID", session.ParentSessionID)
		}
		writeLine(out, "Last message (UTC)", session.LastTimestamp)
		fmt.Fprintf(out, "Messages: %d\n", session.MessageCount)
		writeBody(out, "Last preview", session.LastPreview)
	}
	return nil
}

func renderActivities(out *bytes.Buffer, response ActivitiesResponse) {
	fmt.Fprintf(out, "Activities (%d):\n", len(response.Activities))
	if len(response.Activities) == 0 {
		out.WriteString("  None\n")
		return
	}
	for i, activity := range response.Activities {
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(out, "[Activity %d]\n", i+1)
		renderActivity(out, activity)
	}
}

func renderActivity(out *bytes.Buffer, activity Activity) {
	writeLine(out, "Activity ID", activity.ActivityID)
	writeLine(out, "Status", activity.Status)
	writeLine(out, "Title", activity.Title)
	writeLine(out, "Model", activity.Model)
	writeLine(out, "Effort", activity.Effort)
	fmt.Fprintf(out, "Tool uses: %d\n", activity.ToolUses)
	writeLine(out, "Started (UTC)", activity.StartedAt)
	writeLine(out, "Completed (UTC)", activity.CompletedAt)
	writeOptionalLine(out, "Parent session ID", activity.ParentSessionID)
	writeOptionalLine(out, "Child session ID", activity.ChildSessionID)
	writeOptionalLine(out, "Parent activity ID", activity.ParentActivityID)
	writeOptionalLine(out, "Kind", activity.Kind)
	writeOptionalLine(out, "Namespace", activity.Namespace)
	writeOptionalLine(out, "Start entry ID", activity.StartEntryID)
	writeOptionalLine(out, "Start parent ID", activity.StartParentID)
	writeOptionalLine(out, "Linked entry ID", activity.LinkedEntryID)
	writeOptionalLine(out, "Terminal entry ID", activity.TerminalEntryID)
	writeOptionalLine(out, "Terminal parent ID", activity.TerminalParentID)
	writeOptionalBody(out, "Description", activity.Description)
	writeBody(out, "Result summary", activity.ResultSummary)
}

func writeSearchNotices(out *bytes.Buffer, relaxed, truncated bool, budget Budget) {
	if relaxed {
		out.WriteString("Notice: Search used relaxed matching; results may match any query term rather than all terms.\n")
	}
	if truncated {
		out.WriteString("Notice: Results are truncated.\n")
	}
	if budget.Dropped == 0 && !budget.Shrunk {
		return
	}
	out.WriteString("Budget:")
	if budget.Limit > 0 {
		fmt.Fprintf(out, " limit %d characters;", budget.Limit)
	} else {
		out.WriteString(" no character limit;")
	}
	fmt.Fprintf(out, " spent %d characters", budget.Spent)
	if budget.Dropped > 0 {
		fmt.Fprintf(out, "; dropped %d result(s)", budget.Dropped)
	}
	if budget.Shrunk {
		out.WriteString("; bodies were shortened to fit")
	}
	out.WriteByte('\n')
}

func writeLine(out *bytes.Buffer, label, value string) {
	fmt.Fprintf(out, "%s: %s\n", label, safeLine(value))
}

func writeOptionalLine(out *bytes.Buffer, label, value string) {
	if value != "" {
		writeLine(out, label, value)
	}
}

func writeBody(out *bytes.Buffer, label, value string) {
	fmt.Fprintf(out, "%s: %s\n", label, safeBody(value))
}

func writeOptionalBody(out *bytes.Buffer, label, value string) {
	if value != "" {
		writeBody(out, label, value)
	}
}

// safeLine makes metadata single-line and renders control characters visibly.
func safeLine(value string) string {
	return sanitize(value, false)
}

// safeBody preserves ordinary transcript line breaks and tabs while rendering
// terminal controls visibly so payloads cannot execute terminal sequences.
func safeBody(value string) string {
	return sanitize(value, true)
}

func sanitize(value string, preserveLayout bool) string {
	var clean strings.Builder
	for _, r := range value {
		if !unicode.IsControl(r) {
			clean.WriteRune(r)
			continue
		}
		if preserveLayout && (r == '\n' || r == '\t') {
			clean.WriteRune(r)
			continue
		}
		switch r {
		case '\n':
			clean.WriteString(`\n`)
		case '\r':
			clean.WriteString(`\r`)
		case '\t':
			clean.WriteString(`\t`)
		default:
			if r <= 0xFFFF {
				fmt.Fprintf(&clean, `\u%04X`, r)
			} else {
				fmt.Fprintf(&clean, `\U%08X`, r)
			}
		}
	}
	return clean.String()
}

func writeJSONText(w io.Writer, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal text fallback: %w", err)
	}
	return writeAll(w, []byte(safeBody(string(encoded))+"\n"))
}

func writeAll(w io.Writer, data []byte) error {
	for len(data) > 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
