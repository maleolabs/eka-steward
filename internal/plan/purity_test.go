package plan

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

// allowedImports is the pinned import allow-list for the plan package's
// NON-test files. Every entry is pure stdlib (or internal/decision, itself
// effect-free). This is what makes selection determinism-safe and LLM-free by
// construction: with only these imports the planner cannot read a clock, draw
// randomness, touch the filesystem / network, spawn a process or call an LLM /
// agent client — so any selection it makes is a pure function of its input.
//
// Adding an import here is a deliberate determinism review: an effectful
// dependency belongs in the loop / driver layer, never in the pure planner.
var allowedImports = map[string]bool{
	"cmp":     true,
	"fmt":     true,
	"slices":  true,
	"strings": true,
	"github.com/maleolabs/eka-steward/v2/internal/decision": true,
}

// TestPlanPackageImportsArePure asserts every non-test file imports only
// allow-listed, effect-free packages. It is the "no LLM / no I/O by
// construction" test for the selection seam.
func TestPlanPackageImportsArePure(t *testing.T) {
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
		t.Fatal("no non-test .go files found in the plan package")
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("plan package must stay pure (stdlib, no effects); violations:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// packageDir returns the directory holding the plan package source.
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
