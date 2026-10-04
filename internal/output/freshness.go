package output

// Freshness describes a successfully published transcript-root snapshot. Times
// are Unix milliseconds; Error is a sanitized background-refresh category.
type Freshness struct {
	LastSuccess int64  `json:"lastSuccess"`
	LastAttempt int64  `json:"lastAttempt"`
	Stale       bool   `json:"stale"`
	Error       string `json:"error,omitempty"`
}
