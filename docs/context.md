# eka-steward context payload

**The context renderer is pure and NEVER writes EKA.** It turns a bounded
selection of knowledge into the deterministic markdown payload handed to a
steward worker over ACP. The caller maps EKA knowledge into an `Input`
struct; the renderer only lays those bytes out. Rendering is a one-way,
in-memory transformation — same `Input` + `Options` in, same bytes out,
always.

The renderer lives in `internal/render` (stdlib only; imports no EKA package
and no `os`/`os/exec`/`net/http`/`io/fs`, pinned by
`internal/render/purity_test.go`). It is not the control loop and does not
touch the network, a clock, randomness, an LLM, or the EKA store.

## Payload shape

```
# <Header>

## <Title>
<Body>

## <Title>
<Body>
```

- The header block (`# <Header>`) is emitted only when `Input.Header` is
  non-empty. It is the payload identity (task id + title).
- One block per `Section`, in the caller's `Input.Sections` order. A section
  block is `## <Title>` plus `\n\n` plus `Body`; an empty `Body` renders the
  heading alone.
- Blocks are separated by a blank line (`\n\n`) and the payload ends with a
  single trailing newline (`\n`). An empty input renders as the empty string.
- Section order is the caller's order. The renderer only **removes** or
  **shortens** to fit the budget; it never reorders content.

The exact bytes for the representative input are pinned in
`internal/render/testdata/context.golden`.

## Byte budget

`Options.MaxBytes` is a hard cap. `<= 0` uses `DefaultMaxBytes` (8192).
`Render` **never returns a payload larger than `MaxBytes`** for any budget at
or above the floor: the returned bytes always satisfy `len(out) <= MaxBytes`.

`MinMaxBytes` (64) is the floor. A positive `MaxBytes` below it cannot carry
even the minimum payload, so `Render` returns `ErrBudgetTooSmall` rather than
guessing. `MaxBytes <= 0` is *not* an error — it selects the default.

The argument to `Render` is never mutated (all reduction is on the renderer's
own copy of the chunks).

## Truncation order

When the full content does not fit, content is reduced in this fixed,
deterministic order. Sections are reduced one at a time; the first reduction
that brings the payload within budget short-circuits the rest.

| Phase | Action | Order |
| --- | --- | --- |
| A | **Drop** non-instruction sections whole | ascending keep-rank |
| B | **Shorten** instruction bodies, then drop if still over | ascending keep-rank |
| C | **Shorten** the header, then drop it | last resort |

Within a phase, sections are processed in ascending **keep-rank**; equal
ranks are broken by **descending original index** (later sections reduce
first). The order is a total order, so it never depends on map iteration.

### Kind precedence (keep-rank)

Higher rank is kept **longer**; truncation removes lower ranks first.

| Kind | Keep-rank | Reduced |
| --- | --- | --- |
| `instruction` | 100 | last |
| `acceptance` | 80 | 4th |
| `constraint` | 60 | 3rd |
| `knowledge` | 40 | 2nd |
| `note` | 20 | 1st |
| *any other Kind* | 0 | first of all |

So the reduction order is: unknown → `note` → `knowledge` → `constraint` →
`acceptance` → `instruction` → header. An unrecognized `Kind` gets the
documented deterministic rank 0 — never a random map order. Changing this
table is an output-breaking change and must update this doc and the golden
file.

## UTF-8 safety rule

A shortened `Body` (or header) is cut at a **line boundary** where one fits:
the last `\n` at or before the byte limit, with the newline itself dropped.
When no line boundary fits, the cut is moved back to the nearest UTF-8
**rune start**, so a multibyte rune is never split. The fixed marker
`...[truncated]` (`TruncatedMarker`) is then appended. When the budget cannot
carry even the marker, the body is reduced to the empty string. The output is
therefore always valid UTF-8.

## Minimum payload

For very small (but valid) budgets the renderer degrades deterministically: it
drops lowest-priority content first and finally shortens the header, so the
result is a documented minimum payload — the header (possibly truncated) plus
the truncation marker — rather than an error. Below `MinMaxBytes` there is no
room for even that, which is why `ErrBudgetTooSmall` exists.

## Determinism test rule

Same `Input` + `Options` → byte-identical output. This is pinned by
`TestGolden` (exact bytes), `TestByteIdentityAcrossRuns` (100 repeats),
`TestNeverOverBudget` (budget sweep) and `TestUTF8Safety`. No timestamp,
randomness, map iteration or LLM input reaches the payload.
