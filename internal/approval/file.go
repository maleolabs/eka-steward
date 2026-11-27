package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// File/queue channel: the DEFAULT Approver. It needs NO TTY — a human (or
// another process) reads a structured request file and writes a structured
// response file; the steward polls with a bounded wait. See docs/approval.md.
//
//	<Dir>/approval-request.json    written by the steward (the question)
//	<Dir>/approval-response.json   written by the human/operator (the answer)
//
// A response answers a request ONLY when its `key` equals the request's
// deterministic Key; a stale or mismatched response is ignored (the wait keeps
// polling until it times out to Deny). This is a single-in-flight queue per
// Dir — the loop admits one approval at a time — and it never reads a terminal.
const (
	// DefaultRequestFile is the request file name inside Dir.
	DefaultRequestFile = "approval-request.json"
	// DefaultResponseFile is the response file name inside Dir.
	DefaultResponseFile = "approval-response.json"
	// DefaultPollInterval is how often the channel re-reads the response.
	DefaultPollInterval = 100 * time.Millisecond
)

// Response is the on-disk answer shape (written by a human, an operator tool or
// the steward's protocol surface). It carries the request Key so a stale answer
// is never applied.
type Response struct {
	Key     string  `json:"key"`
	Verdict Verdict `json:"verdict"`
	By      string  `json:"by,omitempty"`
	Reason  string  `json:"reason,omitempty"`
}

// WriteResponse writes one decision to the file/queue channel at dir,
// atomically, in the exact shape Ask's readResponse consumes. It is the
// operator/consumer side of the channel: an external process answers a pending
// request without a TTY. A response only answers the request whose Key it
// carries; a mismatch is ignored by Ask.
func WriteResponse(dir string, r Response) error {
	if dir == "" {
		return errors.New("approval: WriteResponse needs a Dir")
	}
	return writeJSON(filepath.Join(dir, DefaultResponseFile), r)
}

// FileApprover is the file/queue Approver. Dir is required and must exist;
// the loop passes its StateDir. It never reads os.Stdin, a terminal or an
// isatty probe.
type FileApprover struct {
	// Dir is the channel directory (the steward StateDir in production).
	Dir string
	// RequestFile overrides DefaultRequestFile.
	RequestFile string
	// ResponseFile overrides DefaultResponseFile.
	ResponseFile string
	// PollInterval overrides DefaultPollInterval.
	PollInterval time.Duration
}

// Ask writes the request file and polls for a matching response until ctx is
// done (the bounded wait lives in AskBounded). A malformed, stale or
// mismatched response is ignored, so a bad answer can only ever DENY by
// timing out — never approve by accident.
func (a FileApprover) Ask(ctx context.Context, req Request) (Decision, error) {
	if a.Dir == "" {
		return Decision{Verdict: Deny}, errors.New("approval: FileApprover needs a Dir")
	}
	reqPath := filepath.Join(a.Dir, a.name(a.RequestFile, DefaultRequestFile))
	respPath := filepath.Join(a.Dir, a.name(a.ResponseFile, DefaultResponseFile))

	// Publish the question atomically so a reader never sees a half-written
	// request (the human side may also write atomically; we do not require it).
	if err := writeJSON(reqPath, req); err != nil {
		return Decision{Verdict: Deny}, fmt.Errorf("approval: write request: %w", err)
	}

	// Check once immediately: a response that already exists (a resume) is
	// returned without waiting for the first tick.
	if d, ok := readResponse(respPath, req.Key); ok {
		return d, nil
	}

	interval := a.PollInterval
	if interval <= 0 {
		interval = DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Decision{Verdict: Deny, TimedOut: true}, ctx.Err()
		case <-ticker.C:
			if d, ok := readResponse(respPath, req.Key); ok {
				return d, nil
			}
		}
	}
}

// name returns override when set, else fallback.
func (a FileApprover) name(override, fallback string) string {
	if override != "" {
		return override
	}
	return fallback
}

// readResponse reads and validates the response file. ok is false when the
// file is absent, malformed, carries a different request key, or an unknown
// verdict — every one of which must NOT approve.
func readResponse(path, key string) (Decision, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Decision{}, false
	}
	var r Response
	if err := json.Unmarshal(b, &r); err != nil {
		return Decision{}, false
	}
	if r.Key != key {
		return Decision{}, false
	}
	switch r.Verdict {
	case Approve, Deny:
	default:
		return Decision{}, false
	}
	return Decision{Verdict: r.Verdict, By: r.By, Reason: r.Reason}, true
}

// writeJSON writes v as indented JSON to path atomically (temp file in the
// same directory, then rename), so a reader never sees a partial request.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".approval-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
