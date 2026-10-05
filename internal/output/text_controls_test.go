package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestTextEscapesC1ControlsInArgumentsAndDiagnostics(t *testing.T) {
	for _, value := range []any{
		CommandsResponse{Commands: []Command{{Arguments: map[string]any{"command": "\u009b2Jmalicious"}}}},
		InfoResponse{CurrentSession: "\u009b2Jmalicious"},
	} {
		var out bytes.Buffer
		if err := WriteText(&out, value); err != nil {
			t.Fatal(err)
		}
		if strings.ContainsRune(out.String(), '\u009b') {
			t.Fatalf("raw terminal control escaped sanitization: %q", out.String())
		}
	}
}
