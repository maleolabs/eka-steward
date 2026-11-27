package decision

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// allowedImports is the pinned import allow-list for the decision
// package's NON-test files. Every entry is pure stdlib with no observable
// effect. This is what makes the package determinism-safe by construction:
// with only these imports it cannot read a clock, draw randomness, touch
// the filesystem / network, spawn a process or call an LLM / agent client.
//
// Adding an import here is a deliberate determinism review: if a future
// change needs an effectful dependency it belongs in a different package
// (the loop, pool or driver layer), not in the pure decision kernel.
var allowedImports = map[string]bool{
	"cmp":    true,
	"slices": true,
}

// TestDecisionPackageImportsArePure asserts every non-test file of this
// package imports only allow-listed, effect-free stdlib packages. It is
// the "no LLM / no I/O by construction" test for the decision seam: with
// this import set the package has no way to call an LLM, read a clock or
// spawn a process, so any decision it makes must be a pure function of its
// input.
func TestDecisionPackageImportsArePure(t *testing.T) {
	dir := packageDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	checked := 0
	var violations []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		checked++
		path := filepath.Join(dir, e.Name())
		for _, imp := range fileImports(t, path) {
			if !allowedImports[imp] {
				violations = append(violations, e.Name()+" imports "+imp+" (not in allowedImports; effectful / nondeterministic imports are forbidden here)")
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test .go files found in the decision package")
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("decision package must stay pure (stdlib, no effects); violations:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// packageDir returns the directory holding the decision package source.
func packageDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return dir
}

// fileImports returns the import paths declared by the file at path.
func fileImports(t *testing.T, path string) []string {
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
