package loop

// Human-approval escalation stage. A permission request the pure policy cannot
// decide (guard.Escalate) is handed to an Approver; the human decision is
// recorded as a typed EKA artifact (a resolved review note, through
// internal/ekawrite) and drives the gate: approve proceeds, deny/timeout
// blocks and leaves the candidate un-Done. The in-progress approval is a
// checkpointed write-ahead intent, so a killed run resumes without recording
// the same decision twice (the ekawrite replay gate). See docs/approval.md.

import (
	"context"
	"fmt"

	"github.com/maleolabs/eka-steward/v2/internal/approval"
	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/guard"
)

// WriteKindApproval is the write class of a recorded human approval.
const WriteKindApproval = "approval"

// ApprovalPolicy configures the human-approval escalation stage. Its ZERO
// VALUE DISABLES the stage (Request is nil), so the loop is byte-identical to
// the pre-approval loop.
type ApprovalPolicy struct {
	// Approver is the human channel (approval.Approver). Required when a
	// request escalates.
	Approver approval.Approver
	// Policy is the pure permission policy that classifies a request. Its
	// zero value default-escalates the undecidable class.
	Policy guard.Policy
	// Request yields the permission request a candidate needs, if any. nil
	// disables the stage (the zero value). The decision it returns is pure:
	// the same candidate always yields the same request (and Key).
	Request func(decision.Candidate) (guard.PermissionRequest, bool)
}

// enabled reports whether the escalation stage is configured.
func (p ApprovalPolicy) enabled() bool { return p.Request != nil }

// ApprovalRecord is one human decision to persist as a typed artifact.
type ApprovalRecord struct {
	// Key is the deterministic request key.
	Key string
	// ToolKind and Path are the requested tool class and path.
	ToolKind string
	Path     string
	// Verdict is the raw decision: approve | deny | timeout.
	Verdict string
	// Reason is the human's or the timeout's explanation.
	Reason string
}

// ApprovalRecorder is implemented by a Writer that can persist an approval
// decision (the ekawrite-backed Writer does). The loop requires it only when a
// request actually escalates.
type ApprovalRecorder interface {
	Approve(ctx context.Context, c decision.Candidate, rec ApprovalRecord) error
}

// ApprovalRefusal is the deterministic, surfaced outcome of a denied or
// timed-out approval. The candidate is left un-Done (blocked, never advanced).
type ApprovalRefusal struct {
	CandidateID string
	RequestKey  string
	Verdict     string
	Reason      string
}

// Error renders the deterministic refusal.
func (e *ApprovalRefusal) Error() string {
	return fmt.Sprintf("loop: approval of %s (request %s) was %s; the item is left un-done",
		e.CandidateID, e.RequestKey, e.Verdict)
}

// approvalGate runs the escalation gate for one candidate whose request was
// classified Escalate. It returns nil to proceed and an *ApprovalRefusal to
// block. Allow and Deny (clearly-deniable) short-circuit without a human.
func (l *Loop) approvalGate(ctx context.Context, c decision.Candidate, step Step, req guard.PermissionRequest) error {
	switch l.cfg.Approval.Policy.Decide(req) {
	case guard.Allow:
		return nil
	case guard.Deny:
		return &ApprovalRefusal{CandidateID: c.ID, Verdict: string(approval.Deny), Reason: "denied by policy"}
	}

	// Escalate: needs a recorder and a channel.
	rec, ok := l.writer.(ApprovalRecorder)
	if l.writer == nil || !ok {
		return fmt.Errorf("loop: approval required for %s but the writer cannot record an approval", c.ID)
	}
	if l.cfg.Approval.Approver == nil {
		return fmt.Errorf("loop: approval required for %s but no approver is configured", c.ID)
	}

	areq := approval.RequestFor(req)
	in := Intent{
		CandidateID: c.ID,
		Kind:        IntentApproval,
		Target:      c.ID,
		Approval: &ApprovalIntent{
			Key:           areq.Key,
			ToolKind:      areq.ToolKind,
			Path:          areq.Path,
			WorkspaceRoot: areq.WorkspaceRoot,
			OptionID:      areq.OptionID,
		},
		Step: step,
	}
	if err := l.beginIntent(in); err != nil {
		return err
	}
	l.crash(BoundaryAfterIntent)

	dec, err := approval.AskBounded(ctx, l.cfg.Approval.Policy, l.cfg.Approval.Approver, areq)
	if err != nil || !dec.Approved() {
		verdict, reason := denial(dec, err)
		if rerr := l.recordApproval(ctx, rec, c, areq, verdict, reason); rerr != nil {
			return fmt.Errorf("loop: record approval of %s: %w", c.ID, rerr)
		}
		l.observeWrite(c.ID, WriteKindApproval, verdict)
		l.crash(BoundaryAfterApply)
		if cerr := l.clearIntent(c.ID); cerr != nil {
			return cerr
		}
		return &ApprovalRefusal{CandidateID: c.ID, RequestKey: areq.Key, Verdict: verdict, Reason: reason}
	}

	if rerr := l.recordApproval(ctx, rec, c, areq, "approve", dec.Reason); rerr != nil {
		return fmt.Errorf("loop: record approval of %s: %w", c.ID, rerr)
	}
	l.observeWrite(c.ID, WriteKindApproval, "approve")
	l.crash(BoundaryAfterApply)
	return l.clearIntent(c.ID)
}

// recordApproval persists one decision through the single write path under
// writeMu, so the EKA write path is never entered concurrently.
func (l *Loop) recordApproval(ctx context.Context, rec ApprovalRecorder, c decision.Candidate, req approval.Request, verdict, reason string) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	return rec.Approve(ctx, c, ApprovalRecord{
		Key:      req.Key,
		ToolKind: req.ToolKind,
		Path:     req.Path,
		Verdict:  verdict,
		Reason:   reason,
	})
}

// denial maps a non-approve decision (or a bounded-wait error) to the raw
// verdict recorded and the reason. A timeout is recorded as "timeout"; any
// other failure as "deny" (fail closed).
func denial(dec approval.Decision, err error) (verdict, reason string) {
	switch {
	case dec.TimedOut || (err != nil && approval.TimedOut(err)):
		verdict = "timeout"
		reason = "timed out waiting for approval"
	case err != nil:
		verdict = "deny"
		reason = err.Error()
	default:
		verdict = "deny"
		reason = dec.Reason
	}
	return verdict, reason
}

// reconcileApproval re-reads the durable decision and re-records the artifact
// through the replay gate. It never dispatches, and a re-issued identical
// decision is a no-op (no second note). The candidate stays un-Done here; the
// next gate run decides proceed/block from the same recorded decision.
func (l *Loop) reconcileApproval(ctx context.Context, in Intent) error {
	if in.Approval == nil {
		return fmt.Errorf("loop: approval intent for %s has no request payload", in.CandidateID)
	}
	rec, ok := l.writer.(ApprovalRecorder)
	if l.writer == nil || !ok {
		return fmt.Errorf("loop: in-flight approval for %s but the writer cannot record an approval", in.CandidateID)
	}
	if l.cfg.Approval.Approver == nil {
		return fmt.Errorf("loop: in-flight approval for %s but no approver is configured", in.CandidateID)
	}
	req := approval.Request{
		Key:           in.Approval.Key,
		ToolKind:      in.Approval.ToolKind,
		Path:          in.Approval.Path,
		WorkspaceRoot: in.Approval.WorkspaceRoot,
		OptionID:      in.Approval.OptionID,
	}
	dec, err := approval.AskBounded(ctx, l.cfg.Approval.Policy, l.cfg.Approval.Approver, req)
	verdict := "approve"
	reason := dec.Reason
	if err != nil || !dec.Approved() {
		verdict, reason = denial(dec, err)
	}
	c := decision.Candidate{ID: in.CandidateID, State: in.Step.State, Priority: in.Step.Priority, CreatedSeq: in.Step.CreatedSeq}
	if rerr := l.recordApproval(ctx, rec, c, req, verdict, reason); rerr != nil {
		return fmt.Errorf("loop: reconcile approval of %s: %w", in.CandidateID, rerr)
	}
	return nil
}
