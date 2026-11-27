// Package discuss implements the steward's discussion phase: a bounded,
// resumable, deterministic turn loop that drives a worker to produce
// decisions for one work item, records those decisions as EKA knowledge
// through the single write path, and derives the next (execute) phase's
// input by READING the recorded knowledge back — never from in-memory
// state.
//
// It keeps the steward's core split (docs/determinism.md): CODE owns the
// orchestration — how many rounds, which role, when to stop — and the
// CONTENT (the decision text) is the worker's. A round's bounded context is
// assembled with internal/render.Render and handed to the worker through the
// existing Dispatcher seam (extended with the context payload, exactly as
// internal/loop extended it for review).
//
// # Recorded knowledge
//
// A decision is recorded as a dec- (decision record) EKA artifact via
// internal/ekawrite.NewDraft/Publish, reached through the loop.Writer seam.
// dec- is the artifact that IS a decision record (Context / Decision /
// Consequences / Alternatives Considered, ADR domain Architecture), and its
// creation goes through ekawrite's deterministic read-then-write replay gate
// — so a resume re-issue of the same payload is a no-op (no max+1 instance).
// A cmt- note would not have that gate and is a comment, not a decision.
//
// # Resumability
//
// The checkpoint is internal/loop's write-ahead intent log. Because the
// recorded content is worker-produced, the intent is persisted AFTER the
// dispatch and BEFORE the publish: a kill after the dispatch (no intent yet)
// re-dispatches the round (worker side effects are not checkpointed, per
// docs/checkpoint.md) and still records exactly one decision; a kill after
// the intent reconciles the persisted payload through the replay gate without
// re-dispatching; a kill after the publish is a replay no-op. See
// docs/discuss.md.
//
// # Next phase
//
// BuildExecuteInput takes only the read seam and the subject line. It reads
// the recorded dec- units back through api.Knowledge (the read-only contract)
// and renders the execute-phase payload, so the next phase's input is
// reconstructed from the store, not carried across in memory.
package discuss

import (
	"context"
	"fmt"
	"strings"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/loop"
	"github.com/maleolabs/eka-steward/v2/internal/render"
	"github.com/maleolabs/eka-steward/v2/internal/role"
)

// DecisionType is the EKA artifact type a captured decision is recorded as.
const DecisionType = "dec"

// RoleAuthor is the role name used when Config.Role is empty.
const RoleAuthor = "author"

// MaxRounds bounds the discussion length. Config.Rounds is clamped to it, so
// a discussion can never run unbounded rounds (the deterministic stop is the
// code-owned round count, not a worker signal).
const MaxRounds = 64

// DefaultMaxBytes is the render budget used when Config.MaxBytes is <= 0.
const DefaultMaxBytes = render.DefaultMaxBytes

// KindRound is the Step.Kind a discussion round intent carries.
const KindRound = "discuss"

// Subject is the work item under discussion, as read from EKA.
type Subject struct {
	// Line is the qualified line form "<ns>/<type>:<id>".
	Line string
	// Namespace, Type and ID are the parsed identity segments.
	Namespace string
	Type      string
	ID        string
	// Title is a short display title (the subject's description).
	Title string
	// Brief is the subject's full description/acceptance context.
	Brief string
}

// Decision is one captured discussion decision — the recorded content of one
// round. It mirrors the dec- artifact's sections.
type Decision struct {
	// ID is the dec- unit id ("<subject-id>-dec-r<NN>").
	ID string `json:"id"`
	// Round is the 1-based round number.
	Round int `json:"round"`
	// Context is the deterministic discussion context of the round.
	Context string `json:"context"`
	// Decision is the worker-produced decision text.
	Decision string `json:"decision"`
	// Consequences is the recorded consequence (deterministic, empty by
	// default).
	Consequences string `json:"consequences"`
	// Alternatives are the recorded considered alternatives.
	Alternatives []string `json:"alternatives,omitempty"`
}

// Knowledge is the read-only EKA view the discussion needs: the subject's
// context and the decisions already recorded for it. A StoreKnowledge
// implementation reads through api.Knowledge/api.Resolver; it never mutates.
type Knowledge interface {
	// Subject returns the work item under discussion.
	Subject(ctx context.Context, line string) (Subject, error)
	// Decisions returns the decisions recorded for subjectLine, in
	// deterministic round order, read back from EKA.
	Decisions(ctx context.Context, subjectLine string) ([]Decision, error)
}

// Recorder is the single write path's idempotent apply seam. A discussion
// records a decision by re-issuing a publish intent through it; the
// production implementation is loop.EkawriteWriter (→ internal/ekawrite),
// whose publish path goes through the deterministic replay gate.
type Recorder interface {
	Reconcile(ctx context.Context, in loop.Intent) (loop.ReconcileResult, error)
}

// ContextDispatcher is the worker seam extended with the rendered context
// payload, mirroring loop.ReviewDispatcher. Dispatch must remain the ONLY
// place a worker/LLM runs; the payload is data, never a control input.
type ContextDispatcher interface {
	loop.Dispatcher
	DispatchContext(ctx context.Context, c decision.Candidate, payload []byte) (loop.DispatchResult, error)
}

// Round is one completed (or resumed) discussion round, reported to
// Config.OnRound exactly once per round.
type Round struct {
	// Index is the 1-based round number.
	Index int `json:"index"`
	// DecisionID is the recorded dec- unit id.
	DecisionID string `json:"decisionId"`
}

// Config is the operator-owned discussion configuration.
type Config struct {
	// Project is the EKA project scope (required).
	Project string
	// Subject is the qualified line form of the work item (required).
	Subject string
	// Rounds is the code-owned round count; < 1 clamps to 1, > MaxRounds
	// clamps to MaxRounds.
	Rounds int
	// MaxBytes is the render budget; <= 0 uses DefaultMaxBytes.
	MaxBytes int
	// Role is the worker role name; empty uses RoleAuthor.
	Role string
	// Roles, when set, validates/resolves Role deterministically (an
	// unknown role is an error, never a fallback).
	Roles *role.Set
	// Crash is the injectable crash seam (loop.CrashPoint). Production
	// leaves it nil.
	Crash loop.CrashPoint
	// OnRound observes each completed round exactly once, in plan order.
	OnRound func(Round)
}

// Discussion runs the resumable discussion of one work item. Construct it
// with New. It is sequential: Run drives one round at a time.
type Discussion struct {
	knowledge Knowledge
	worker    ContextDispatcher
	recorder  Recorder
	state     *loop.Store
	cfg       Config
}

// New wires a Discussion. state must be non-nil: the discussion records
// completed rounds and in-flight intents there so a restart resumes
// deterministically.
func New(knowledge Knowledge, worker ContextDispatcher, recorder Recorder, state *loop.Store, cfg Config) *Discussion {
	return &Discussion{
		knowledge: knowledge,
		worker:    worker,
		recorder:  recorder,
		state:     state,
		cfg:       cfg,
	}
}

// Rounds returns the resolved (clamped) round count.
func (d *Discussion) Rounds() int { return clampRounds(d.cfg.Rounds) }

// clampRounds clamps a configured round count into [1, MaxRounds].
func clampRounds(n int) int {
	if n < 1 {
		return 1
	}
	if n > MaxRounds {
		return MaxRounds
	}
	return n
}

// Run reconciles any in-flight intent (resume), then drives one round at a
// time until every configured round is recorded.
func (d *Discussion) Run(ctx context.Context) error {
	if err := d.Resume(ctx); err != nil {
		return err
	}
	for {
		_, ok, err := d.Step(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
}

// Step records the next pending round. It returns false when every configured
// round is already recorded.
func (d *Discussion) Step(ctx context.Context) (Round, bool, error) {
	idx, ok, err := d.nextRound(ctx)
	if err != nil || !ok {
		return Round{}, false, err
	}
	r, err := d.runRound(ctx, idx)
	if err != nil {
		return r, true, err
	}
	return r, true, nil
}

// nextRound returns the smallest round index in [1, Rounds] whose decision is
// not yet recorded, and false when all are.
func (d *Discussion) nextRound(ctx context.Context) (int, bool, error) {
	st, err := d.state.Load()
	if err != nil {
		return 0, false, err
	}
	subj, err := d.knowledge.Subject(ctx, d.cfg.Subject)
	if err != nil {
		return 0, false, err
	}
	for i := 1; i <= d.Rounds(); i++ {
		if !st.Has(decisionID(subj, i)) {
			return i, true, nil
		}
	}
	return 0, false, nil
}

// runRound executes one round: build the bounded context, dispatch it, then
// write-ahead the publish intent and record the decision through the replay
// gate. See docs/discuss.md for the boundary order.
func (d *Discussion) runRound(ctx context.Context, idx int) (Round, error) {
	subj, err := d.knowledge.Subject(ctx, d.cfg.Subject)
	if err != nil {
		return Round{}, err
	}
	prior, err := d.knowledge.Decisions(ctx, d.cfg.Subject)
	if err != nil {
		return Round{}, err
	}
	payload, err := d.contextPayload(subj, prior, idx)
	if err != nil {
		return Round{}, err
	}

	id := decisionID(subj, idx)
	cand := decision.Candidate{ID: id}
	out, err := d.worker.DispatchContext(ctx, cand, payload)
	if err != nil {
		return Round{}, fmt.Errorf("discuss: dispatch round %d of %s: %w", idx, subj.Line, err)
	}
	d.crash(loop.BoundaryAfterDispatch)

	in := loop.Intent{
		CandidateID: id,
		Kind:        loop.IntentPublish,
		Target:      publishTarget(subj, id),
		Publish: &loop.PublishIntent{
			Project:   d.cfg.Project,
			Namespace: subj.Namespace,
			Type:      DecisionType,
			ID:        id,
			// "decisions" is the Architecture dimension of a dec-
			// artifact (conformance rule 6 requires one).
			Dimension: "decisions",
			Content:   d.decisionContent(subj, idx, out.Output),
		},
		Step: loop.Step{CandidateID: id, Kind: KindRound, CreatedSeq: uint64(idx)},
	}
	if err := d.beginIntent(in); err != nil {
		return Round{}, err
	}
	d.crash(loop.BoundaryAfterIntent)

	if d.recorder == nil {
		return Round{}, fmt.Errorf("discuss: no recorder configured for round %d of %s", idx, subj.Line)
	}
	if _, err := d.recorder.Reconcile(ctx, in); err != nil {
		return Round{}, fmt.Errorf("discuss: record round %d of %s: %w", idx, subj.Line, err)
	}
	d.crash(loop.BoundaryAfterApply)

	if err := d.completeIntent(id); err != nil {
		return Round{}, err
	}
	r := Round{Index: idx, DecisionID: id}
	d.observe(r)
	return r, nil
}

// Resume reconciles every in-flight intent left by a previous run BEFORE any
// new round, then clears it. It never dispatches: the persisted publish
// payload is re-issued through the replay gate (an identical payload is a
// no-op, so no decision is recorded twice).
func (d *Discussion) Resume(ctx context.Context) error {
	st, err := d.state.Load()
	if err != nil {
		return err
	}
	if len(st.InFlight) == 0 {
		return nil
	}
	if d.recorder == nil {
		return fmt.Errorf("discuss: %d in-flight intent(s) but no recorder to reconcile", len(st.InFlight))
	}
	for _, in := range st.InFlight {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := d.recorder.Reconcile(ctx, in); err != nil {
			return fmt.Errorf("discuss: reconcile %s: %w", in.CandidateID, err)
		}
		if err := d.completeIntent(in.CandidateID); err != nil {
			return err
		}
		d.observe(roundFromIntent(in))
	}
	return nil
}

// contextPayload builds the byte-bounded markdown context for one round:
// instructions, the subject brief, and the already-recorded prior decisions,
// all laid out deterministically by internal/render.
func (d *Discussion) contextPayload(subj Subject, prior []Decision, idx int) ([]byte, error) {
	r, err := d.resolvedRole()
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("discussion: %s (round %d/%d)", subjectTitle(subj), idx, d.Rounds())
	instruction := fmt.Sprintf(
		"You are the %q role in a bounded discussion of %s.\n\nProduce this round's decision for the work item. A later phase will execute from the recorded decisions, so state the decision and its consequences plainly.",
		r.Name, subj.Line,
	)
	sections := []render.Section{
		{Kind: render.KindInstruction, Title: "Instructions", Body: instruction},
		{Kind: render.KindKnowledge, Title: "Subject", Body: subjectBody(subj)},
	}
	if body := decisionsBody(prior); body != "" {
		sections = append(sections, render.Section{Kind: render.KindKnowledge, Title: "Prior decisions", Body: body})
	}
	return render.Render(render.Input{Header: header, Sections: sections}, render.Options{MaxBytes: d.cfg.MaxBytes})
}

// decisionContent builds the dec- artifact content for one round. It is the
// deterministic projection of the worker's output into the dec- sections.
//
// ponytail: consequences/alternativesConsidered are required section keys but
// left empty; fill them when the worker seam returns structured decisions
// instead of one text blob.
func (d *Discussion) decisionContent(subj Subject, idx int, output string) map[string]any {
	return map[string]any{
		"context":                fmt.Sprintf("Round %d discussion of %s.", idx, subj.Line),
		"decision":               strings.TrimSpace(output),
		"consequences":           "",
		"alternativesConsidered": "",
	}
}

// resolvedRole resolves the configured role, defaulting the name and
// validating it against the role set when one is configured.
func (d *Discussion) resolvedRole() (role.Role, error) {
	name := d.cfg.Role
	if name == "" {
		name = RoleAuthor
	}
	if d.cfg.Roles == nil {
		return role.Role{Name: name}, nil
	}
	return d.cfg.Roles.Resolve(name)
}

// decisionID is the deterministic dec- unit id for one round. The round is
// zero-padded so lexical id order equals round order.
func decisionID(subj Subject, round int) string {
	return fmt.Sprintf("%s-dec-r%02d", subj.ID, round)
}

// publishTarget is the line form of the recorded decision.
func publishTarget(subj Subject, id string) string {
	return subj.Namespace + "/" + DecisionType + ":" + id
}

// roundFromIntent reconstructs the observed Round from a persisted intent.
func roundFromIntent(in loop.Intent) Round {
	return Round{Index: int(in.Step.CreatedSeq), DecisionID: in.CandidateID}
}

// subjectTitle returns a short non-empty display title.
func subjectTitle(s Subject) string {
	if t := strings.TrimSpace(s.Title); t != "" {
		return t
	}
	return s.Line
}

// subjectBody renders the subject brief deterministically.
func subjectBody(s Subject) string {
	if b := strings.TrimSpace(s.Brief); b != "" {
		return b
	}
	return s.Line
}

// decisionsBody renders prior decisions in deterministic round order.
func decisionsBody(ds []Decision) string {
	if len(ds) == 0 {
		return ""
	}
	var b strings.Builder
	for _, d := range orderDecisions(ds) {
		fmt.Fprintf(&b, "### Round %d\n\n%s\n\n", d.Round, strings.TrimSpace(d.Decision))
	}
	return strings.TrimSpace(b.String())
}

// orderDecisions orders decisions by round through the pure decision kernel
// (no map iteration reaches the output).
func orderDecisions(ds []Decision) []Decision {
	if len(ds) <= 1 {
		return append([]Decision(nil), ds...)
	}
	cs := make([]decision.Candidate, len(ds))
	byID := make(map[string]Decision, len(ds))
	for i, d := range ds {
		cs[i] = decision.Candidate{ID: d.ID, CreatedSeq: uint64(d.Round)}
		byID[d.ID] = d
	}
	ordered := decision.Order(cs)
	out := make([]Decision, 0, len(ordered))
	for _, c := range ordered {
		out = append(out, byID[c.ID])
	}
	return out
}

// Begin/complete/rollback are the same write-ahead protocol as internal/loop,
// expressed over loop's exported State/Store because the loop keeps its
// equivalents unexported. Store.Save normalizes (sorts, de-duplicates) the
// intent log, so the persisted bytes stay deterministic.
//
// ponytail: three ~10-line append/remove helpers, not a shared Checkpoint type;
// promote them into internal/loop if a third phase needs the same protocol.
func (d *Discussion) beginIntent(in loop.Intent) error {
	st, err := d.state.Load()
	if err != nil {
		return err
	}
	replaced := false
	for i := range st.InFlight {
		if st.InFlight[i].CandidateID == in.CandidateID {
			st.InFlight[i] = in
			replaced = true
			break
		}
	}
	if !replaced {
		st.InFlight = append(st.InFlight, in)
	}
	return d.state.Save(st)
}

func (d *Discussion) completeIntent(id string) error {
	st, err := d.state.Load()
	if err != nil {
		return err
	}
	st.InFlight = removeIntent(st.InFlight, id)
	if !st.Has(id) {
		st.Done = append(st.Done, id)
	}
	return d.state.Save(st)
}

// removeIntent returns in without the entry for id.
func removeIntent(in []loop.Intent, id string) []loop.Intent {
	out := in[:0:0]
	for _, it := range in {
		if it.CandidateID != id {
			out = append(out, it)
		}
	}
	return out
}

// crash invokes the configured crash-injection seam, if any.
func (d *Discussion) crash(boundary string) {
	if d.cfg.Crash != nil {
		d.cfg.Crash(boundary)
	}
}

// observe reports one completed round to the observability seam, if any.
func (d *Discussion) observe(r Round) {
	if d.cfg.OnRound != nil {
		d.cfg.OnRound(r)
	}
}

// BuildExecuteInput reads the recorded discussion decisions back from EKA and
// renders the bounded execute-phase input. It takes only the read seam and the
// subject line — never a Discussion value — so the input is reconstructed from
// the store, not from ephemeral session state. Identical recorded knowledge
// yields byte-identical input.
func BuildExecuteInput(ctx context.Context, k Knowledge, subjectLine string, maxBytes int) ([]byte, error) {
	subj, err := k.Subject(ctx, subjectLine)
	if err != nil {
		return nil, err
	}
	decisions, err := k.Decisions(ctx, subjectLine)
	if err != nil {
		return nil, err
	}
	sections := []render.Section{
		{
			Kind:  render.KindInstruction,
			Title: "Instructions",
			Body:  fmt.Sprintf("Execute %s using the recorded discussion decisions below. They are the authority for what to build.", subj.Line),
		},
		{Kind: render.KindKnowledge, Title: "Subject", Body: subjectBody(subj)},
	}
	if body := decisionsBody(decisions); body != "" {
		sections = append(sections, render.Section{Kind: render.KindKnowledge, Title: "Recorded decisions", Body: body})
	}
	return render.Render(
		render.Input{Header: "execute: " + subjectTitle(subj), Sections: sections},
		render.Options{MaxBytes: maxBytes},
	)
}
