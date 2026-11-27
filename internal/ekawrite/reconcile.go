package ekawrite

import (
	"fmt"

	"github.com/maleolabs/eka-core/v2/api"
)

// This file adds the checkpoint/resume reconcile entry point to the single
// write path. Reconcile re-issues an in-flight write idempotently, so a loop
// that crashed after persisting its intent but before confirming the write can
// complete it without a double apply:
//
//   - the unit-creation class (Publish) goes through the deterministic
//     read-then-write replay gate (Publish), so a same-payload re-issue is a
//     no-op (no max+1 instance);
//   - the state-transition class reads the line's current execution state and
//     is a no-op when the line is already at or after the target state (the
//     core transition engine is a forward/backward state machine, not
//     idempotent on replay).
//
// Reconcile is a mutation site: like every other write it lives in this
// package, the only place a non-test file may call the api.Authoring surface
// (cmd/boundary_arch_test.go).

// ReconcileStatus classifies the outcome of one reconcile.
type ReconcileStatus string

const (
	// ReconcileNoOp: the intended mutation was already present — nothing was
	// written (the transition line was at/after the target, or the publish
	// payload already matched the line's current instance).
	ReconcileNoOp ReconcileStatus = "no-op"
	// ReconcileApplied: reconcile performed the mutation.
	ReconcileApplied ReconcileStatus = "applied"
)

// ReconcileRequest describes one in-flight intent to reconcile. Exactly one
// class is selected: Publish non-nil is the unit-creation class; otherwise the
// transition class (Target + To).
type ReconcileRequest struct {
	// Publish is the re-issuable publish payload (unit-creation class).
	Publish *PublishInput
	// Digest is the intended publish payload digest. When non-empty it is
	// verified against the re-issued payload; a mismatch is a deterministic
	// error (a corrupted intent never silently mis-writes).
	Digest string
	// Target is the transition line ("<ns>/<type>:<id>", transition class).
	Target string
	// To is the transition destination state (transition class).
	To string
	// RepoPath addresses a transition from a repository (transition class).
	RepoPath string
}

// ReconcileResult reports one reconcile.
type ReconcileResult struct {
	// Status is "applied" or "no-op".
	Status ReconcileStatus
	// Form is the canonical identity form of the affected instance (unit
	// class).
	Form string
	// InstanceVersion is the instance version of that form (unit class).
	InstanceVersion int
	// From, To are the transition states (transition class).
	From, To string
}

// Reconcile re-issues one in-flight intent idempotently. It never fails on the
// already-applied path; a genuine write refusal is surfaced unchanged.
func (w *Writer) Reconcile(req ReconcileRequest) (ReconcileResult, error) {
	if err := w.ready(); err != nil {
		return ReconcileResult{}, err
	}
	if req.Publish != nil {
		return w.reconcilePublish(req)
	}
	return w.reconcileTransition(req)
}

// reconcilePublish re-issues a publish through the replay gate.
func (w *Writer) reconcilePublish(req ReconcileRequest) (ReconcileResult, error) {
	// A recorded digest is an integrity check on the intent: recompute the
	// intended digest and refuse a mismatch deterministically.
	if req.Digest != "" {
		want, err := w.intendedDigest(*req.Publish)
		if err != nil {
			return ReconcileResult{}, err
		}
		if want != req.Digest {
			return ReconcileResult{}, fmt.Errorf("ekawrite: reconcile digest mismatch for %s/%s:%s (intent %s, payload %s)",
				req.Publish.Namespace, req.Publish.Type, req.Publish.ID, req.Digest, want)
		}
	}
	res, err := w.Publish(*req.Publish)
	if err != nil {
		return ReconcileResult{}, err
	}
	status := ReconcileApplied
	if res.Status == PublishNoOp {
		status = ReconcileNoOp
	}
	return ReconcileResult{
		Status:          status,
		Form:            res.Form,
		InstanceVersion: res.InstanceVersion,
	}, nil
}

// reconcileTransition re-issues a state transition unless the line is already
// at or after the target state.
func (w *Writer) reconcileTransition(req ReconcileRequest) (ReconcileResult, error) {
	if req.Target == "" || req.To == "" {
		return ReconcileResult{}, fmt.Errorf("ekawrite: reconcile transition needs a target and destination state")
	}
	cur, found, err := w.rt.Resolver().Resolve(req.Target)
	if err != nil {
		return ReconcileResult{}, fmt.Errorf("ekawrite: reconcile read %s: %w", req.Target, err)
	}
	if !found {
		return ReconcileResult{}, fmt.Errorf("ekawrite: reconcile %s: line not found", req.Target)
	}
	from := cur.StateVector.ExecutionState
	if executionStateAtOrAfter(from, req.To) {
		return ReconcileResult{Status: ReconcileNoOp, From: from, To: req.To}, nil
	}
	res, err := w.Transition(api.TransitionRequest{
		RepoPath:  req.RepoPath,
		Target:    req.Target,
		To:        req.To,
		Confirmed: true,
	})
	if err != nil {
		return ReconcileResult{}, err
	}
	return ReconcileResult{Status: ReconcileApplied, From: res.From, To: res.To}, nil
}

// executionRank is the forward rank of the D1 execution-state table. canceled
// is intentionally absent (it is a terminal branch, not a forward position), so
// a canceled line is never treated as "at/after" a forward target and reconcile
// re-issues it (a legal canceled -> todo re-activation).
var executionRank = map[string]int{
	"planned":     0,
	"todo":        1,
	"in-progress": 2,
	"in-review":   3,
	"done":        4,
}

// executionStateAtOrAfter reports whether current is at or after target in the
// forward execution-state order. It is conservative: an unknown state on either
// side is false, so reconcile re-issues and the engine's own transition table
// makes the final call.
func executionStateAtOrAfter(current, target string) bool {
	if current == target {
		return true
	}
	c, okC := executionRank[current]
	t, okT := executionRank[target]
	if !okC || !okT {
		return false
	}
	return c >= t
}
