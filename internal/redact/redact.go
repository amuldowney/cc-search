// Package redact detects and removes likely credentials from recent transcript
// records. It deliberately reports counts and paths, never matched values.
package redact

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	defaultScannerBuffer = 64 * 1024
	maxJSONLineSize      = 64 * 1024 * 1024
	redacted             = "<REDACTED>"
)

var (
	secretKey = regexp.MustCompile(`(?i)(?:token|secret|password|passwd|api[_-]?key|access[_-]?key|credential|private[_-]?key|signing[_-]?key|client[_-]?secret|auth[_-]?token)`)
	envLine   = regexp.MustCompile(`^\s*(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*?)\s*$`)

	patterns = []pattern{
		{
			name:        "private_key",
			re:          regexp.MustCompile(`(?s)-----BEGIN [^-\r\n]*PRIVATE KEY-----.*?-----END [^-\r\n]*PRIVATE KEY-----`),
			replacement: "<REDACTED_PRIVATE_KEY>",
		},
		{
			name:        "bearer",
			re:          regexp.MustCompile(`(?i)(\b(?:bearer\s+|authorization\s*:\s*bearer\s+))[A-Za-z0-9._~+/=-]{12,}`),
			replacement: `$1` + redacted,
		},
		{
			name:        "basic_auth_url",
			re:          regexp.MustCompile(`(\b(?:https?|postgres(?:ql)?|mysql|redis|rediss)://[^\s/:@]+:)[^\s@]+@`),
			replacement: `$1` + redacted + `@`,
		},
		{
			name:        "known_token",
			re:          regexp.MustCompile(`(?i)(?:sk-ant-[A-Za-z0-9_-]{16,}|sk-[A-Za-z0-9_-]{20,}|ghp_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_-]{20,}|xox[baprs]-[A-Za-z0-9-]{20,}|AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{30,})`),
			replacement: redacted,
		},
		{
			name:        "jwt",
			re:          regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
			replacement: redacted,
		},
		{
			name:        "secret_header",
			re:          regexp.MustCompile(`(?i)(\b(?:cf-access-client-secret|x-api-key|api-key|authorization)\s*:\s*)[^\s,;]+`),
			replacement: `$1` + redacted,
		},
		{
			name:        "secret_assignment",
			re:          regexp.MustCompile(`(?i)(\b(?:token|secret|password|passwd|api[_-]?key|access[_-]?token|client[_-]?secret)\s*[=:]\s*["']?)[A-Za-z0-9._~+/=-]{8,}`),
			replacement: `$1` + redacted,
		},
		{
			name:        "secret_query",
			re:          regexp.MustCompile(`(?i)(\b(?:access_token|api_key|token|password|secret)=)[^&\s"']{8,}`),
			replacement: `$1` + redacted,
		},
	}
)

type pattern struct {
	name        string
	re          *regexp.Regexp
	replacement string
}

// Options controls the time window and optional exact values to redact.
type Options struct {
	Since        time.Time
	SecretValues []string
}

// Finding describes a transcript containing one or more likely secrets. It
// intentionally contains no matched text.
type Finding struct {
	Root    string
	Path    string
	Session string
	Matches int
}

// Report is the result of scanning or redacting transcript roots.
type Report struct {
	FilesScanned   int
	RecordsScanned int
	Matches        int
	Findings       []Finding
}

// SecretValuesFromEnvironment returns values from environment variables whose
// names identify credentials. Values are held in memory only and are never
// included in a Report.
func SecretValuesFromEnvironment() []string {
	values := make([]string, 0)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok && secretKey.MatchString(key) {
			if value = cleanValue(value); len(value) >= 8 && !placeholder(value) {
				values = append(values, value)
			}
		}
	}
	return normalizeValues(values)
}

// SecretValuesFromFiles reads dotenv-like files and collects values from
// credential-named keys. It is useful for an explicit local secret manifest;
// file contents are never returned in diagnostics.
func SecretValuesFromFiles(paths []string) ([]string, error) {
	values := make([]string, 0)
	for _, name := range paths {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read secret file %q: %w", name, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			match := envLine.FindStringSubmatch(line)
			if len(match) == 3 && secretKey.MatchString(match[1]) {
				if value := cleanValue(match[2]); len(value) >= 8 && !placeholder(value) {
					values = append(values, value)
				}
			}
		}
	}
	return normalizeValues(values), nil
}

func cleanValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') ||
		(value[0] == '"' && value[len(value)-1] == '"')) {
		value = value[1 : len(value)-1]
	}
	return value
}

func placeholder(value string) bool {
	switch strings.ToLower(value) {
	case "password", "secret", "changeme", "replace-me", "example", "smoke-secret", "test-secret", "redacted", redacted:
		return true
	default:
		return false
	}
}

func normalizeValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool { return len(result[i]) > len(result[j]) })
	return result
}

// Scan walks transcript roots and finds likely secrets in records at or after
// Since. Missing roots are ignored, matching normal cc-search behavior.
func Scan(roots []string, opts Options) (Report, error) {
	report := Report{Findings: []Finding{}}
	values := normalizeValues(opts.SecretValues)
	for _, root := range roots {
		if _, err := os.Stat(root); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return report, err
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !isTranscript(path) {
				return nil
			}
			result, err := inspectFile(path, root, opts, values, false)
			if err != nil {
				return err
			}
			report.FilesScanned += result.FilesScanned
			report.RecordsScanned += result.RecordsScanned
			report.Matches += result.Matches
			report.Findings = append(report.Findings, result.Findings...)
			return nil
		})
		if err != nil {
			return report, err
		}
	}
	return report, nil
}

// CountMatches returns the number of detector matches in one indexed text
// value. It is intended for verifying that the derived SQLite content is
// clean; it never returns the matched text.
func CountMatches(value string, secretValues []string) int {
	counts := &matchCounts{}
	scrubString(value, normalizeValues(secretValues), counts)
	return counts.total()
}

// RedactFile atomically rewrites one transcript. Only records at or after
// Since are changed. With apply=false it performs the same parsing and counts
// matches without modifying the source.
func RedactFile(path string, root string, opts Options, apply bool) (Report, error) {
	values := normalizeValues(opts.SecretValues)
	return inspectFile(path, root, opts, values, apply)
}

func inspectFile(path, root string, opts Options, values []string, apply bool) (Report, error) {
	report := Report{Findings: []Finding{}}
	if !isTranscript(path) {
		return report, nil
	}
	input, err := os.Open(path)
	if err != nil {
		return report, err
	}
	defer input.Close()

	reader, closeReader, err := transcriptReader(path, input)
	if err != nil {
		return report, err
	}
	defer closeReader()
	report.FilesScanned = 1

	var temp *os.File
	var writer io.Writer
	var buffered *bufio.Writer
	var encoder *zstd.Encoder
	var changed bool
	if apply {
		mode, modeErr := inputMode(path)
		if modeErr != nil {
			return report, modeErr
		}
		temp, err = os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".redact-*")
		if err != nil {
			return report, err
		}
		tempName := temp.Name()
		defer func() {
			_ = temp.Close()
			_ = os.Remove(tempName)
		}()
		if err := temp.Chmod(mode); err != nil {
			return report, err
		}
		writer = temp
		if strings.HasSuffix(path, ".jsonl.zst") {
			encoder, err = zstd.NewWriter(temp)
			if err != nil {
				return report, err
			}
			writer = encoder
		}
		buffered = bufio.NewWriterSize(writer, defaultScannerBuffer)
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, defaultScannerBuffer), maxJSONLineSize)
	for scanner.Scan() {
		raw := scanner.Bytes()
		report.RecordsScanned++
		var record any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&record); err != nil {
			// Preserve malformed transcript lines; cc-search itself ignores them.
			if apply {
				if _, err := buffered.Write(append(append([]byte(nil), raw...), '\n')); err != nil {
					return report, err
				}
			}
			continue
		}
		timestamp := recordTimestamp(record)
		if !timestamp.IsZero() && !timestamp.Before(opts.Since) {
			counts := &matchCounts{}
			scrubbed := scrubValue(record, values, counts)
			if counts.total() > 0 {
				changed = true
				report.Matches += counts.total()
			}
			if apply {
				if counts.total() > 0 {
					encoded, err := json.Marshal(scrubbed)
					if err != nil {
						return report, err
					}
					if _, err := buffered.Write(append(encoded, '\n')); err != nil {
						return report, err
					}
				} else if _, err := buffered.Write(append(append([]byte(nil), raw...), '\n')); err != nil {
					return report, err
				}
			}
		} else if apply {
			if _, err := buffered.Write(append(append([]byte(nil), raw...), '\n')); err != nil {
				return report, err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return report, err
	}
	if report.Matches > 0 {
		report.Findings = append(report.Findings, Finding{
			Root: root, Path: path, Session: sessionName(path), Matches: report.Matches,
		})
	}
	if !apply || !changed {
		return report, nil
	}
	if err := buffered.Flush(); err != nil {
		return report, err
	}
	if encoder != nil {
		if err := encoder.Close(); err != nil {
			return report, err
		}
	}
	if err := temp.Sync(); err != nil {
		return report, err
	}
	if err := temp.Close(); err != nil {
		return report, err
	}
	if err := os.Rename(temp.Name(), path); err != nil {
		return report, err
	}
	return report, nil
}

func inputMode(path string) (os.FileMode, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Mode().Perm(), nil
}

func transcriptReader(path string, input *os.File) (io.Reader, func(), error) {
	if strings.HasSuffix(path, ".jsonl.zst") {
		decoder, err := zstd.NewReader(input)
		if err != nil {
			return nil, func() {}, err
		}
		return decoder, decoder.Close, nil
	}
	return input, func() {}, nil
}

func isTranscript(path string) bool {
	return strings.HasSuffix(path, ".jsonl") || strings.HasSuffix(path, ".jsonl.zst")
}

func sessionName(path string) string {
	name := filepath.Base(path)
	name = strings.TrimSuffix(name, ".zst")
	return strings.TrimSuffix(name, ".jsonl")
}

func recordTimestamp(value any) time.Time {
	object, ok := value.(map[string]any)
	if !ok {
		return time.Time{}
	}
	stamp, _ := object["timestamp"].(string)
	if stamp == "" {
		if message, ok := object["message"].(map[string]any); ok {
			stamp, _ = message["timestamp"].(string)
		}
	}
	if stamp == "" {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, stamp)
	if err != nil {
		return time.Time{}
	}
	return parsed
}

type matchCounts struct {
	categories map[string]int
}

func (m *matchCounts) add(name string, count int) {
	if count == 0 {
		return
	}
	if m.categories == nil {
		m.categories = make(map[string]int)
	}
	m.categories[name] += count
}

func (m *matchCounts) total() int {
	total := 0
	for _, count := range m.categories {
		total += count
	}
	return total
}

func scrubValue(value any, values []string, counts *matchCounts) any {
	switch typed := value.(type) {
	case string:
		return scrubString(typed, values, counts)
	case []any:
		result := make([]any, len(typed))
		for i, child := range typed {
			result[i] = scrubValue(child, values, counts)
		}
		return result
	case map[string]any:
		opaque := false
		if kind, ok := typed["type"].(string); ok {
			switch strings.ToLower(kind) {
			case "image", "audio", "file", "binary":
				opaque = true
			}
		}
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if skipKey(key) || (opaque && (key == "data" || key == "content")) {
				result[key] = child
				continue
			}
			if text, ok := child.(string); ok && secretKey.MatchString(key) && len(text) >= 8 {
				counts.add("secret_key_field", 1)
				result[key] = redacted
				continue
			}
			result[key] = scrubValue(child, values, counts)
		}
		return result
	default:
		return value
	}
}

func skipKey(key string) bool {
	switch key {
	case "thinkingSignature", "encrypted_content", "signature":
		return true
	default:
		return false
	}
}

func scrubString(value string, values []string, counts *matchCounts) string {
	for _, secret := range values {
		if strings.Contains(value, secret) {
			occurrences := strings.Count(value, secret)
			value = strings.ReplaceAll(value, secret, redacted)
			counts.add("exact", occurrences)
		}
	}
	for _, pattern := range patterns {
		var matches int
		value, matches = replacePattern(pattern, value)
		counts.add(pattern.name, matches)
	}
	return value
}

func replacePattern(pattern pattern, value string) (string, int) {
	locations := pattern.re.FindAllStringIndex(value, -1)
	if len(locations) == 0 {
		return value, 0
	}
	if pattern.name != "secret_header" && pattern.name != "secret_query" {
		return pattern.re.ReplaceAllString(value, pattern.replacement), len(locations)
	}
	matches := 0
	result := pattern.re.ReplaceAllStringFunc(value, func(match string) string {
		if strings.HasSuffix(match, redacted) || strings.HasSuffix(match, "<REDACTED_PRIVATE_KEY>") {
			return match
		}
		matches++
		return pattern.re.ReplaceAllString(match, pattern.replacement)
	})
	return result, matches
}
