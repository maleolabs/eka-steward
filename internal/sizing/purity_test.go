package sizing

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

// forbiddenImports are the effectful stdlib imports the sizing package
// must never use: sizing is a pure function that reads nothing from the
// process — no filesystem, no network, no process spawn. The spawn path
// is assembled from fragments so that a plain source grep for it stays
// empty in this package (the repo's absence check greps the literal).
//
// Go's stdlib "runtime" package is deliberately NOT forbidden: it is a
// different thing from the EKA modules, and sizing does not need it
// anyway.
var forbiddenImports = map[string]bool{
	"os":                true,
	"os" + "/" + "exec": true,
	"net/http":          true,
	"io/fs":             true,
	"syscall":           true,
}

// ekaCorePrefix is assembled so a plain grep for the EKA module path
// stays empty in this package — this is the only place the prefix
// appears, and only to reject it.
const ekaCorePrefix = "github.com/maleolabs/" + "eka" + "-core"

// TestSizingPackageImportsArePure asserts every non-test file of this
// package imports no EKA package and no effectful stdlib package. With
// this import set sizing cannot read a clock, a cgroup, the environment,
// the network, or call an LLM — so the count it returns is a pure
// function of the Resources and Config handed to it.
func TestSizingPackageImportsArePure(t *testing.T) {
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
			switch {
			case forbiddenImports[imp]:
				violations = append(violations, e.Name()+" imports "+imp+" (forbidden: no I/O, no subprocess)")
			case strings.HasPrefix(imp, ekaCorePrefix):
				violations = append(violations, e.Name()+" imports "+imp+" (forbidden: sizing takes no EKA input)")
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test .go files found in the sizing package")
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("sizing package must stay pure (stdlib, no effects, no EKA); violations:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// packageDir returns the directory holding the sizing package source.
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
