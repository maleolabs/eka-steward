# eka-steward guardrails — worker containment

This document specifies the four containment layers the steward applies
to a worker, states the one boundary that is deliberately **not** a
security boundary, and is honest about what each layer can and cannot
enforce. The rules are enforced by tests — they are not on the honour
system.

The package is [`internal/guard`](../internal/guard). Containment is
**enabled by default and has no opt-out in v1**: the zero value of
`guard.Config` is contained, and there is deliberately no `Disable` /
`Bypass` field.

## The four layers

| # | Layer | Package | Guarantee |
| --- | --- | --- | --- |
| 1 | Environment | `guard.WorkerEnv` / `ekawrite.WorkerEnv` | a worker runs with the allow-listed env only |
| 2 | Worktree | `guard.Worktree` | each worker gets its own git worktree |
| 3 | Permission policy | `guard.Policy` | ACP permission is default-deny and bounded |
| 4 | Driver pinning | `guard.RequirePinned` | every driver is pinned (version + checksum) |

### 1. Environment — allow-listed env only

`guard.WorkerEnv(extra...)` **reuses** `ekawrite.WorkerEnv` (the single
source of the worker environment; it is never duplicated) and appends only
caller-supplied extras. Extension fails closed: `HOME`, `EKA_HOME` and
`PATH` are refused, as is any credential-looking name (`TOKEN`, `KEY`,
`SECRET`, `CREDENTIAL`, `PASSWORD`, `AUTH`).

With this environment the worker has:

- **no `HOME`/`EKA_HOME`** → the default EKA store path (`<home>/.eka`) and
  any `~`-relative host dotfile (`~/.eka`, `~/.ssh`, `~/.aws`, …) cannot be
  resolved *from the ambient environment*;
- **no `PATH`** → a bare-name `eka` (or any tool) cannot be resolved; a
  worker that must run a binary is handed its **absolute path**;
- **no credential variable** → no token/key/secret is inherited.

Negative tests: `internal/guard/guard_test.go`
(`TestWorkerEnvIsAllowListed`, `TestWorkerEnvRefusesUnsafeExtensions`).

A worker that legitimately needs a model credential gets **only the names its
driver declares**, through `guard.WorkerEnvFor(broker, tool)` — the
credential-free base plus the broker's declared keys, never `os.Environ()`. See
[`docs/secrets.md`](secrets.md).

### 2. Worktree — VCS isolation, **not** a security boundary

`guard.NewWorktree(repo, base)` issues one git worktree per worker
(`Path(worker)` is a deterministic `base/worker-<sanitized>`), created
with `git worktree add --detach` and removed with
`git worktree remove --force`. git runs through the single audited
subprocess adapter [`internal/escapehatch`](boundary.md) (an allow-list of
exactly `git`); no other package — and not `internal/guard` — imports
`os/exec`.

> **A worktree is VCS isolation, not a security boundary.** It gives each
> worker its own checkout and git bookkeeping so concurrent workers do not
> fight over one working tree. It does **not** confine the worker's
> filesystem access: a worker process can read or write outside its
> worktree (an absolute path, `/tmp`, the parent repository, another
> worker's tree) whenever the steward user can. Do not treat the worktree
> as a sandbox.

### 3. Permission policy — default-deny, bounded, never hang

`guard.Policy` is a pure decision type. `Decide` is deterministic
(same input → same answer; no clock, randomness, I/O or state) and
default-Deny. `AwaitAnswer` waits for an external answer under a
**bounded** timeout and **never hangs**: on timeout or a cancelled
context it returns `(Deny, ctx.Err())`; a responder error, a panic
inside the responder, and any non-`allow_once` option id all resolve to
`Deny`. An **unknown `optionId` is never approval**, and `allow_always`
is never auto-selected.

Decision table (`Decide`):

| Tool kind | Option | Path | Policy allows reads | Outcome |
| --- | --- | --- | --- | --- |
| any | unknown id | any | any | **Deny** |
| any | `allow_always` / reject | any | any | **Deny** |
| `read` | `allow_once` | inside root | yes | **Allow** |
| `read` | `allow_once` | escapes root via `..` / outside | yes | **Deny** |
| `read` | `allow_once` | inside root | no | **Deny** |
| `execute` / `edit` / `delete` / `move`, any other class | any | any | any | **Deny** |
| any (zero request) | any | any | any | **Deny** |

Path containment is a `filepath.Clean` + `filepath.Rel` prefix check over
**absolute** paths, so a `..` traversal is not "inside" the root.

The wait is bounded by `Policy.Timeout` (`DefaultPermissionTimeout` =
120 s when unset), matching the ACP policy in
[`docs/acp-spike.md`](acp-spike.md) §11 and the `adr:steward-acp-bridge`
intent: **deny by default, bounded wait, never hang**.

### 4. Driver pinning — exact version + checksum

`guard.RequirePinned(reg)` iterates the driver registry and **refuses any
driver that lacks either an exact `version` or a sha256 `checksum`**, then
verifies the resolved binary's sha256 against the pin. The rule is:

- missing `version` → refused (error names the tool and `version`);
- missing `checksum` → refused (error names the tool and `checksum`);
- binary not resolvable → refused (error names the tool and the binary);
- checksum mismatch (case-insensitive hex) → refused;
- empty registry → vacuously pinned.

Resolution and hashing use `os` + `crypto/sha256` only — **no `os/exec`**.
A `cmd` containing a path separator is used as-is; a bare name is looked up
on `PATH`. `driver.Parse` still *accepts* an unpinned driver; the **guard**
is what refuses it, so pinning is a containment gate, not a parse rule.
Tests: `internal/guard/pinning_test.go`.

## What is and is not enforceable (honest ceilings)

These layers raise the cost of escape and remove ambient paths; none of
them is a kernel sandbox. Stated plainly:

- **Absolute-path execution bypasses the `PATH` argument.** The env layer
  removes bare-name resolution, but a worker handed (or able to guess) an
  absolute path can still execute it. Confining execution needs a sandbox
  (namespaces, seatbelt, job objects), which this layer is not.
- **The env layer removes the *default* store path, not filesystem read
  access.** A worker inherits the steward user's filesystem permissions and
  can read any file that user can read if it knows the absolute path
  (e.g. an absolute `~/.ssh/id_ed25519`). "No `HOME`" removes `~`
  expansion and the default `.eka` location; it is not read protection.
- **The worktree is not confinement** (see above).
- **Permission policy is a library, not yet a live session.** `Decide` /
  `AwaitAnswer` are the decision and bounded-wait primitives; wiring them
  into a running ACP worker session is the streaming-client work item. Until
  then the policy is tested in isolation.
- **Checksum validation has a TOCTOU window.** `RequirePinned` hashes the
  binary at validation time; a binary swapped between validation and spawn
  is not caught. Validate immediately before exec — or stage a
  content-addressed copy — to close the window.
- **Egress is not enforced here.** Network egress is described by a declarative
  allow-list (`internal/secrets.EgressPolicy`); no layer of this package blocks
  a worker's network. A sandbox/network namespace (or a host firewall) must
  enforce it — see [`docs/secrets.md`](secrets.md) §4.
- **Allow-list checks the base name only** (see
  [`docs/boundary.md`](boundary.md)); it narrows *which* executables the
  steward launches, not what a launched worker may itself do.

## Default-on, no opt-out

`guard.Config`'s zero value **is contained** and there is no field to turn
containment off in v1. A test asserts the zero config is contained
(`TestZeroConfigIsContained`) and that no `Config` field looks like an
opt-out (`TestConfigHasNoOptOutField`). Adding an opt-out would make
"contained" a configuration choice instead of a property, so it is omitted
on purpose.

## Where this is enforced

| Guarantee | Test |
| --- | --- |
| allow-listed worker env (no HOME/EKA_HOME/PATH/token) | `guard_test.go` |
| per-worker worktree create/remove | `worktree_test.go` |
| permission default-deny / bounded / never-hang | `permission_test.go` |
| driver pinning refused/verified | `pinning_test.go` |
| containment default-on, no opt-out | `guard_test.go` |
| no `os/exec` outside `internal/escapehatch` | `cmd/boundary_arch_test.go` |
