# Remove recap support and duplicate output Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove recap-specific behavior and ensure each cc-search response emits either a compact `preview` or full `content`, never both.

**Architecture:** Keep recap text as ordinary transcript content while deleting recap metadata from the parser, SQLite schema, query options, CLI flags, and JSON response. Make output body fields mutually exclusive with `omitempty`: compact mode sets `preview`, and `--full` sets `content` without transforming the source text.

**Tech Stack:** Go, standard `flag` and `encoding/json`, SQLite/FTS5 through `go-sqlite3`, package tests with `go test`.

## Global Constraints

- Recap messages remain searchable and are not filtered out.
- Remove `--prefer-recaps`, `--recaps-only`, `isRecap`, and `recapCount` rather than retaining hidden compatibility paths.
- Bump the derived SQLite schema version so old indexes rebuild automatically.
- Compact previews remain one-line and length-limited.
- Full content remains exact, including newlines.
- A response must never contain both `preview` and `content` in one result.
- Character budgets continue to measure the body actually emitted.
- Production changes follow red-green-refactor: each behavior test must fail before its implementation is written.

---

### Task 1: Make compact and full result bodies mutually exclusive

**Files:**
- Modify: `internal/output/output.go: Result and Format`
- Modify: `internal/output/output_test.go: output JSON tests`

**Interfaces:**
- Consumes: existing `output.Options{Full, PreviewLength, Budget, UseProse}`.
- Produces: `output.Result` with `Preview` populated only when `Full == false`, and `Content` populated only when `Full == true`.

- [ ] **Step 1: Write the failing test**

Add this test to `internal/output/output_test.go`:

```go
func TestFormatEmitsOnlyOneBodyField(t *testing.T) {
	msgs := []transcript.Message{{Content: "line one\nline two"}}

	compact, err := json.Marshal(Format(msgs, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(compact), `"content"`) {
		t.Errorf("compact output contains content: %s", compact)
	}
	if !strings.Contains(string(compact), `"preview":"line one line two"`) {
		t.Errorf("compact output lacks preview: %s", compact)
	}

	full, err := json.Marshal(Format(msgs, Options{Full: true}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(full), `"preview"`) {
		t.Errorf("full output contains preview: %s", full)
	}
	if !strings.Contains(string(full), `"content":"line one\\nline two"`) {
		t.Errorf("full output lacks exact content: %s", full)
	}
}
```

- [ ] **Step 2: Run the focused test and verify it fails**

Run:

```bash
go test ./internal/output -run TestFormatEmitsOnlyOneBodyField -count=1
```

Expected: FAIL because current full mode still populates and marshals `preview`.

- [ ] **Step 3: Implement the minimal output change**

Change the result body fields to omit empty values:

```go
Preview string `json:"preview,omitempty"`
Content string `json:"content,omitempty"`
```

In `Format`, retain the existing body selection, budget planning, and rendering. Populate the result body as follows:

```go
if opts.Full {
	r.Content = body
} else {
	r.Preview = body
}
```

Do not render a second preview in full mode.

- [ ] **Step 4: Run the focused and package tests**

Run:

```bash
go test ./internal/output -run 'TestFormatEmitsOnlyOneBodyField|TestFormat' -count=1
```

Expected: PASS, with all existing output behavior and budget tests still green.

- [ ] **Step 5: Commit the output contract**

```bash
git add internal/output/output.go internal/output/output_test.go
git commit -m "fix: avoid duplicate preview and content output"
```

---

### Task 2: Remove recap metadata atomically from parser, index, CLI, and responses

**Files:**
- Modify: `internal/transcript/parse.go: Message and Parser.ParseLine`
- Modify: `internal/transcript/parse_test.go: recap-specific tests`
- Modify: `internal/index/index.go: schema, SearchOptions, indexing, selection, search, collection`
- Modify: `internal/index/index_test.go: schema and recap-query tests`
- Modify: `internal/cli/cli.go: usage and runSearch`
- Modify: `internal/cli/cli_test.go: recap and JSON response tests`
- Modify: `internal/output/output.go: Result, Response, Format`
- Modify: `internal/output/output_test.go: recap assertions and empty JSON`

**Interfaces:**
- Consumes: ordinary transcript text, including text containing the word `recap`.
- Produces: ordinary `transcript.Message` values, an index without recap storage/query state, a CLI without recap flags, and JSON without `isRecap` or `recapCount`.

- [ ] **Step 1: Write failing tests for the new public and schema contracts**

Add this schema test to `internal/index/index_test.go`; it uses the existing `newIndex` helper:

```go
func TestSchemaDoesNotStoreRecapMetadata(t *testing.T) {
	db, _ := newIndex(t, []msg{{"assistant", "recap: ordinary text", 10}})

	var version int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 4 {
		t.Fatalf("schema version = %d, want 4", version)
	}

	var columns int
	if err := db.sql.QueryRow(`
		SELECT count(*) FROM pragma_table_info('messages') WHERE name = 'isRecap'`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatalf("messages has %d isRecap columns, want none", columns)
	}
}
```

Replace the old recap-only CLI test in `internal/cli/cli_test.go` with this ordinary-search test and add the removed-flag test:

```go
func TestSearchTreatsRecapLikeTextNormally(t *testing.T) {
	cfg := fixture(t, []msg{
		{"assistant", "recap: the proxy moved", 30},
		{"assistant", "the proxy moved", 20},
	})

	resp, stderr, code := run(t, cfg, "search", "proxy")
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if resp.Total != 2 {
		t.Fatalf("Total = %d, want 2", resp.Total)
	}
}

func TestSearchRejectsRemovedRecapFlags(t *testing.T) {
	cfg := fixture(t, []msg{{"assistant", "proxy note", 10}})
	for _, removed := range []string{"--prefer-recaps", "--recaps-only"} {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"search", "proxy", removed}, cfg, &stdout, &stderr)
		if code != 2 {
			t.Errorf("flag %s exit code = %d, want 2", removed, code)
		}
	}
}
```

Add a response-field assertion to `internal/output/output_test.go`:

```go
func TestFormatOmitsRecapFields(t *testing.T) {
	encoded, err := json.Marshal(Format([]transcript.Message{{Content: "recap text"}}, Options{}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"isRecap"`) || strings.Contains(string(encoded), `"recapCount"`) {
		t.Errorf("response contains removed recap fields: %s", encoded)
	}
}
```

Update the expected empty JSON string in the existing output and CLI tests to remove `recapCount`. These new/updated tests must fail before the production removal: the current schema is version 3, current flags are accepted, and current JSON contains recap fields.

- [ ] **Step 2: Run the focused tests and verify they fail for the intended reasons**

Run:

```bash
go test ./internal/index ./internal/cli ./internal/output -run 'TestSchemaDoesNotStoreRecapMetadata|TestSearchTreatsRecapLikeTextNormally|TestSearchRejectsRemovedRecapFlags|TestFormatOmitsRecapFields' -count=1
```

Expected: the schema assertion, removed-flag assertion, and removed-field assertion fail; the ordinary-search test confirms the fixture is searchable but does not replace the failing contract tests.

- [ ] **Step 3: Remove recap state from transcript parsing**

In `internal/transcript/parse.go`, delete `IsRecap bool` from `Message`. Delete the `isRecap` calculation in `Parser.ParseLine` and stop assigning it in the returned message. Keep prose extraction, tool-result handling, and ordinary content unchanged.

In `internal/transcript/parse_test.go`, remove the three tests whose only purpose is asserting recap detection or non-detection. Retain the underlying prose/tool-block coverage, and add this replacement test:

```go
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
```

- [ ] **Step 4: Remove recap storage and bump the derived schema**

In `internal/index/index.go`:

- Change `schemaVersion` from `3` to `4`.
- Delete `isRecap INTEGER NOT NULL DEFAULT 0` from `messages`.
- Change inserts and conflict updates to use only `id, sessionId, timestamp, type, content, prose, charCount`.
- Remove `isRecap` from `selectColumns`.
- Remove `IsRecap` from row scans.

The existing `Open` schema-mismatch path will discard and rebuild old version-3 indexes.

- [ ] **Step 5: Remove recap query options and SQL**

Delete `PreferRecaps` and `RecapsOnly` from `SearchOptions`. Delete the recap predicate and priority ordering from `DB.Search`. Keep relevance and timestamp ordering unchanged:

```go
query += ` ORDER BY bm25(f.messages_fts), m.timestamp DESC`
```

Delete the old index tests `TestSearchRecapsOnlyExcludesNonRecaps` and `TestSearchPreferRecapsBoostsRecapsToTop`; the ordinary-search test from Step 1 replaces their relevant coverage.

- [ ] **Step 6: Remove recap fields from output and CLI**

In `internal/output/output.go`, delete `IsRecap` from `Result`, delete `RecapCount` from `Response`, and remove recap assignment/counting from `Format`. Keep `Total`, `Truncated`, `Relaxed`, and budget accounting unchanged.

In `internal/cli/cli.go`, delete the recap flag lines from the usage text. In `runSearch`, remove the two flag declarations, the local variables, and the `SearchOptions` assignments. The standard `flag.FlagSet` must reject both names as usage errors.

Update all output/CLI assertions that inspect `IsRecap` or `RecapCount` so they assert the remaining fields instead. The empty response JSON must now begin:

```json
{"results":[],"total":0,"truncated":false,"relaxed":false,
```

- [ ] **Step 7: Run all affected package tests**

Run:

```bash
gofmt -w internal/transcript internal/index internal/output internal/cli
go test ./internal/transcript ./internal/index ./internal/output ./internal/cli -count=1
```

Expected: PASS, including schema version 4, ordinary recap-like text, removed flags returning exit code 2, and responses with no recap fields.

- [ ] **Step 8: Commit the atomic recap removal**

```bash
git add internal/transcript internal/index internal/output internal/cli
git commit -m "refactor: remove recap support"
```

---

### Task 3: Update current documentation and perform full verification

**Files:**
- Modify: `README.md: usage examples, response example, output and indexing descriptions`
- Verify: `docs/superpowers/specs/2026-08-14-remove-recaps-and-duplicate-output-design.md`

**Interfaces:**
- Consumes: the final CLI and JSON contracts from Tasks 1–2.
- Produces: documentation that shows no removed flags or response fields.

- [ ] **Step 1: Update README examples and contract text**

Remove `--prefer-recaps` and `--recaps-only` examples. Change the JSON example to omit `isRecap` and `recapCount`. Replace the output description with:

```text
Compact output contains a single-line `preview` field. `--full` replaces it
with the complete `content` field; the two fields are never emitted together.
Full content preserves newlines. `--preview-length N` only affects compact
output.
```

Remove the Recaps subsection. Keep the prose-vs-content and budget sections, updating any sentence that says `--full` adds content so it instead says `--full` selects content.

- [ ] **Step 2: Check current source and README for stale public references**

Run:

```bash
rg -n -- '--prefer-recaps|--recaps-only|isRecap|recapCount' README.md cmd internal
```

Expected: no matches. The historical 2026-07-22 design document may retain its original requirements because it is a historical design record, not current behavior documentation.

- [ ] **Step 3: Run formatting, tests, and build**

Run:

```bash
gofmt -w cmd/cc-search/main.go internal/cli internal/index internal/output internal/transcript
go test ./... -count=1
make build
```

Expected: all tests pass and `make build` produces `./cc-search` successfully.

- [ ] **Step 4: Inspect the final diff and verify the contract manually**

Run:

```bash
git diff --check HEAD~3..HEAD
git status --short
git diff --stat HEAD~3..HEAD
```

Confirm that compact JSON has `preview` but no `content`, full JSON has `content` but no `preview`, and no changed source path references recap metadata.

- [ ] **Step 5: Commit documentation and verification changes**

```bash
git add README.md
git commit -m "docs: update cc-search output contract"
```
