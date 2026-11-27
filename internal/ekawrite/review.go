package ekawrite

// Review recording: the steward records a review outcome as a cmt- REVIEW NOTE
// (role=review) that discusses the work item and is RESOLVED, so the EKA done
// gate (every child note resolved, ADR-019 D6) is released only by an approving
// review. This is the same note convention `eka note --role review` uses; the
// steward does not invent a parallel store. It goes through the public authoring
// surface only: NoteDraft scaffolds the note, the draft's note-state is advanced
// to `resolved` (the explicit resolution action; ResolveNote is not on the
// public api contract), and Publish persists it as an immutable unit.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-core/v2/conformance"
)

// Review verdict vocabulary. The review-note schema allows exactly approve and
// changes-requested; VerdictReject is accepted as an alias and normalized to
// changes-requested.
const (
	VerdictApprove          = "approve"
	VerdictChangesRequested = "changes-requested"
	VerdictReject           = "reject"
)

// ReviewInput describes one review outcome to record as a resolved review note.
type ReviewInput struct {
	// RepoPath is the directory the repository is addressed from (the
	// registered repository locates the project the note is scaffolded in).
	RepoPath string
	// Target is the reviewed line: "<ns>/<type>:<id>" or "<type>:<id>".
	Target string
	// Verdict is approve | changes-requested (reject is normalized to
	// changes-requested).
	Verdict string
	// Notes are the reviewer's findings (recorded in the note's `notes` list).
	Notes []string
}

// ReviewResult reports one recorded review note.
type ReviewResult struct {
	// NoteID is the generated note id ("<subject-id>-review[-<n>]").
	NoteID string
	// Form is the canonical published form of the note.
	Form string
	// Verdict is the normalized verdict recorded in the note.
	Verdict string
}

// Review records one review outcome as a RESOLVED review note discussing
// Target. The verdict must be approve or changes-requested (reject is accepted
// as an alias for changes-requested).
func (w *Writer) Review(in ReviewInput) (ReviewResult, error) {
	if err := w.ready(); err != nil {
		return ReviewResult{}, err
	}
	verdict, ok := normalizeReviewVerdict(in.Verdict)
	if !ok {
		return ReviewResult{}, fmt.Errorf("ekawrite: review verdict %q is not one of %s, %s, %s",
			in.Verdict, VerdictApprove, VerdictChangesRequested, VerdictReject)
	}
	if in.Target == "" {
		return ReviewResult{}, fmt.Errorf("ekawrite: review needs a target line")
	}
	project, err := w.projectFor(in.RepoPath)
	if err != nil {
		return ReviewResult{}, err
	}

	notes := in.Notes
	if notes == nil {
		notes = []string{}
	}
	contentFile, cleanup, err := writeContentFile(map[string]any{
		"verdict": verdict,
		"notes":   notes,
	})
	if err != nil {
		return ReviewResult{}, err
	}
	defer cleanup()

	draft, err := w.rt.Authoring().NoteDraft(api.NoteDraftRequest{
		RepoPath:    in.RepoPath,
		Target:      in.Target,
		Role:        "review",
		By:          w.author,
		ContentFile: contentFile,
	})
	if err != nil {
		return ReviewResult{}, fmt.Errorf("ekawrite: scaffold review note for %s: %w", in.Target, err)
	}

	if err := resolveReviewDraft(draft.Path, w.author); err != nil {
		return ReviewResult{}, err
	}

	res, err := w.rt.Authoring().Publish("cmt:"+draft.ID, api.PublishOptions{Project: project})
	if err != nil {
		return ReviewResult{}, fmt.Errorf("ekawrite: publish review note %s: %w", draft.ID, err)
	}
	return ReviewResult{NoteID: draft.ID, Form: res.Form, Verdict: verdict}, nil
}

// normalizeReviewVerdict maps an accepted verdict to the note schema's value.
func normalizeReviewVerdict(v string) (string, bool) {
	switch v {
	case VerdictApprove:
		return VerdictApprove, true
	case VerdictChangesRequested, VerdictReject:
		return VerdictChangesRequested, true
	}
	return "", false
}

// resolveReviewDraft advances a scaffolded review note draft's note-state to
// "resolved" and appends the matching change-log entry (open -> resolved, by
// the writer's author). It mirrors the explicit `eka note resolve` action on a
// draft; conformance rule 7 requires the last change-log entry to match the
// state field, so both move together.
func resolveReviewDraft(path string, by conformance.AuthorIdentity) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("ekawrite: cannot read review draft %s: %w", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("ekawrite: review draft %s is not valid JSON: %w", path, err)
	}
	state, _ := doc["state"].(map[string]any)
	if state == nil {
		state = map[string]any{}
		doc["state"] = state
	}
	if state["noteState"] == "resolved" {
		return nil
	}
	state["noteState"] = "resolved"

	log, _ := doc["changeLog"].([]any)
	doc["changeLog"] = append(log, map[string]any{
		"date":   time.Now().Format("2006-01-02"),
		"domain": conformance.StateKeyCamel(conformance.DomainNoteState),
		"from":   "open",
		"to":     "resolved",
		"by":     by,
	})

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("ekawrite: cannot encode review draft %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return fmt.Errorf("ekawrite: cannot write review draft %s: %w", path, err)
	}
	return nil
}

// projectFor resolves the project of the registered repository at repoPath.
func (w *Writer) projectFor(repoPath string) (string, error) {
	abs, err := filepath.Abs(repoPath)
	if err != nil {
		return "", fmt.Errorf("ekawrite: cannot resolve %q: %w", repoPath, err)
	}
	repo, found, err := w.rt.Workspace().FindRepo(abs)
	if err != nil {
		return "", fmt.Errorf("ekawrite: cannot find repo %s: %w", abs, err)
	}
	if !found {
		return "", fmt.Errorf("ekawrite: repository %s is not registered in the workspace", abs)
	}
	return repo.ProjectID, nil
}
