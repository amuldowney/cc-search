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

// compactMarkedSnippet strips the private FTS markers, moves the match to the
// front so further output-budget trimming cannot lose a late match, and clips
// on Unicode rune boundaries while retaining omission marks.
func compactMarkedSnippet(raw string, maxRunes int) (string, bool) {
	flat, matchStart, matchEnd, ok := flattenMarkedSnippet(raw)
	if !ok || maxRunes <= 0 {
		return "", false
	}
	runes := []rune(flat)
	if matchEnd > len(runes) || matchStart < 0 || matchEnd <= matchStart {
		return "", false
	}

	prefix := ""
	if maxRunes > 2 && (matchStart > 0 || strings.HasPrefix(flat, "…")) {
		prefix = "… "
	}
	out := []rune(prefix)
	match := runes[matchStart:matchEnd]
	if len(out)+len(match) > maxRunes {
		room := maxRunes - len(out)
		if room <= 0 {
			return string(out[:maxRunes]), true
		}
		out = append(out, match[:room]...)
		markTruncated(&out, maxRunes)
		return string(out), true
	}
	out = append(out, match...)

	tail := runes[matchEnd:]
	for len(tail) > 0 && unicode.IsSpace(tail[0]) {
		tail = tail[1:]
	}
	suffixOmitted := len(runes) > 0 && runes[len(runes)-1] == '…'
	if len(tail) > 0 {
		if len(out) < maxRunes {
			out = append(out, ' ')
		}
		room := maxRunes - len(out)
		if room == 0 {
			if len(out) > 0 && out[len(out)-1] == ' ' {
				out = out[:len(out)-1]
			}
			return string(out), true
		}
		if room < len(tail) {
			out = append(out, tail[:room]...)
			markTruncated(&out, maxRunes)
			return string(out), true
		}
		out = append(out, tail...)
	} else if suffixOmitted {
		if len(out)+2 <= maxRunes {
			out = append(out, ' ', '…')
		} else if len(out) < maxRunes {
			out = append(out, '…')
		}
	}
	return string(out), true
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
