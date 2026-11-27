# Release checklist — `eka-steward`

Executed by the release owner. Every step references
[`.github/workflows/release.yml`](../.github/workflows/release.yml); no step is
invented here. Do **not** push tags or create releases from a working session —
this is the owner's action after the PR is merged.

Throughout this checklist, `<version>` is the release version in
[`anvil.yaml`](../anvil.yaml) → `project.version` (e.g. `2.2.0`); `v<version>` is
its git tag spelling.

## 0. Preconditions

- The release PR is **merged to the default branch** (`develop`).
- `anvil.yaml` → `project.version` is **`<version>`** and matches the tag you are
  about to create (the workflow **fails fast** on a mismatch):
  ```sh
  grep -E '^[[:space:]]*version:' anvil.yaml   # → <version>
  ```
- No release exists for this version yet (the workflow refuses if one does):
  ```sh
  gh release view v<version>   # expect: release not found
  ```
- `go.mod` pins `github.com/maleolabs/eka-core/v2 v2.0.1` (state it in the notes).

## 1. Tag the release (one of)

**Path A — tag push (primary).** Triggers the `Release` workflow on `v*`:

```sh
git checkout main && git pull
git tag v<version>
git push origin v<version>
```

**Path B — manual dispatch** (re-run / release without pushing a tag):

```sh
gh workflow run Release -f version=<version>
```

`scripts/bump.sh major` performs path A end-to-end (CI gate → `anvil project
version set` → commit → push → tag → push). If `anvil.yaml` is already
`<version>`, use the raw `git tag`/`git push` form above.

## 2. Let the workflow run

The workflow executes, in order: extract version → semver guard → **guard
version vs `anvil.yaml`** → local tag (dispatch path) → release-exists check →
`anvil pipeline ci` → `anvil pipeline build` (binaries) → `anvil artifact
package` → stage `install.sh`/`install.ps1` → `SHA256SUMS.txt` → `gh release
create`.

```sh
gh run watch   # or: gh run list --workflow Release
```

A failure before `gh release create` publishes nothing; fix and re-run.

## 3. Confirm release assets + checksums

```sh
gh release view v<version>
```

Expect these assets:

- `eka-steward-linux-amd64`, `eka-steward-linux-arm64`
- `eka-steward-darwin-amd64`, `eka-steward-darwin-arm64`
- `eka-steward-windows-amd64.exe`, `eka-steward-windows-arm64.exe`
- `install.sh`, `install.ps1`
- `SHA256SUMS.txt`
- the source artifact (`anvil artifact package` tarball)

Verify checksums cover every binary:

```sh
gh release download v<version> --pattern SHA256SUMS.txt
grep -c 'eka-steward-' SHA256SUMS.txt   # → 6 binaries
```

## 4. Verify the install path resolves

```sh
curl -fsSL https://github.com/maleolabs/eka-steward/releases/download/v<version>/install.sh | sh -s -- --to /tmp/eka-steward-bin
/tmp/eka-steward-bin/eka-steward version   # → eka-steward <version> / eka-core api v1
```

(Installer verification is fail-closed: a missing `SHA256SUMS.txt` or a
checksum mismatch aborts the install.)

## 5. Post

- Paste [`RELEASE_NOTES.md`](../RELEASE_NOTES.md) into the GitHub release body
  if `--generate-notes` output is not detailed enough.
- Confirm the tag exists in the independent `v*` namespace of this repo only:
  ```sh
  git ls-remote --tags origin 'v<version>'
  ```
- Nothing is synced to the `eka-cli`/`eka-mcp`/`eka-core` trains.
