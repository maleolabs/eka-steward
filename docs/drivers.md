# eka-steward drivers

**Drivers are steward config; no driver reads or writes EKA.**

A *driver* is how the steward launches and talks to one coding-agent CLI.
It is a steward-side configuration record — never an EKA knowledge object —
and it resolves **deterministically** to a launch spec (`cmd` + `args`) plus
the wire **kind** the steward uses to talk to it. Drivers live in a steward
config file and nowhere else. The package (`internal/driver`) parses and
resolves those bytes; it does not run a worker session or implement the
streaming ACP client — those are separate work items.

## Config shape

The document has a single `drivers` list. Parsing is **strict**. A driver is
declared as exactly `{tool, cmd, args}`; the wire kind is **not** config — it
is derived from the tool's registered adapter.

```yaml
# driver config — steward-owned, never EKA knowledge
drivers:
  - tool: opencode          # must name a known tool (an adapter exists)
    cmd: opencode
    args: [acp]             # `opencode acp` speaks ACP on stdio

  - tool: claude            # Claude Code — no ACP, CLI adapter
    cmd: claude

  - tool: codex             # Codex — no ACP, CLI adapter
    cmd: codex
```

| Field | Required | Meaning |
| --- | --- | --- |
| `tool` | yes | unique tool name; must have a registered adapter; non-empty |
| `cmd` | yes | executable base name (checked against the escape-hatch allow-list at run time); non-empty |
| `args` | no | argv passed to `cmd` verbatim |

## Adapter kinds

Every known tool maps to a fixed adapter that decides **how the steward
talks to it** and **how it is probed**. The kind is copied onto `Spec.Kind`,
so the difference is explicit and machine-checkable.

| Tool | Kind | Probe | Notes |
| --- | --- | --- | --- |
| `opencode` | `acp` | `ACPProbe` — one v1 `initialize` round-trip | `opencode acp`; negotiates ACP **v1** on the wire |
| `gemini` | `acp` | `ACPProbe` | optional extra; `gemini --acp`; negotiates ACP v1 |
| `claude` | `cli` | `CLIProbe` — `claude --version` | Claude Code does **not** speak ACP |
| `codex` | `cli` | `CLIProbe` — `codex --version` | Codex does **not** speak ACP |

`opencode`/`gemini` are ACP-capable (they speak JSON-RPC over stdio);
`claude`/`codex` are CLI-only and are driven as plain command-line tools. See
[`docs/acp-spike.md`](acp-spike.md) for the live handshakes.

## Resolution and selection (deterministic)

Resolution is a pure function of the config bytes — same input, same output.

- `Parse(data []byte) (*Registry, error)` decodes and validates strictly,
  then sorts drivers by `tool`. The returned `Registry` is immutable.
- `Registry.Names()` lists every tool in deterministic (sorted) order.
- `Registry.Resolve(tool) (Spec, error)` returns the launch spec, or an
  error naming an **unknown tool**.
- `Registry.Select(tool) (Driver, bool)` is the deterministic selection:
  the same tool always yields the same spec; an unknown tool returns
  `false` (no error).
- `Registry.Has(tool) bool` satisfies `role.DriverLookup`, so
  `role.Set.ValidateDrivers` can check role drivers against the registry
  without importing it.

## Capability probing (recorded)

`Registry.Probe(ctx, tool, run)` runs the tool's adapter and returns a
structured `Capabilities`

```go
type Capabilities struct {
	Kind       Kind              // acp | cli
	Version    string            // tool/agent version, if reported
	ACPVersion int               // ACP protocolVersion; 0 for a CLI adapter
	Raw        map[string]string // flattened probe detail (deterministic)
}
```

`Registry.SelectProbed(ctx, tool, run)` resolves the driver, probes it and
returns a `Driver{Spec, Caps}` with the capabilities **stored on the resolved
driver**. For ACP, `Raw` is the agent's `agentCapabilities` flattened to
`dotted.key = "value"` (sorted, so identical input → identical map). For a
CLI adapter, `Version` is the first non-empty line of `--version`.

### The `Runner` seam

Probing never spawns directly. It goes through one small interface:

```go
type Runner interface {
	Run(ctx context.Context, spec Spec) ([]byte, error)
}
```

- **Unit tests** pass a fake `Runner` (canned stdout, no subprocess), so the
  probe parse is testable in isolation. The ACP test replays the exact
  `initialize` frame captured from `opencode acp` in the spike.
- **Production** wires `ExecRunner` (`NewExecRunner(cmds...)` /
  `Registry.Runner()`), a thin adapter over [`internal/escapehatch`](boundary.md)
  whose allow-list is exactly the declared driver `cmd` values. This is the
  **only** file in `internal/driver` that imports `escapehatch`; the pure
  core (`registry.go`) imports no effectful package and the probe parse
  (`probe.go`) imports no subprocess package. A purity test enforces both.

## Unknown-tool behaviour

An unknown tool is a deterministic, named failure. `Parse` rejects a config
that declares a tool with no adapter (`driver: unknown tool "ghost"`);
`Resolve` and `Probe`/`SelectProbed` return an error wrapping
`ErrUnknownTool` that names the tool; `Select` returns `false`. Test with
`errors.Is(err, driver.ErrUnknownTool)`.

## Strictness rules

`Parse` rejects, with a deterministic error:

- unknown fields (top-level or inside a driver) — via `KnownFields(true)`;
- a field named `kind` (the kind is derived, not configured);
- a driver with an **empty `tool`**;
- a driver with an **empty `cmd`**;
- **duplicate tool** names;
- a tool with **no registered adapter**;
- malformed YAML, and empty/blank input.

An empty driver list (`drivers: []`) is a valid empty registry.

## Relation to the streaming ACP client (deferred)

`ACPProbe` is a **one-shot** capability probe: it reads the agent's single
`initialize` response and stops. It is **not** the streaming ACP client.

The `Runner` returns whole captured stdout, and `internal/escapehatch.Run`
has no stdin pipe yet, so the real `initialize` request cannot be fed to
`opencode acp` through `ExecRunner` today. That is why the ACP probe is
implemented and tested against the `Runner` seam (fake runner), and real
subprocess probing in this work item is limited to CLI adapters
(`claude --version`, `codex --version`), which need no stdin. The streaming
ACP client — a real stdio pipe, permission round-trips, prompt turns — is a
**later work item** (see the ADR draft in [`docs/acp-spike.md`](acp-spike.md)
§11) and will add the audited stdin-bearing adapter there.