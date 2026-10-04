package cli

import "testing"

func TestSearchDefaultLimitAndExplicitUnlimited(t *testing.T) {
	messages := make([]msg, 25)
	for i := range messages {
		messages[i] = msg{"user", "searchlimitneedle", i}
	}
	cfg := fixture(t, messages)
	for _, tc := range []struct {
		name      string
		flags     []string
		want      int
		truncated bool
	}{
		{"default", nil, 20, true},
		{"unlimited", []string{"--limit", "0"}, 25, false},
		{"explicit", []string{"--limit", "3"}, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"search", "searchlimitneedle"}, tc.flags...)
			resp, stderr, code := run(t, cfg, args...)
			if code != 0 || len(resp.Results) != tc.want || resp.Truncated != tc.truncated {
				t.Fatalf("code=%d count=%d truncated=%v stderr=%s; want count=%d truncated=%v", code, len(resp.Results), resp.Truncated, stderr, tc.want, tc.truncated)
			}
		})
	}
}
