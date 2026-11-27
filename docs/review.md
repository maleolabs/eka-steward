# eka-steward review stage

**Execution output is reviewed by a distinct, deterministically-selected review
role when the policy requires it; the outcome is recorded as a resolved EKA
review note, and the completion gate is released only by an approving review.**

The review stage is one more CODE decision plus one worker/LLM call: the loop
(`internal/loop`) decides *whether* review is required and *who* reviews; the
reviewer (a `Dispatcher`, same worker seam) decides the verdict. Selection and
outcome handling are pure functions of config and input — no map iteration, no
randomness — so the same run always produces the same reviewer and the same step
sequence. The policy types live in `internal/loop/review.go`; the recording
lives in `internal/ekawrite/review.go`.

## Policy

`loop.Config.Review` is a `ReviewPolicy`:

```go
type ReviewPolicy struct {
    Roles      *role.Set     // nil => review OFF (the zero value)
    Author     string        // the role the loop dispatches as (required when on)
    RequireFor map[string]bool // optional explicit author roles that need review
}
```

- **Zero value disables review.** With `Roles == nil` the loop is byte-identical
  to the pre-review loop: dispatch → advance → done, one dispatch per step. This
  is why every existing loop/backpressure/checkpoint test is unchanged.
- **Default rule** (enabled, `RequireFor == nil`): the author role requires
  review unless the author role is *itself* a review role (`review: true`). A
  review role's output is not re-reviewed. This is the documented default:
  *non-review work requires review*.
- **Explicit rule** (`RequireFor != nil`): review is required exactly for the
  author roles named `true` in the map. The map is only looked up, never
  iterated.

Enabling review without an `Author`, or with an author absent from the role set
under the default rule, is a deterministic error surfaced from `Step`/`Run`.

## Deterministic, distinct reviewer selection

`SelectReviewer(author string, set *role.Set) (role.Role, error)`:

1. Take `set.ReviewRoles()` — the `review: true` roles in the set's sorted
   (by-name) order.
2. Return the **first** role whose name is not the author role.
3. If none is eligible (no review role at all, or only the author role) return
   `loop.ErrNoReviewer`.

The same `(author, role set)` always yields the same reviewer, so a given
candidate is always reviewed by the same role and the step sequence is stable
across runs. **No fallback:** a configuration with no eligible reviewer is a
deterministic error, never a silent self-review.

The reviewer is handed the author's output through the *same* worker seam, in a
`ReviewRequest{Candidate, Author, Reviewer, Output}`. A `Dispatcher` opts into
the review stage by implementing `loop.ReviewDispatcher`
(`DispatchReview(ctx, ReviewRequest) (ReviewOutcome, error)`). A review-enabled
loop over a dispatcher that cannot review is a deterministic error.

## Outcome recording (review-note convention)

The outcome is persisted through the single write path (`internal/ekawrite`),
reusing the EKA review-note convention (`eka note --role review`), never a
parallel store:

- `Writer.Review(ReviewInput)`: scaffolds a `cmt-` note draft with
  `role: review`, the `verdict` and `notes` fields, and the `discusses` edge to
  the work item (`api.NoteDraft`);
- the draft's `note-state` is advanced to `resolved` (with the matching
  `open -> resolved` change-log entry — conformance rule 7 requires the last
  entry to match the state field);
- the note is published (`api.Publish`) as an immutable unit.

The note schema allows exactly two verdicts, `approve` and `changes-requested`;
the loop also accepts a `reject` verdict and normalizes it to
`changes-requested` when recording (the note always records the outcome — an
approval or a request for changes).

`internal/ekawrite` remains the **only** legal mutation site: the recording
calls `api.Authoring` only inside that package, so `cmd/boundary_arch_test.go`
stays green.

## Completion gate

The loop **never** advances or marks a candidate done unless the review
**approves**:

- **approve** → persist the transition write-ahead intent, `Advance` (→
  `Config.DispatchState`), complete the intent, record the candidate done;
- **changes-requested / reject** → do **not** advance, do **not** mark done;
  the outcome is recorded and a deterministic `loop.ReviewRefusal` is surfaced
  from `Step`/`Run`. Bounded re-work is out of scope for this cut — the item is
  left un-done and re-selectable by a later `Run`.

This aligns with the EKA done gate: a work item at `done` requires **every**
child note resolved (ADR-019 D6), and the recorded review note is resolved.

## Crash safety

For a review-required step the transition write-ahead intent is persisted
**after** approval (the author dispatch and review run before it). A crash during
the author dispatch or the review therefore leaves **no in-flight intent**, and
`Run`'s resume pass never advances an un-reviewed item. A crash after approval
but before apply is recovered by the normal checkpoint protocol
(`docs/checkpoint.md`).

Known limitation: recording the review note is not itself part of the intent log,
so a crash after the note is published but before the transition applies causes
the next `Run` to re-dispatch and re-review, allocating a second review note
(`-2` suffix). This is bounded and idempotent-on-completion, and is revisited
when bounded re-work lands.