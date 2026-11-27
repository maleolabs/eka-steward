package loop

import "time"

// Recovery: bounded retry + cancellation, and how a failed worker maps to no
// partial EKA write. See docs/recovery.md.

// RetryPolicy bounds how many times the loop re-dispatches ONE candidate.
//
// MaxAttempts is the TOTAL number of dispatch attempts, not the number of
// retries: MaxAttempts == 1 means "try once, never retry" — the pre-retry
// behaviour, and the semantic of the zero value. MaxAttempts < 1 is clamped to
// 1. After MaxAttempts attempts all fail, the candidate is left un-Done
// (deferred, re-selectable by the next Run) and the last error is returned.
//
// Backoff is the deterministic delay between attempts: no jitter, no
// randomness, so the retry schedule is reproducible. It is applied through
// Config.Sleep (injectable), so tests retry without wall-clock waits. A
// non-positive Backoff retries immediately.
type RetryPolicy struct {
	MaxAttempts int
	Backoff     time.Duration
}
