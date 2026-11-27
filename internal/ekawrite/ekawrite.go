// Package ekawrite is the ONE AND ONLY EKA write path of eka-steward.
//
// eka-steward is a CONSUMER of eka-core (ADR-014, ADR-030): its only
// supported contract is github.com/maleolabs/eka-core/v2/api. Every EKA
// MUTATION the steward ever performs goes through this package — no other
// non-test package may call the mutating surface of an api.Authoring value
// (NewDraft, Publish, PublishBatch, Transition, Relate, NoteDraft,
// DiscardDraft, Sync, SyncAdopt). That rule is enforced by
// cmd/boundary_arch_test.go (TestEKAMutationsOnlyInEkawrite), not on the
// honour system. Reading EKA (api.Knowledge, api.Resolver, api.Workspace,
// api.Integrity, api.Contexts) stays allowed everywhere.
//
// # Why a package and not a call site
//
// Concentrating the write surface in one place buys three invariants that
// are otherwise impossible to keep:
//
//  1. Correct attribution. Every write carries a deterministic author
//     identity (kind agent for a steward-originated write, worker for a
//     worker-originated one); the caller can never forget or spoof it.
//  2. Replay safety. api.Authoring.Publish is FORWARD-ONLY and api exposes
//     no store.PutUnit, so a byte-identical re-issue would otherwise
//     allocate a fresh instance version every time. This package guarantees
//     no-op-on-replay with a deterministic read-then-write gate (see
//     Publish).
//  3. A single audited place to reason about ADR-032 provenance and the
//     worker environment (see docs/write-path.md).
//
// # Provenance and adopt (ADR-032)
//
// Publish writes WORKSPACE-NATIVE knowledge: the persisted reference is
// attributed to the provenance sentinel source_repo = "runtime", so it
// never appears in any repository snapshot. To make a steward-authored unit
// travel with a repository, the caller runs an EXPLICIT Adopt (SyncAdopt),
// which re-attributes the reference to the repository's provenance; the
// next Sync push then emits it into <repo>/exchange/snapshots. Publish never
// auto-adopts and never auto-syncs. The per-write-class contract is stated
// in docs/write-path.md.
package ekawrite

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-core/v2/conformance"
	"github.com/maleolabs/eka-core/v2/exchange"
)

// ErrConcurrentWrite marks a publish refused because the line advanced
// between the read-then-write gate's read and the Publish call: another
// writer allocated the version this call intended. The caller may retry
// (a fresh read then succeeds as a no-op when the concurrent write carried
// the same payload); it must not be treated as a transient store failure.
var ErrConcurrentWrite = errors.New("ekawrite: concurrent write detected on the line")

// Writer is the steward's EKA write gateway. Construct it with NewSteward
// (steward-originated writes, author kind "agent") or NewWorker
// (worker-originated writes, author kind "worker"); the author is fixed for
// the Writer's lifetime, so attribution is deterministic and the caller
// cannot forget it. A Writer is safe for sequential use; it holds no cache
// and reads the line on every Publish.
type Writer struct {
	rt     api.Runtime
	author conformance.AuthorIdentity
}

// NewSteward returns a Writer whose writes are attributed to the steward
// (author kind "agent", name "eka-steward").
func NewSteward(rt api.Runtime) *Writer {
	return New(rt, conformance.AuthorIdentity{Kind: conformance.KindAgent, Name: "eka-steward"})
}

// NewWorker returns a Writer whose writes are attributed to a worker
// (author kind "worker", name "eka-worker"). The name is deterministic: a
// per-worker distinction is display metadata, never identity, so it does
// not enter the store.
func NewWorker(rt api.Runtime) *Writer {
	return New(rt, conformance.AuthorIdentity{Kind: conformance.KindWorker, Name: "eka-worker"})
}

// New returns a Writer with an explicit author identity. An empty kind
// defaults to "agent"; an unknown kind is refused at the first write (ready),
// not silently coerced. An empty name defaults from the kind
// ("eka-worker" for a worker, else "eka-steward").
func New(rt api.Runtime, author conformance.AuthorIdentity) *Writer {
	if author.Kind == "" {
		author.Kind = conformance.KindAgent
	}
	if author.Name == "" {
		if author.Kind == conformance.KindWorker {
			author.Name = "eka-worker"
		} else {
			author.Name = "eka-steward"
		}
	}
	return &Writer{rt: rt, author: author}
}

// Runtime returns the Runtime the Writer writes through (read-only use).
func (w *Writer) Runtime() api.Runtime { return w.rt }

// Author returns the author identity every write of this Writer carries.
func (w *Writer) Author() conformance.AuthorIdentity { return w.author }

// ready reports whether the Writer can reach an initialized workspace and
// carries a valid author kind.
func (w *Writer) ready() error {
	if w == nil || w.rt == nil {
		return errors.New("ekawrite: nil runtime")
	}
	if !conformance.IsAuthorKind(w.author.Kind) {
		return fmt.Errorf("ekawrite: unknown author kind %q; expected one of %s", w.author.Kind, strings.Join(conformance.AuthorKinds, " | "))
	}
	if !w.rt.Exists() {
		return errors.New("ekawrite: the EKA workspace is not initialized")
	}
	return nil
}

// --- Publish (the unit write class) ---

// PublishInput describes one knowledge unit to publish. Identity, project
// and content are explicit: the Writer never infers a project from the
// current directory (a hidden cwd dependency would make writes
// non-deterministic), so Project is required.
type PublishInput struct {
	// Project is the project scope the draft is scaffolded under
	// (drafts/<project>/); it must be the project of the repository the
	// unit is destined for when the caller intends to Adopt it.
	Project string
	// Namespace, Type and ID are the unit identity. Type must be an
	// authoring artifact token (conformance.IsAuthoringType).
	Namespace string
	Type      string
	ID        string
	// Content is the structured content object (the required section keys
	// of the type plus any allowed extras). A nil map scaffolds the
	// type's required sections as empty placeholders.
	Content map[string]any
	// Relationships are the authoring edges of the unit (type + target).
	// Only the canonical authoring relationship fields are persisted; the
	// rest are ignored by the draft pipeline.
	Relationships []exchange.Relationship
	// Dimension, Domain and Phase are the optional classification/context
	// fields (Phase is scp-/plan- only; Domain is the canonical
	// Engineering Domain spelling).
	Dimension string
	Domain    string
	Phase     string
}

// PublishStatus classifies the outcome of one Publish call.
type PublishStatus string

const (
	// PublishPublished: a new instance was persisted at max+1.
	PublishPublished PublishStatus = "published"
	// PublishNoOp: the line's current instance already carries the exact
	// identity, author and content digest — nothing was written and the
	// instance version was NOT bumped (the replay gate).
	PublishNoOp PublishStatus = "no-op"
)

// PublishResult is the outcome of one Publish call.
type PublishResult struct {
	// Status is "published" (a new instance) or "no-op" (the replay gate
	// matched the line's current instance).
	Status PublishStatus
	// Form is the canonical identity form of the affected instance.
	Form string
	// InstanceVersion is the instance version of that form.
	InstanceVersion int
	// ObjectHash is the content-derived object hash of the instance ("" on
	// the no-op path when the store did not report one).
	ObjectHash string
}

// Publish persists one knowledge unit as an immutable Canonical Knowledge
// Object, with the deterministic replay gate described below.
//
// The replay gate (why it exists): api.Authoring.Publish is forward-only —
// an explicit InstanceVersion is refused unless it exceeds the line's
// highest — and api exposes no store.PutUnit. A consumer therefore cannot
// express "write this exact unit" as a store-level no-op. The steward
// reconstructs PutUnit-equivalent replay safety at this layer:
//
//  1. Compute the intended unit's version-independent content digest (see
//     contentDigest — identity + author + compiled content payload +
//     normalized relationships).
//  2. Read the line first via Resolver().ResolveLine and take its CURRENT
//     (highest-version) instance.
//  3. If that instance carries the SAME author and SAME content digest, the
//     write is a NO-OP: no draft is scaffolded, Publish is never called,
//     and no max+1 version is allocated.
//  4. Otherwise the draft is scaffolded and published with an EXPLICIT,
//     deterministic InstanceVersion = max(existing)+1. The auto-assignment
//     path is never used implicitly. If Publish still refuses because a
//     concurrent writer took that version, the refusal is surfaced as
//     ErrConcurrentWrite — a deterministic conflict, never a silent bump.
//
// The digest covers the compiled content payload (the same bytes the store
// content-addresses by), the identity, the author, and the normalized
// relationships. Two writes of the same unit therefore produce exactly one
// instance; a changed content payload produces exactly one additional
// instance.
func (w *Writer) Publish(in PublishInput) (PublishResult, error) {
	if err := w.ready(); err != nil {
		return PublishResult{}, err
	}
	if err := in.validate(); err != nil {
		return PublishResult{}, err
	}

	want, err := w.intendedDigest(in)
	if err != nil {
		return PublishResult{}, err
	}

	units, err := w.rt.Resolver().ResolveLine(in.Namespace, in.Type, in.ID)
	if err != nil {
		return PublishResult{}, fmt.Errorf("ekawrite: cannot read line %s/%s:%s: %w", in.Namespace, in.Type, in.ID, err)
	}
	maxVersion := 0
	var current *api.KnowledgeUnit
	for _, u := range units {
		if u.Identity.InstanceVersion > maxVersion {
			maxVersion = u.Identity.InstanceVersion
			current = u
		}
	}
	if current != nil && unitDigest(current) == want {
		return PublishResult{
			Status:          PublishNoOp,
			Form:            current.CanonicalIdentityForm,
			InstanceVersion: current.Identity.InstanceVersion,
			ObjectHash:      current.Digest,
		}, nil
	}

	next := maxVersion + 1
	target := in.Namespace + "/" + in.Type + ":" + in.ID

	// A draft left behind by a previous refused run (validation failure, a
	// crash after scaffold) would collide with NewDraft's O_EXCL guard.
	// Discard the stale draft so the deterministic input wins; this is the
	// only place a draft is destroyed.
	if _, err := w.rt.Authoring().ResolveDraft(target, in.Project); err == nil {
		if _, err := w.rt.Authoring().DiscardDraft(target, in.Project, true); err != nil {
			return PublishResult{}, fmt.Errorf("ekawrite: cannot discard the stale draft %s: %w", target, err)
		}
	}

	contentFile, cleanup, err := writeContentFile(in.Content)
	if err != nil {
		return PublishResult{}, err
	}
	defer cleanup()

	if _, err := w.rt.Authoring().NewDraft(api.NewDraftRequest{
		Project:       in.Project,
		Namespace:     in.Namespace,
		Type:          in.Type,
		ID:            in.ID,
		Dimension:     in.Dimension,
		Phase:         in.Phase,
		Domain:        in.Domain,
		By:            w.author,
		Relationships: in.Relationships,
		ContentFile:   contentFile,
	}); err != nil {
		return PublishResult{}, fmt.Errorf("ekawrite: scaffold draft %s: %w", target, err)
	}

	res, err := w.rt.Authoring().Publish(target, api.PublishOptions{Project: in.Project, InstanceVersion: next})
	if err != nil {
		// Race check: if the line now holds >= next, a concurrent writer
		// took our version — a deterministic conflict, not a store error.
		if again, rerr := w.rt.Resolver().ResolveLine(in.Namespace, in.Type, in.ID); rerr == nil && maxInstance(again) >= next {
			return PublishResult{}, fmt.Errorf("%w: %s (intended instance version %d): %v", ErrConcurrentWrite, target, next, err)
		}
		return PublishResult{}, fmt.Errorf("ekawrite: publish %s: %w", target, err)
	}
	return PublishResult{
		Status:          PublishPublished,
		Form:            res.Form,
		InstanceVersion: res.InstanceVersion,
		ObjectHash:      res.ObjectHash,
	}, nil
}

// validate enforces the trust boundary of Publish: identity and project are
// non-empty, the type is an authoring token, and every path-bearing segment
// (project, namespace, type, id) is a single safe path element — a value
// carrying a separator or ".." could otherwise escape the drafts tree
// (draftPath is filepath.Join(drafts, project, type-id.json)).
func (in PublishInput) validate() error {
	for _, f := range []struct{ name, value string }{
		{"project", in.Project},
		{"namespace", in.Namespace},
		{"type", in.Type},
		{"id", in.ID},
	} {
		if err := checkSegment(f.name, f.value); err != nil {
			return err
		}
	}
	if !conformance.IsAuthoringType(in.Type) {
		return fmt.Errorf("ekawrite: type %q is not an authoring artifact token", in.Type)
	}
	return nil
}

// checkSegment refuses an empty, dot, dot-dot or separator-bearing path
// segment.
func checkSegment(name, value string) error {
	if value == "" {
		return fmt.Errorf("ekawrite: %s must be non-empty", name)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("ekawrite: %s %q is not a valid path segment", name, value)
	}
	if strings.ContainsAny(value, `/\`) || strings.ContainsRune(value, 0) {
		return fmt.Errorf("ekawrite: %s %q must not contain a path separator", name, value)
	}
	return nil
}

// --- Transition / Relate / Note (line mutation write classes) ---

// Transition performs one state transition. The change-log authority is the
// Writer's author (kind + name), always — a caller cannot attribute a
// transition to a different actor.
func (w *Writer) Transition(req api.TransitionRequest) (*api.TransitionResult, error) {
	if err := w.ready(); err != nil {
		return nil, err
	}
	req.By = w.author.Name
	req.ByKind = w.author.Kind
	return w.rt.Authoring().Transition(req)
}

// Relate adds relationships to a published line or a pending draft. Relate
// re-points the current instance without bumping the instance version, so it
// is idempotent at the core level (a duplicate edge is a no-op); the Writer
// adds no author because the engine records none.
func (w *Writer) Relate(req api.RelateRequest) (*api.RelateResult, error) {
	if err := w.ready(); err != nil {
		return nil, err
	}
	return w.rt.Authoring().Relate(req)
}

// Note creates one note draft against a target, attributed to the Writer's
// author (draft author and change-log authority).
func (w *Writer) Note(req api.NoteDraftRequest) (*api.NoteDraftResult, error) {
	if err := w.ready(); err != nil {
		return nil, err
	}
	req.By = w.author
	return w.rt.Authoring().NoteDraft(req)
}

// Discard removes one draft file without publishing. It is the explicit
// "abandon this authoring intent" write class.
func (w *Writer) Discard(target, projectHint string, force bool) (string, error) {
	if err := w.ready(); err != nil {
		return "", err
	}
	return w.rt.Authoring().DiscardDraft(target, projectHint, force)
}

// --- Sync / Adopt (transport write classes) ---

// Sync runs one synchronization of the repository at repoPath (registration,
// pull and/or push per opts). It is the transport write class: pull seeds the
// canonical store from the repository, push emits the repository's units into
// <repo>/exchange/snapshots.
func (w *Writer) Sync(repoPath string, opts api.SyncOptions) (*api.SyncResult, error) {
	if err := w.ready(); err != nil {
		return nil, err
	}
	return w.rt.Authoring().Sync(repoPath, opts)
}

// Adopt is the EXPLICIT provenance write class (ADR-032 Option C2): it
// re-attributes the repository's workspace-native units (source_repo =
// "runtime") to the repository provenance, so the next Sync push carries
// them. It is reference-only — the immutable payloads are untouched. dryRun
// computes the result without touching the store.
func (w *Writer) Adopt(repoPath string, targets []string, dryRun bool) (*api.SyncAdoptResult, error) {
	if err := w.ready(); err != nil {
		return nil, err
	}
	return w.rt.Authoring().SyncAdopt(repoPath, targets, dryRun)
}

// --- digest / normalization helpers ---

// digestVersion tags the digest algorithm; changing the covered fields
// changes the tag so a stale on-disk store cannot silently match.
const digestVersion = "ekawrite/1"

// intendedDigest computes the version-independent content digest of the unit
// a PublishInput would produce. It normalizes the content exactly as the
// draft pipeline does (required section placeholders filled, then the
// canonical ContentJSON encoding) so the digest of the intended unit equals
// the digest of the persisted unit for identical input.
func (w *Writer) intendedDigest(in PublishInput) (string, error) {
	content, err := normalizeContent(in.Content, in.Type)
	if err != nil {
		return "", err
	}
	payload, err := conformance.ContentJSON(content, conformance.RequiredSectionsFor(in.Type))
	if err != nil {
		return "", fmt.Errorf("ekawrite: cannot compile content of %s:%s: %w", in.Type, in.ID, err)
	}
	return contentDigest(in.Namespace, in.Type, in.ID, w.author, payload, in.Relationships), nil
}

// unitDigest computes the same digest from a persisted unit.
func unitDigest(u *api.KnowledgeUnit) string {
	return contentDigest(u.Identity.Namespace, u.Identity.Type, u.Identity.ID, u.Author, u.ContentPayload, u.Relationships)
}

// contentDigest is the shared digest: a SHA-256 over a NUL-delimited,
// deterministic projection of the identity, the author, the compiled content
// payload and the normalized relationships. The projection excludes the
// instance version (that is what makes a re-issue a no-op), the change log
// and the timestamps (date-derived, so they would defeat replay stability
// across a day boundary).
func contentDigest(ns, typeToken, id string, author conformance.AuthorIdentity, payload []byte, rels []exchange.Relationship) string {
	h := sha256.New()
	field := func(s string) {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	field(digestVersion)
	field(ns)
	field(typeToken)
	field(id)
	field("1") // revision: the draft pipeline's fixed revision
	field(author.Kind)
	field(author.Name)
	h.Write(payload)
	h.Write([]byte{0})
	for _, r := range normalizeRelationships(rels) {
		field(r.Type)
		field(r.Target)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// normalizeRelationships mirrors the draft pipeline's relationship
// normalization: only the canonical authoring relationship fields survive
// (an unknown field is dropped by the draft template), duplicates (type +
// target) collapse, and the result is sorted by (type, target).
func normalizeRelationships(rels []exchange.Relationship) []exchange.Relationship {
	allowed := map[string]bool{}
	for _, f := range conformance.AuthoringRelationshipFields() {
		allowed[f] = true
	}
	type key struct{ t, target string }
	seen := map[key]bool{}
	var keys []key
	for _, r := range rels {
		if r.Type == "" || r.Target == "" || !allowed[r.Type] {
			continue
		}
		k := key{r.Type, r.Target}
		if seen[k] {
			continue
		}
		seen[k] = true
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].t != keys[j].t {
			return keys[i].t < keys[j].t
		}
		return keys[i].target < keys[j].target
	})
	out := make([]exchange.Relationship, 0, len(keys))
	for _, k := range keys {
		out = append(out, exchange.Relationship{Type: k.t, Target: k.target})
	}
	return out
}

// normalizeContent mirrors the draft pipeline's content compilation: a JSON
// round-trip produces the same decoded shapes NewDraft reads from the
// content file (numbers as float64, nested objects as map[string]any), then
// the type's required section keys are filled with empty placeholders when
// absent (the tkt- "commands" placeholder carries the projection header, as
// the scaffold does).
func normalizeContent(content map[string]any, typeToken string) (map[string]any, error) {
	data, err := json.Marshal(content)
	if err != nil {
		return nil, fmt.Errorf("ekawrite: content of %s is not JSON-encodable: %w", typeToken, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("ekawrite: content of %s must be a JSON object: %w", typeToken, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	for _, section := range conformance.RequiredSectionsFor(typeToken) {
		key := conformance.SectionKey(section)
		if _, ok := out[key]; ok {
			continue
		}
		if typeToken == "tkt" && key == "commands" {
			out[key] = conformance.ProjectionHeader + "\n"
			continue
		}
		out[key] = ""
	}
	return out, nil
}

// maxInstance returns the highest instance version of a resolved line (0
// when empty). ResolveLine returns instances in ascending version order, so
// the last is the highest; the loop keeps the helper independent of that
// ordering.
func maxInstance(units []*api.KnowledgeUnit) int {
	max := 0
	for _, u := range units {
		if u.Identity.InstanceVersion > max {
			max = u.Identity.InstanceVersion
		}
	}
	return max
}

// writeContentFile writes the content object to a temp JSON file the draft
// scaffold reads (NewDraftRequest.ContentFile). The file lives outside the
// workspace, so it is not an EKA mutation. The returned cleanup removes it.
func writeContentFile(content map[string]any) (string, func(), error) {
	if content == nil {
		content = map[string]any{}
	}
	data, err := json.Marshal(content)
	if err != nil {
		return "", nil, fmt.Errorf("ekawrite: content is not JSON-encodable: %w", err)
	}
	f, err := os.CreateTemp("", "ekawrite-content-*.json")
	if err != nil {
		return "", nil, fmt.Errorf("ekawrite: cannot create a content file: %w", err)
	}
	name := f.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("ekawrite: cannot write the content file: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("ekawrite: cannot close the content file: %w", err)
	}
	return name, cleanup, nil
}
