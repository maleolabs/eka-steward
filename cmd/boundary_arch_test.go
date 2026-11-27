package cmd

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This is the consumer-boundary architecture test (ADR-014, ADR-030):
// eka-steward is a CONSUMER of eka-core. Its public contract is
// github.com/maleolabs/eka-core/v2/api; the kernel-internal packages
// (runtime, store, workspace, sync) are private implementation details
// and must not appear in the NON-test source of this module.
//
// The test walks every non-test .go file of the module and fails if one
// imports a forbidden package. Test files are exempt: a test may import
// the internals to set up fixtures.

// forbiddenInternal are the eka-core kernel-internal import paths no
// non-test eka-steward file may depend on.
var forbiddenInternal = []string{
	"github.com/maleolabs/eka-core/v2/runtime",
	"github.com/maleolabs/eka-core/v2/store",
	"github.com/maleolabs/eka-core/v2/workspace",
	"github.com/maleolabs/eka-core/v2/sync",
}

// knownCoreGap is intentionally empty: the boundary is complete. The
// test still fails if ANY non-test file couples — keep this map empty
// (a stale entry fails too).
var knownCoreGap = map[string]string{}

func TestNoInternalCoreImportsInNonTestCode(t *testing.T) {
	root := moduleRoot(t)

	found := map[string][]string{} // relative path -> forbidden imports
	for _, rel := range nonTestGoFiles(t, root) {
		for _, p := range importsOf(t, filepath.Join(root, rel)) {
			if isForbidden(p) {
				found[rel] = append(found[rel], p)
			}
		}
	}

	// Every forbidden coupling must be exactly the pinned known gap:
	// an unknown file fails, and a stale gap entry fails.
	var violations []string
	for file, imps := range found {
		if _, ok := knownCoreGap[file]; !ok {
			sort.Strings(imps)
			violations = append(violations, file+" -> "+strings.Join(imps, ", "))
		}
	}
	for file, reason := range knownCoreGap {
		if _, ok := found[file]; !ok {
			violations = append(violations, "stale knownCoreGap entry (gap is gone; delete it): "+file+" ("+reason+")")
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("non-test code must not import eka-core internals %v; offending files:\n  %s",
			forbiddenInternal, strings.Join(violations, "\n  "))
	}
}

// escapeHatchPackage is the one and only non-test package allowed to
// spawn subprocesses via os/exec. It is deliberately a single adapter
// (internal/escapehatch) so every "escape hatch" call is audited in one
// place, and so a subprocess can never become a second path to the EKA
// store. The test below fails if any other non-test file reaches
// os/exec, and fails if this pin goes stale.
const escapeHatchPackage = "internal/escapehatch"

// TestSubprocessCallsOnlyInEscapeHatchAdapter fails if any non-test .go
// file outside internal/escapehatch imports os/exec — i.e. if the store
// could be touched through a process spawn anywhere but the one audited
// adapter. It also fails if the pinned adapter no longer imports os/exec
// (a stale pin), mirroring the knownCoreGap self-check above.
func TestSubprocessCallsOnlyInEscapeHatchAdapter(t *testing.T) {
	root := moduleRoot(t)

	var violations []string
	adapterImportsExec := false
	for _, rel := range nonTestGoFiles(t, root) {
		if !importsPath(t, filepath.Join(root, rel), "os/exec") {
			continue
		}
		if filepath.Dir(rel) == escapeHatchPackage {
			adapterImportsExec = true
			continue
		}
		violations = append(violations, rel+" imports os/exec (only "+escapeHatchPackage+" may spawn subprocesses)")
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("os/exec may only be used by the %s adapter; offending files:\n  %s",
			escapeHatchPackage, strings.Join(violations, "\n  "))
	}
	if !adapterImportsExec {
		t.Fatalf("stale pin: %s no longer imports os/exec; update or remove escapeHatchPackage", escapeHatchPackage)
	}
}

// TestNoMCPDependency asserts the steward's go.mod requires no MCP
// module: the MCP surface is not a steward dependency (docs/boundary.md).
func TestNoMCPDependency(t *testing.T) {
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.Contains(strings.ToLower(line), "mcp") {
			t.Fatalf("go.mod line %d references an MCP module (the MCP surface must not be a steward dependency): %q",
				i+1, strings.TrimSpace(line))
		}
	}
}

// isForbidden reports whether import path p names a kernel-internal
// eka-core package (exactly, or a subpackage of it).
func isForbidden(p string) bool {
	for _, f := range forbiddenInternal {
		if p == f || strings.HasPrefix(p, f+"/") {
			return true
		}
	}
	return false
}

// importsOf returns the import paths declared by the file at path.
func importsOf(t *testing.T, path string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var imports []string
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil {
			imports = append(imports, p)
		}
	}
	return imports
}

// importsPath reports whether the file at path imports importPath.
func importsPath(t *testing.T, path, importPath string) bool {
	t.Helper()
	for _, p := range importsOf(t, path) {
		if p == importPath {
			return true
		}
	}
	return false
}

// nonTestGoFiles returns the module-relative slash paths of every
// non-test .go file in the module, skipping VCS/build noise. The same
// pass backs every boundary assertion so the source set is consistent.
func nonTestGoFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS/build noise; never skip a real source dir in this module.
			if name := d.Name(); name == ".git" || name == "testdata" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walk module source: %v", err)
	}
	return files
}

// moduleRoot walks up from the test's working directory to the dir
// holding go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("boundary test: go.mod not found above the test directory")
		}
		dir = parent
	}
}
