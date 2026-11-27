package loop

// This file defines the checkpoint's write-ahead intent vocabulary. An Intent
// is what the loop persists BEFORE it attempts a mutation and clears once the
// mutation is confirmed applied. It is the unit the resume pass reconciles, so
// a crash at any boundary of a step is recoverable (docs/checkpoint.md).
//
// The two write classes mirror the ekawrite contract:
//
//   - IntentTransition: a state transition (the loop's only forward mutation
//     today). Reconcile reads the line's current execution state and re-issues
//     the transition only when the line is not yet at/after the target state,
//     because the core transition engine is a forward/backward state machine —
//     not idempotent on replay.
//   - IntentPublish: a unit creation. Reconcile re-issues through ekawrite's
//     deterministic read-then-write replay gate, so an identical payload is a
//     no-op (no max+1 instance) and a changed payload is one explicit write.

// IntentKind classifies the EKA mutation one step intends to apply.
type IntentKind string

const (
	// IntentTransition is a state transition on an existing line.
	IntentTransition IntentKind = "transition"
	// IntentPublish is the creation of a knowledge unit.
	IntentPublish IntentKind = "publish"
	// IntentApproval is an escalated permission request awaiting (or already
	// carrying) a human decision. Its reconcile re-reads the durable decision
	// and re-records the typed artifact through ekawrite's replay gate, so a
	// restart never records the same approval twice (docs/approval.md).
	IntentApproval IntentKind = "approval"
)

// Intent is one write-ahead record: the mutation a step intends to apply,
// persisted BEFORE the mutation is attempted and cleared once it is confirmed
// applied. It is self-contained so resume can reconcile it without the
// original dispatch.
type Intent struct {
	// CandidateID is the dispatched candidate identity (the qualified line
	// form). It keys the intent so a per-candidate entry never duplicates.
	CandidateID string `json:"candidateId"`
	// Kind is the write class the intent belongs to.
	Kind IntentKind `json:"kind"`
	// Target is the affected line ("<ns>/<type>:<id>").
	Target string `json:"target"`
	// To is the transition destination state (IntentTransition only).
	To string `json:"to,omitempty"`
	// Digest is the intended publish payload digest (IntentPublish only). It
	// is verified against the re-issued payload, so a corrupted intent is a
	// deterministic error rather than a silent mis-write.
	Digest string `json:"digest,omitempty"`
	// Publish carries the payload needed to re-issue a publish (IntentPublish
	// only).
	Publish *PublishIntent `json:"publish,omitempty"`
	// Approval carries the escalated permission request (IntentApproval only).
	// It is self-contained so resume can re-read the durable decision without
	// re-deriving it.
	Approval *ApprovalIntent `json:"approval,omitempty"`
	// Step is the exact deterministic step this intent belongs to; resume
	// replays it verbatim so the step sequence is byte-identical after a
	// restart.
	Step Step `json:"step"`
}

// ApprovalIntent is the re-issuable payload of an escalated permission
// request. Key is the deterministic request key (approval.RequestFor), so a
// re-issued request is the SAME approval.
type ApprovalIntent struct {
	Key           string `json:"key"`
	ToolKind      string `json:"toolKind"`
	Path          string `json:"path,omitempty"`
	WorkspaceRoot string `json:"workspaceRoot,omitempty"`
	OptionID      string `json:"optionId"`
}

// PublishIntent is the re-issuable payload of a unit-creation intent. It
// mirrors the fields of an ekawrite.PublishInput; the loop keeps its own
// vocabulary so the Writer seam stays adapter-shaped.
type PublishIntent struct {
	Project       string         `json:"project"`
	Namespace     string         `json:"namespace"`
	Type          string         `json:"type"`
	ID            string         `json:"id"`
	Content       map[string]any `json:"content,omitempty"`
	Dimension     string         `json:"dimension,omitempty"`
	Domain        string         `json:"domain,omitempty"`
	Phase         string         `json:"phase,omitempty"`
	Relationships []Relationship `json:"relationships,omitempty"`
}

// Relationship is the loop's minimal authoring edge (type + target).
type Relationship struct {
	Type   string `json:"type"`
	Target string `json:"target"`
}

// ReconcileResult is the outcome of reconciling one in-flight intent. Applied
// is false when the intended mutation was already present (the no-op path) and
// true when reconcile performed it.
type ReconcileResult struct {
	Applied bool
	Status  string
}

// Crash boundaries: the loop calls a configured CrashPoint at each checked
// boundary of a step so a test can deterministically simulate a kill (panic or
// process exit) without a real process. The names are the crash matrix rows of
// docs/checkpoint.md.
const (
	// BoundaryAfterIntent: the intent is persisted, the worker has not run.
	BoundaryAfterIntent = "after-intent"
	// BoundaryAfterDispatch: the worker ran, the mutation has not been applied.
	BoundaryAfterDispatch = "after-dispatch"
	// BoundaryAfterApply: the mutation applied, the intent is not yet cleared.
	BoundaryAfterApply = "after-apply"
)

// CrashPoint is the injectable crash-injection seam. Production leaves it nil
// (no crash injection); a test installs a hook that panics at a chosen boundary
// to simulate a kill. The hook must not return normally if it wants to emulate
// a crash — the loop assumes it either panics/exits or returns to continue.
type CrashPoint func(boundary string)
