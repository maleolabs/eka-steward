# eka-steward secrets, credentials and egress

This document specifies how a worker receives credentials, how the steward's
own git remote operations authenticate, and the **declarative** worker-egress
allow-list — including an honest statement of what the steward can and cannot
enforce. The rules are enforced by tests; they are not on the honour system.

The package is [`internal/secrets`](../internal/secrets); the credential
declaration table lives in
[`internal/driver/credentials.go`](../internal/driver/credentials.go).

## 1. Credential declarations — per driver, declarative

A driver declares the **environment-variable names** its tool needs. The
declaration is the whole ask: a driver that needs no credential declares an
empty list. The fixed table (`driver.DeclaredCredentials()` /
`driver.CredentialsFor(tool)`) is:

| Tool | Declared credential |
| --- | --- |
| `claude` | `ANTHROPIC_API_KEY` |
| `codex` | `OPENAI_API_KEY` |
| `opencode` | `OPENCODE_API_KEY` |
| `gemini` | `GEMINI_API_KEY` |

Every registered adapter has an entry (a drift between the adapter table and
the declaration table fails a test). The set is per **tool**; a tool with more
than one possible provider declares the single key for the endpoint it is
configured against.

## 2. The broker — least privilege, never `os.Environ()`

`secrets.Broker` turns a driver's declaration into a minimal environment for
that driver, from an injected [`Resolver`](../internal/secrets/secrets.go):

- `Broker.EnvFor(tool) (map[string]string, error)` resolves **exactly** the
  declared names — never one more. A missing or empty declared value is a
  deterministic error naming the tool and the variable; a tool with no
  declaration is refused.
- `Broker.WorkerEnv(base, tool)` composes `base` (the credential-free
  `guard.WorkerEnv()`) with **only** the declared credentials, as sorted
  `NAME=value` entries.
- `Broker.RedactorFor(tool)` returns an `obs.Redactor` seeded with the resolved
  values, so a transcript logged for that tool masks exactly those secrets.

**The host environment is never blanket-inherited.** The broker reads only its
`Resolver` — there is no `os.Environ()` call anywhere in the package. So the
ambient `GH_TOKEN`, `SSH_AUTH_SOCK`, `AWS_SECRET_ACCESS_KEY`, `HOME` (and any
other host secret) do **not** reach a worker, unless the tool's own declaration
names it.

### Sources of values

- `secrets.MapResolver` — an in-memory map (tests, injected values).
- `secrets.FileResolver` — a `StateDir`-based dotenv file, one `NAME=value`
  per line (blank lines and `#` comments ignored). A missing file means "no
  credentials configured", not an error; a malformed line is a deterministic
  error. The production caller passes a path under the steward's `StateDir`
  (e.g. `<StateDir>/secrets.env`).

### Wiring the worker env

`guard.WorkerEnvFor(broker, tool)` is the one sanctioned way a credential
reaches a worker: the credential-free allow-listed base
(`guard.WorkerEnv` → `ekawrite.WorkerEnv`) **plus** the broker's declared names.
The base carries no credential; the broker adds **only** the declared keys.
`guard.WorkerEnv` itself still **refuses** a credential-looking extension, so a
caller cannot smuggle an undeclared secret in through `extra...`.

Tests: `internal/secrets/secrets_test.go`, `internal/guard/secrets_test.go`.

## 3. Scoped git auth for the steward's own remote operations

The steward's **own** git remote operations (e.g. `git push`) must not ride on
the host's inherited credentials. They authenticate with a **scoped,
broker-provided** token:

- git is pointed at an askpass helper (`GIT_ASKPASS`);
- the helper reads a token (`EKA_STEWARD_GIT_TOKEN`) bound to one configured
  remote (`EKA_STEWARD_GIT_REMOTE`), so it answers only for that remote.

`secrets.Broker.GitCreds(remote)` resolves the helper and token from the
broker (never `os.Environ()`) and returns the scoped entries;
`guard.RemoteGitEnv(broker, remote)` adds them to the credential-free git base
(no `HOME`, no `PATH`, no host credential, no host ssh agent). The host
`GH_TOKEN` / `SSH_AUTH_SOCK` are never present by default. This is the
**plumbing only** — no network call is made or assumed; the env/credential path
is tested deterministically.

> **Honest boundary.** This is the *credential-scoping* path. It does not, by
> itself, replace a git credential helper if one is configured globally; the
> git base sets `GIT_CONFIG_NOSYSTEM=1` and runs with an explicit environment
> (never the parent's, via `internal/escapehatch`), which is what removes the
> host credential from the ambient path.

Tests: `internal/secrets/git_test.go`, `internal/guard/secrets_test.go`.

## 4. Egress — declarative allow-list, with an honest enforcement gap

`secrets.EgressPolicy{Allow []string}` is the **declarative** worker-egress
allow-list (the model endpoints). `Validate()` checks every entry is a bare,
non-empty host pattern (no scheme, path, port or whitespace) and `Allowed(host)`
applies the documented matching rule:

> **Matching rule.** Case-insensitive. An exact entry matches only that host. An
> entry beginning with `.` matches the domain itself **and** any subdomain
> (`.example.com` matches `example.com` and `a.example.com`). An empty host is
> never allowed.

`DefaultEgressPolicy()` names the model endpoints that correspond to the
declared credentials (`api.anthropic.com`, `api.openai.com`,
`generativelanguage.googleapis.com`, `opencode.ai`).

> **The steward cannot enforce network egress.** A worker process inherits the
> host network; nothing in this package can stop a process from opening a socket
> to a disallowed host. `EgressPolicy` is a validated, testable **description**
> of the intended allow-list — the contract a **future sandbox / network
> namespace (or a host firewall)** enforces. It is **not** an enforcement
> mechanism, and this change does not claim one. A test asserts the rule and the
> validation, not that egress is blocked.

Tests: `internal/secrets/egress_test.go`.

## What is and is not enforceable (honest ceilings)

- **Credential scope is enforced by construction.** A worker's env is the
  declaration only; the broker has no path to `os.Environ()`. This is a property
  of the code, tested.
- **Egress is NOT enforced by the steward.** The allow-list is declarative until
  a sandbox/netns/firewall enforces it (see §4).
- **A token is a secret in the child's memory.** Once handed to a worker, the
  worker (or a process it spawns) can read and re-send it; the broker limits
  *what* is handed over, not what the holder does with it.
- **`FileResolver` is only as protected as its file mode.** It reads a file under
  `StateDir`; the operator owns that file's permissions.

## Where this is enforced

| Guarantee | Test |
| --- | --- |
| declaration covers every adapter; exact set | `internal/driver/credentials_test.go` |
| broker returns exactly the declared set; foreign secret absent | `internal/secrets/secrets_test.go` |
| missing credential → deterministic error (names tool + var) | `internal/secrets/secrets_test.go` |
| broker never reads `os.Environ()` | `internal/secrets/secrets_test.go` |
| worker env = base + declared only; host secrets absent | `internal/guard/secrets_test.go` |
| env map deterministic (sorted) | `internal/secrets/secrets_test.go` |
| egress rule + validation | `internal/secrets/egress_test.go` |
| scoped git auth; host `GH_TOKEN`/`SSH_AUTH_SOCK` unused by default | `internal/secrets/git_test.go`, `internal/guard/secrets_test.go` |
