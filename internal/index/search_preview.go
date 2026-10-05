package index

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/amuldowney/cc-search/internal/transcript"
)

const (
	maxSearchPreviewRunes = 512
	searchSnippetTokens   = 64
	searchHitStart        = "\x01cc-search-hit-start\x02"
	searchHitEnd          = "\x01cc-search-hit-end\x02"
)

// sessionSearchOrder keeps the normal bm25/time ordering intact, optionally
// putting non-noise hits first and using rowid only as a deterministic tie-break.
func sessionSearchOrder(reduceNoise bool, noise, score, timestamp, rowid string) string {
	parts := make([]string, 0, 4)
	if reduceNoise {
		parts = append(parts, noise)
	}
	parts = append(parts, score, timestamp+" DESC")
	if rowid != "" {
		parts = append(parts, rowid+" DESC")
	}
	return strings.Join(parts, ", ")
}

// searchNoiseCase conservatively identifies copied skill frontmatter and
// echoed cc-search response objects in the existing small cached prefixes.
func searchNoiseCase(contentPrefix, prosePrefix string) string {
	prefix := "lower(COALESCE(" + contentPrefix + ", '') || ' ' || COALESCE(" + prosePrefix + ", ''))"
	return `CASE
		WHEN ` + prefix + ` LIKE '---%' AND ` + prefix + ` LIKE '%name:%' AND ` + prefix + ` LIKE '%description:%' THEN 1
		WHEN ` + prefix + ` LIKE '%"results"%' AND ` + prefix + ` LIKE '%"budget"%' AND ` + prefix + ` LIKE '%"relaxed"%' THEN 1
        WHEN ` + prefix + ` LIKE '{"results":%' AND ` + prefix + ` LIKE '%"sessionid":%' AND ` + prefix + ` LIKE '%"id":%' THEN 1
        WHEN ` + prefix + ` LIKE '{"commands":%' AND ` + prefix + ` LIKE '%"messageid":%' AND ` + prefix + ` LIKE '%"tool":%' THEN 1
		ELSE 0 END`
}

// fillSearchPreviews asks FTS5 for snippets only for the already-ranked and
// limited hits. No message bodies outside that selected set are loaded.
func (d *DB) fillSearchPreviews(match string, proseOnly bool, previewLength int, messages []transcript.Message) error {
	maxRunes := maxSearchPreviewRunes
	if previewLength > 0 && previewLength < maxRunes {
		maxRunes = previewLength
	}
	column := -1 // let FTS5 choose the matching column
	if proseOnly {
		column = 1
	}
	// Keep the selected-row hydration query below SQLite's conservative bind
	// parameter limit even when the caller asks for an unbounded result set.
	const batchSize = 500
	for start := 0; start < len(messages); start += batchSize {
		end := start + batchSize
		if end > len(messages) {
			end = len(messages)
		}
		if err := d.fillSearchPreviewBatch(match, column, maxRunes, messages[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) fillSearchPreviewBatch(match string, column, maxRunes int, messages []transcript.Message) error {
	query := fmt.Sprintf(`SELECT m.id, snippet(messages_fts, %d, ?, ?, ' … ', %d)
		FROM messages_fts JOIN messages m ON m.rowid = messages_fts.rowid
		WHERE messages_fts MATCH ? AND m.id IN (`, column, searchSnippetTokens)
	args := []any{searchHitStart, searchHitEnd, match}
	for i, message := range messages {
		if i != 0 {
			query += ","
		}
		query += "?"
		args = append(args, message.ID)
	}
	query += ")"

	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return fmt.Errorf("build matching search previews: %w", err)
	}
	defer rows.Close()
	previews := make(map[string]string, len(messages))
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return fmt.Errorf("read matching search preview: %w", err)
		}
		preview, ok := compactMarkedSnippet(raw, maxRunes)
		if !ok {
			return fmt.Errorf("FTS returned a search hit without a marked snippet: %s", id)
		}
		previews[id] = preview
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read matching search previews: %w", err)
	}
	for i := range messages {
		preview, ok := previews[messages[i].ID]
		if !ok {
			return fmt.Errorf("FTS omitted selected search preview: %s", messages[i].ID)
		}
		messages[i].SearchPreview = preview
	}
	return nil
}

// compactMarkedSnippet preserves the selected passage verbatim after whitespace
// normalization. Retain immediate pre-match context (including negations) and
// punctuation; never turn an identifier like foo.bar into "foo .bar".
func compactMarkedSnippet(raw string, maxRunes int) (string, bool) {
	flat, matchStart, matchEnd, ok := flattenMarkedSnippet(raw)
	if !ok || maxRunes <= 0 {
		return "", false
	}
	runes := []rune(flat)
	if matchEnd > len(runes) || matchStart < 0 || matchEnd <= matchStart {
		return "", false
	}
	if len(runes) <= maxRunes {
		return flat, true
	}
	if maxRunes == 1 {
		return string(runes[matchStart]), true
	}
	context := min(maxRunes/4, 16)
	start := max(0, matchStart-context)
	// Start at a word boundary without advancing beyond the first match.
	for start > 0 && start < matchStart && !unicode.IsSpace(runes[start-1]) {
		start++
	}
	prefix := ""
	if start > 0 && maxRunes > 3 {
		prefix = "… "
	}
	room := maxRunes - len([]rune(prefix))
	if matchEnd-start > room && matchEnd-matchStart <= room {
		start = matchEnd - room
		if start > 0 && maxRunes > 3 {
			prefix = "… "
			room = maxRunes - 2
		}
	}
	end := min(len(runes), start+room)
	out := append([]rune(prefix), runes[start:end]...)
	if end < len(runes) {
		markTruncated(&out, maxRunes)
	}
	return strings.TrimSpace(string(out)), true
}

func markTruncated(out *[]rune, maxRunes int) {
	if maxRunes <= 0 {
		return
	}
	if len(*out) < maxRunes {
		*out = append(*out, '…')
		return
	}
	if len(*out) > maxRunes {
		*out = (*out)[:maxRunes]
	}
	(*out)[maxRunes-1] = '…'
}

func flattenMarkedSnippet(raw string) (flat string, matchStart, matchEnd int, ok bool) {
	var out strings.Builder
	runeCount := 0
	pendingSpace := false
	inFirstMatch := false
	foundMatch := false
	for len(raw) > 0 {
		if strings.HasPrefix(raw, searchHitStart) {
			raw = raw[len(searchHitStart):]
			if !foundMatch {
				if pendingSpace && out.Len() > 0 {
					out.WriteByte(' ')
					runeCount++
					pendingSpace = false
				}
				matchStart = runeCount
				inFirstMatch = true
			}
			continue
		}
		if strings.HasPrefix(raw, searchHitEnd) {
			raw = raw[len(searchHitEnd):]
			if inFirstMatch {
				matchEnd = runeCount
				inFirstMatch = false
				foundMatch = true
			}
			continue
		}
		r, size := utf8.DecodeRuneInString(raw)
		raw = raw[size:]
		if unicode.IsSpace(r) {
			if out.Len() > 0 {
				pendingSpace = true
			}
			continue
		}
		if pendingSpace {
			out.WriteByte(' ')
			runeCount++
			pendingSpace = false
		}
		out.WriteRune(r)
		runeCount++
	}
	return out.String(), matchStart, matchEnd, foundMatch
}
