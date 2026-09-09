package transcript

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// isCodexRecord distinguishes the rollout envelope from Claude/pi records.
// A payload is enough to opt into the branch for older records whose type was
// simply "message" or "response".
func isCodexRecord(rec record) bool {
	switch rec.Type {
	case "session_meta", "response_item", "event_msg", "turn_context", "compacted", "token_count",
		"user_message", "agent_message", "task_complete":
		return true
	default:
		return len(rec.Payload) > 0 || len(rec.Item) > 0
	}
}

func (p *Parser) parseCodexLine(rec record, line []byte) (Message, bool) {
	p.codex = true
	if p.codexCanonical == nil {
		p.codexCanonical = make(map[string]struct{})
	}
	if p.codexEvents == nil {
		p.codexEvents = make(map[string]struct{})
	}
	if rec.SessionID != "" && p.session.ID == "" {
		p.session.ID = rec.SessionID
	}
	if rec.CWD != "" && p.session.CWD == "" {
		p.session.CWD = rec.CWD
	}

	if rec.Type == "session_meta" {
		meta := rec.Payload
		if len(meta) == 0 {
			meta = rec.Item
		}
		p.parseCodexSessionMeta(meta)
		return Message{}, false
	}

	raw := rec.Payload
	if len(raw) == 0 {
		raw = rec.Item
	}
	if len(raw) == 0 && rec.Message != nil && len(rec.Message.Raw) > 0 {
		raw = rec.Message.Raw
	}

	switch rec.Type {
	case "response_item", "message", "response":
		return p.parseCodexResponseItem(rec, raw, line)
	case "event_msg", "user_message", "agent_message", "task_complete":
		return p.parseCodexEvent(rec, raw, line)
	default:
		// turn_context, compacted, token_count, and other rollout metadata may
		// contain instructions, environment data, or usage counters. Never
		// flatten those into conversational prose.
		return Message{}, false
	}
}

func (p *Parser) parseCodexSessionMeta(raw json.RawMessage) {
	obj := codexObject(codexUnwrapItem(raw))
	if nested := obj["session_meta"]; len(nested) > 0 {
		obj = codexObject(nested)
	}
	if id := codexString(obj, "id", "session_id", "sessionId"); id != "" {
		p.session.ID = id
	}
	if cwd := codexString(obj, "cwd", "working_directory", "project_root"); cwd != "" {
		p.session.CWD = cwd
	}
	if parent := codexString(obj, "parent_session", "parentSession"); parent != "" {
		p.session.ParentSession = parent
	}
}

func (p *Parser) parseCodexResponseItem(rec record, raw, line []byte) (Message, bool) {
	item := codexUnwrapItem(raw)
	obj := codexObject(item)
	if len(obj) == 0 {
		if text := codexTextValue(item); text != "" {
			return p.makeCodexMessageWithParts(rec, item, "assistant", text, text, nil, nil, line)
		}
		return Message{}, false
	}

	itemType := strings.ToLower(codexString(obj, "type"))
	role := strings.ToLower(codexString(obj, "role"))
	if role == "user" || role == "assistant" {
		if itemType == "" || itemType == "message" || len(obj["content"]) > 0 || len(obj["text"]) > 0 {
			return p.makeCodexMessage(rec, item, obj, role, line)
		}
	}
	// Response items for tools may carry an assistant role in older rollouts,
	// but are still structured tool records rather than spoken prose.
	if codexIsToolType(itemType) || codexHasToolFields(obj) {
		return p.makeCodexToolMessage(rec, item, obj, line)
	}
	if role != "" {
		// Keep the normalized output vocabulary small. Developer/system items
		// are metadata rather than user/assistant conversation messages.
		return Message{}, false
	}

	// Be tolerant of older textual response_item variants, without treating
	// arbitrary rollout metadata as a message.
	if text := codexUnknownText(obj); text != "" {
		role = firstNonEmpty(role, "assistant")
		prose := text
		if itemType == "reasoning" || itemType == "summary" || itemType == "summary_text" {
			prose = ""
		}
		return p.makeCodexMessageWithParts(rec, item, role, text, prose, nil, nil, line)
	}
	return Message{}, false
}

func (p *Parser) makeCodexMessage(rec record, item json.RawMessage, obj map[string]json.RawMessage, role string, line []byte) (Message, bool) {
	parts := codexContentParts(obj["content"])
	if len(parts.content) == 0 {
		// A few old rollouts put the text directly on the message item.
		if text := codexString(obj, "text", "message"); text != "" {
			parts.content = text
			parts.prose = text
		}
	}
	if parts.prose != "" {
		proseKey := strings.TrimSpace(parts.prose)
		if role == "user" {
			proseKey = codexUserText(proseKey)
		}
		if _, mirroredEvent := p.codexEvents[role+"\x00"+proseKey]; mirroredEvent {
			// An event mirror arrived before its canonical response item. Keep
			// the useful event-only message rather than emitting both.
			return Message{}, false
		}
	}
	return p.makeCodexMessageWithParts(rec, item, role, parts.content, parts.prose, parts.calls, parts.results, line)
}

func (p *Parser) makeCodexMessageWithParts(rec record, item json.RawMessage, role, content, prose string, calls []ToolCall, results []ToolResult, line []byte) (Message, bool) {
	content = strings.TrimSpace(content)
	prose = strings.TrimSpace(prose)
	if role == "user" {
		prose = codexUserText(prose)
	}
	if content == "" {
		return Message{}, false
	}
	if prose != "" {
		p.codexCanonical[role+"\x00"+prose] = struct{}{}
	}
	return p.codexMessage(rec, item, role, content, prose, calls, results, line), true
}

func (p *Parser) makeCodexToolMessage(rec record, item json.RawMessage, obj map[string]json.RawMessage, line []byte) (Message, bool) {
	parts := codexContentParts(item)
	if len(parts.calls) == 0 && len(parts.results) == 0 {
		calls, results, rendered := codexToolsFromObject(obj)
		parts.calls, parts.results = calls, results
		parts.content = rendered
	} else if parts.content == "" {
		parts.content = codexRenderTools(parts.calls, parts.results)
	}
	if strings.TrimSpace(parts.content) == "" {
		return Message{}, false
	}
	return p.makeCodexMessageWithParts(rec, item, "assistant", parts.content, "", parts.calls, parts.results, line)
}

func (p *Parser) parseCodexEvent(rec record, raw, line []byte) (Message, bool) {
	raw = codexUnwrapItem(raw)
	obj := codexObject(raw)
	if nested := obj["event"]; len(nested) > 0 {
		raw = nested
		obj = codexObject(nested)
	}
	eventType := strings.ToLower(codexString(obj, "type", "event_type"))
	if eventType == "" {
		eventType = strings.TrimSuffix(strings.ToLower(rec.Type), "_message")
	}
	var role string
	switch eventType {
	case "user_message", "user":
		role = "user"
	case "agent_message", "assistant", "assistant_message", "task_complete":
		role = "assistant"
	default:
		return Message{}, false
	}
	text := codexEventText(obj)
	if text == "" && rec.Message != nil {
		text = codexTextValue(rec.Message.Raw)
	}
	text = strings.TrimSpace(text)
	if role == "user" {
		text = codexUserText(text)
	}
	if text == "" {
		return Message{}, false
	}
	key := role + "\x00" + text
	if _, duplicate := p.codexCanonical[key]; duplicate {
		return Message{}, false
	}
	if _, duplicate := p.codexEvents[key]; duplicate {
		return Message{}, false
	}
	p.codexEvents[key] = struct{}{}
	return p.makeCodexMessageWithParts(rec, raw, role, text, text, nil, nil, line)
}

func (p *Parser) codexMessage(rec record, item json.RawMessage, role, content, prose string, calls []ToolCall, results []ToolResult, line []byte) Message {
	id := codexString(codexObject(item), "id", "message_id")
	if id == "" {
		id = rec.ID
	}
	if id == "" {
		id = codexFallbackID(line)
	}
	ts := codexTimestamp(rec.Timestamp, item)
	if ts == 0 {
		return Message{}
	}
	sessionID := firstNonEmpty(rec.SessionID, p.session.ID)
	return Message{
		ID: id, EntryID: id, SessionID: sessionID, Timestamp: ts,
		Type: role, Content: content, Prose: prose, CharCount: len(content),
		ToolCalls: calls, ToolResults: results,
	}
}

func codexTimestamp(top string, raw json.RawMessage) int64 {
	stamp := top
	if stamp == "" {
		stamp = codexString(codexObject(raw), "timestamp", "created_at", "createdAt")
	}
	if stamp == "" {
		return 0
	}
	if ts, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
		return ts.UnixMilli()
	}
	return 0
}

func codexFallbackID(line []byte) string {
	sum := sha256.Sum256(line)
	return "codex-" + hex.EncodeToString(sum[:])
}

func codexUnwrapItem(raw json.RawMessage) json.RawMessage {
	obj := codexObject(raw)
	for _, key := range []string{"item", "payload"} {
		if nested := obj[key]; len(nested) > 0 && len(codexObject(nested)) > 0 {
			return codexUnwrapItem(nested)
		}
	}
	return raw
}

func codexObject(raw json.RawMessage) map[string]json.RawMessage {
	var obj map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &obj) != nil {
		return nil
	}
	return obj
}

func codexString(obj map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if raw := obj[key]; len(raw) > 0 {
			var value string
			if json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != "" {
				return value
			}
		}
	}
	return ""
}

func codexTextValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	if obj := codexObject(raw); len(obj) > 0 {
		for _, key := range []string{"text", "message", "content", "output", "result", "body", "stdout", "stderr", "error", "summary", "summary_text", "reasoning"} {
			if value := codexTextValue(obj[key]); value != "" {
				return value
			}
		}
	}
	var array []json.RawMessage
	if json.Unmarshal(raw, &array) == nil {
		var parts []string
		for _, value := range array {
			if text := codexTextValue(value); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func codexEventText(obj map[string]json.RawMessage) string {
	for _, key := range []string{"message", "last_agent_message", "text", "content", "output"} {
		if text := codexTextValue(obj[key]); text != "" {
			return text
		}
	}
	return ""
}

func codexUserText(text string) string {
	const marker = "## My request for Codex:"
	if idx := strings.Index(text, marker); idx >= 0 {
		return strings.TrimSpace(text[idx+len(marker):])
	}
	return strings.TrimSpace(text)
}

func codexUnknownText(obj map[string]json.RawMessage) string {
	for _, key := range []string{"text", "message", "content", "output", "result", "body", "stdout", "stderr", "error", "summary", "summary_text", "reasoning"} {
		if text := codexTextValue(obj[key]); text != "" {
			return text
		}
	}
	return ""
}

type codexParts struct {
	content string
	prose   string
	calls   []ToolCall
	results []ToolResult
}

func (p codexParts) append(other codexParts, prose bool) codexParts {
	if other.content != "" {
		p.content = joinText(p.content, other.content)
	}
	if prose && other.prose != "" {
		p.prose = joinText(p.prose, other.prose)
	}
	p.calls = append(p.calls, other.calls...)
	p.results = append(p.results, other.results...)
	return p
}

func joinText(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "\n" + b
}

func codexContentParts(raw json.RawMessage) codexParts {
	if len(raw) == 0 {
		return codexParts{}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return codexParts{content: strings.TrimSpace(text), prose: strings.TrimSpace(text)}
	}
	var blocks []json.RawMessage
	if json.Unmarshal(raw, &blocks) == nil {
		var out codexParts
		for _, block := range blocks {
			out = out.append(codexContentParts(block), true)
		}
		return out
	}
	obj := codexObject(raw)
	if len(obj) == 0 {
		return codexParts{}
	}
	typ := strings.ToLower(codexString(obj, "type"))
	if codexIsToolType(typ) || codexIsToolResultType(typ) || codexHasToolFields(obj) {
		calls, results, rendered := codexToolsFromObject(obj)
		return codexParts{content: rendered, calls: calls, results: results}
	}
	if typ == "reasoning" || typ == "summary" || typ == "summary_text" {
		text := codexUnknownText(obj)
		return codexParts{content: text}
	}
	if nested := obj["content"]; len(nested) > 0 {
		return codexContentParts(nested)
	}
	if text := codexString(obj, "text", "value"); text != "" {
		return codexParts{content: text, prose: text}
	}
	if text := codexUnknownText(obj); text != "" {
		return codexParts{content: text, prose: text}
	}
	return codexParts{}
}

func codexIsToolType(typ string) bool {
	switch typ {
	case "function_call", "custom_tool_call", "local_shell_call", "shell_call", "shell_command", "exec_command", "mcp_tool_call", "mcp_call", "mcp_tool_use", "tool_search_call", "web_search_call", "tool_call", "tool_use", "computer_call", "command_execution":
		return true
	default:
		return false
	}
}

func codexIsToolResultType(typ string) bool {
	switch typ {
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output", "shell_call_output", "shell_command_output", "exec_command_output", "mcp_tool_call_output", "mcp_call_output", "tool_search_output", "tool_result", "tool_output", "command_execution_output":
		return true
	default:
		return false
	}
}

func codexHasToolFields(obj map[string]json.RawMessage) bool {
	typ := strings.ToLower(codexString(obj, "type"))
	return codexIsToolType(typ) || codexIsToolResultType(typ)
}

func codexToolsFromObject(obj map[string]json.RawMessage) ([]ToolCall, []ToolResult, string) {
	typ := strings.ToLower(codexString(obj, "type"))
	if codexIsToolResultType(typ) {
		result := codexToolResult(obj)
		return nil, []ToolResult{result}, result.Content
	}
	if !codexIsToolType(typ) {
		return nil, nil, ""
	}
	call := codexToolCall(obj)
	return []ToolCall{call}, nil, codexRenderTools([]ToolCall{call}, nil)
}

func codexToolCall(obj map[string]json.RawMessage) ToolCall {
	id := codexString(obj, "call_id", "callId", "tool_call_id", "toolCallId", "tool_use_id", "toolUseId", "id")
	name := codexString(obj, "name", "tool_name", "toolName", "function_name", "functionName", "tool")
	typ := strings.ToLower(codexString(obj, "type"))
	if name == "" {
		switch typ {
		case "local_shell_call", "shell_call", "shell_command", "exec_command", "command_execution":
			name = "local_shell"
		case "mcp_tool_call", "mcp_call", "mcp_tool_use":
			name = "mcp_tool"
		default:
			name = typ
		}
	}
	args := codexRawString(obj, "arguments", "args", "input", "parameters", "action", "command")
	if args == "" {
		if function := codexObject(obj["function"]); len(function) > 0 {
			args = codexRawString(function, "arguments", "args", "input")
			if name == typ || name == "" {
				name = codexString(function, "name")
			}
		}
	}
	return ToolCall{ID: id, Name: name, Arguments: args}
}

func codexToolResult(obj map[string]json.RawMessage) ToolResult {
	id := codexString(obj, "call_id", "callId", "tool_call_id", "toolCallId", "tool_use_id", "toolUseId", "id")
	name := codexString(obj, "name", "tool_name", "toolName", "function_name", "functionName", "tool")
	content := codexOutputString(obj, "output", "result", "content", "text", "error")
	isError := false
	if raw := obj["is_error"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &isError)
	}
	if raw := obj["isError"]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &isError)
	}
	if len(obj["error"]) > 0 || strings.EqualFold(codexString(obj, "status"), "failed") ||
		strings.EqualFold(codexString(obj, "status"), "declined") || strings.EqualFold(codexString(obj, "status"), "incomplete") {
		isError = true
	}
	if raw := obj["success"]; len(raw) > 0 {
		var success bool
		if json.Unmarshal(raw, &success) == nil && !success {
			isError = true
		}
	}
	return ToolResult{ID: id, Name: name, Content: content, IsError: isError}
}

func codexOutputString(obj map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if raw := obj[key]; len(raw) > 0 {
			var text string
			if json.Unmarshal(raw, &text) == nil {
				return text
			}
			if text := codexTextValue(raw); text != "" {
				return text
			}
			return string(raw)
		}
	}
	return ""
}

func codexRawString(obj map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		if raw := obj[key]; len(raw) > 0 {
			var text string
			if json.Unmarshal(raw, &text) == nil {
				return text
			}
			if len(raw) > 0 {
				return string(raw)
			}
		}
	}
	return ""
}

func codexRenderTools(calls []ToolCall, results []ToolResult) string {
	var parts []string
	for _, call := range calls {
		name := call.Name
		if name == "" {
			name = "tool"
		}
		part := "[tool: " + name + "]"
		if call.Arguments != "" {
			part += " " + call.Arguments
		}
		parts = append(parts, part)
	}
	for _, result := range results {
		if result.Content != "" {
			parts = append(parts, result.Content)
		}
	}
	return strings.Join(parts, "\n")
}
