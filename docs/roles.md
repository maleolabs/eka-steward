# eka-steward roles

**Roles are steward config; no role reads or writes EKA.**

A *role* is one configured worker role. It is a steward-side
configuration record — never an EKA knowledge object — and it resolves
**deterministically** to two things:

- a **driver** name (a registered coding-agent CLI), and
- a **prompt template** (a path or inline id).

Roles live in a steward config file and nowhere else. The role package
(`internal/role`) parses and validates those bytes; it does not execute a
role, render a prompt or implement a driver — those are separate work
items. Enforced imports: the package imports no
`github.com/maleolabs/eka-core*` package and no `os`, `os/exec`,
`net/http` or `io/fs`; config bytes are passed IN (`internal/role/purity_test.go`).

## Config shape

The document has a single `roles` list. Parsing is **strict**.

```yaml
# roles config — steward-owned, never EKA knowledge
roles:
  - name: implementer        # unique; required
    driver: claude           # must reference a registered driver by name
    template: prompts/implementer.md
    review: false            # optional; absent == false

  - name: reviewer           # a review role
    driver: claude
    template: prompts/reviewer.md
    review: true
```

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | yes | unique role name; non-empty |
| `driver` | yes | registered driver name it resolves to; non-empty |
| `template` | yes | prompt template path or inline id; non-empty |
| `review` | no | `true` marks a review role; absent means `false` |

## Resolution rules

Resolution is a pure function of the config bytes — same input, same
output.

- `Parse(data []byte) (*Set, error)` decodes and validates, then sorts
  roles by `name`. The returned `Set` is immutable.
- `Set.Resolve(name)` returns the role for `name`, or an error that names
  the unknown role.
- `Set.Names()` lists every role name in sorted (deterministic) order.
- `Set.ReviewRoles()` returns the `review: true` roles in that same order.
- `Set.ValidateDrivers(d DriverLookup)` checks every `Role.Driver`
  against a driver registry. The registry is reached only through the
  `DriverLookup interface{ Has(name string) bool }` seam, so this package
  never imports the driver registry itself. An unknown driver yields an
  error naming the role and the driver (all unknown drivers, in
  deterministic order).

## Strictness rules

`Parse` rejects, with a deterministic error:

- unknown fields (top-level or inside a role) — via `KnownFields(true)`;
- a role with an **empty `name`**;
- a role with an **empty `driver`**;
- a role with an **empty `template`**;
- **duplicate role names**;
- malformed YAML, and empty/blank input.

`review` is **optional**: absent or `review: false` both mean a
non-review role (the field is ignored when false, not required). An empty
role list (`roles: []`) is a valid empty set; blank bytes are an error.

## Review roles

Roles flagged `review: true` are the pool the steward's review stage selects a
reviewer from. Selection is deterministic (first `ReviewRoles()` name that is
not the author role); the review policy, the outcome recording and the
completion gate are in [`docs/review.md`](review.md).
