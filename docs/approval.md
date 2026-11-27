# eka-steward human approval

**A permission request the pure policy cannot decide is escalated to a human,
the decision is recorded as a typed EKA artifact, and the gate is released only
by an approving decision — with a bounded wait whose timeout is always a deny.**

The escalation stage is one more CODE decision plus one human answer: the guard
policy (`internal/guard`) classifies the request, the approval channel
(`internal/approval`) moves it to a human, and the loop (`internal/loop`) gates
the step on the decision and records it through the single write path
(`internal/ekawrite`). It imports nothing from the eka-core kernel internals and
never reads a terminal, so `cmd/boundary_arch_test.go` stays green.

## Escalation rule

`guard.Policy.Decide` is a pure function of the request:

| request                                         | outcome  |
|-------------------------------------------------|----------|
| option id ≠ `allow_once` (reject, allow_always, unknown) | `Deny` |
| `read` inside `WorkspaceRoot`, policy allows reads | `Allow` |
| `read` inside `WorkspaceRoot`, policy forbids reads | `Deny` |
| `read` outside `WorkspaceRoot`, or any non-read class (`execute`/`edit`/`delete`/`move`/unknown) | `Escalate` (default) / `Deny` when `Policy.DenyUndecidable` |

The rule keeps **default-deny for the clearly-deniable classes**: a request the
responder refused (any non-`allow_once` option) is never rescued by escalation,
and a read the policy forbids inside the root is a plain `Deny`. Only a
well-formed request outside the allow-list — the class the policy genuinely
cannot decide — escalates. `Escalate` is a *classification*, **not** approval:
the human gate (or the bounded-wait timeout) still decides.

Opting out (`Policy.DenyUndecidable = true`) restores the pre-approval
behaviour: an undecidable request is refused instead of escalated. The zero
value escalates, so an undecidable request is never silently denied.

## No-TTY approval channel

The channel is `approval.Approver`:

```go
type Approver interface {
    Ask(ctx context.Context, req Request) (Decision, error)
}
```

The **default** `Approver` is `approval.FileApprover`, a file/queue channel that
needs **no TTY** (no `os.Stdin`, no `isatty`, no `golang.org/x/term`; asserted by
`TestNoTTYInDefaultPath`):

```
<Dir>/approval-request.json    written by the steward (the question)
<Dir>/approval-response.json   written by a human/operator (the answer)
```

The steward publishes the request atomically and polls the response with a
bounded wait. A response answers a request **only** when its `key` equals the
request's deterministic key; a stale, mismatched or malformed response is
ignored, so a bad answer can only ever deny by timing out — never approve the
wrong request. `Dir` is the steward `StateDir` in production.

The request key is the SHA-256 over the request's decision inputs (option id,
tool kind, path, workspace root) — never the clock, a random id or a map order —
so a re-issued request (a resume) is the **same** approval, byte for byte.

Tests inject a fake or an `os.Pipe`-backed approver; the default path never
touches the terminal.

## Typed artifact (review-note convention)

The note schema (ADR-019 D7) fixes the `role` vocabulary at
`implementation|review|fix|reply`; an `approval` role cannot be invented. The
decision is therefore recorded with the **existing review-note convention**
(`role: review`, a **resolved** `cmt-` note that `discusses` the request target),
via `ekawrite.Writer.Approval`:

- `approve` → note `verdict: approve`;
- `deny` / `timeout` → note `verdict: changes-requested` (the schema's only
  non-approval value), distinguished from each other in `notes`;
- `notes` carry the request key, the tool/path and the human's reason, so the
  artifact is self-describing and two different decisions are distinct notes.

`internal/ekawrite` remains the **only** legal mutation site. No parallel store
is introduced.

### Replay gate

`Writer.Review` derives a fresh note id (`-2`, `-3`, …) on every call, so raw
replay would allocate a second note. `Writer.Approval` is **replay-safe**: it
first scans the project's units (deterministic order, no map iteration) for an
already-resolved review note by the same author on the same target with the same
verdict and notes, and returns a `no-op` result when one exists. A re-issued
identical decision records **exactly one** artifact.

## Bounded wait → timeout to deny

`approval.AskBounded` mirrors `guard.Policy.AwaitAnswer`'s bounded-wait
discipline: it runs the approver under `Policy.Timeout`
(`guard.DefaultPermissionTimeout` when unset) and returns `Decision{Verdict:
Deny, TimedOut: true}` on timeout or cancellation — even if the approver ignores
its context — and contains a panicking approver. **A timeout is always a deny**;
the steward never hangs waiting for a human.

## Completion gate

The loop runs the approval gate **before** dispatch:

- **approve** → the decision is recorded, then the step proceeds (review gate,
  dispatch, `Advance`, done);
- **deny** / **timeout** → the decision is recorded, the step is blocked, the
  candidate is **left un-done**, and a deterministic `loop.ApprovalRefusal` is
  surfaced from `Step`/`Run`. The worker is never dispatched.

A denial never advances or marks the item done. The item stays re-selectable by
a later `Run`.

## Resumption guarantee

The in-progress approval is the loop's existing write-ahead checkpoint
(`loop.Intent{Kind: IntentApproval}`), not a second state machine. The intent is
persisted before the human is asked and cleared (`clearIntent`, **without**
marking the candidate done) once the decision is recorded.

On resume the in-flight approval intent is reconciled by re-reading the durable
decision and re-recording it through the replay gate: the same decision is a
no-op, so a crash after recording but before clearing the intent never records a
second artifact. The forward step then runs exactly once. See
`docs/checkpoint.md` for the crash protocol this reuses.