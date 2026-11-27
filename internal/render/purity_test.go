package render

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

// forbiddenImports are the "leaves the process / reaches EKA" imports the
// render package must never use. Rendering takes Input bytes and returns
// output bytes: no filesystem, no network, no subprocess, and no EKA (core
// or client) access whatsoever. With this import set the package cannot
// write the EKA store — the "renderer never writes EKA" guarantee.
var forbiddenImports = map[string]bool{
	"os":       true, // filesystem, env, exec host
	"os/exec":  true, // process spawn
	"net/http": true, // network
	"io/fs":    true, // filesystem
}

// ekaCorePrefix is assembled so a plain grep for the EKA module path stays
// empty in this package — this is the only place the string appears, and
// only to reject it.
const ekaCorePrefix = "github.com/maleolabs/" + "eka" + "-core"

// TestRenderPackageImportsArePure asserts every non-test file of this
// package imports no EKA package and no effectful stdlib package
// (filesystem, network, process spawn). With this import set the package
// cannot read or write EKA, cannot read a clock, and cannot reach an LLM:
// the payload is a pure function of its Input and Options.
func TestRenderPackageImportsArePure(t *testing.T) {
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
				violations = append(violations, e.Name()+" imports "+imp+" (forbidden: the renderer never reads or writes EKA)")
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test .go files found in the render package")
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("render package must stay pure (stdlib, no EKA, no I/O); violations:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// packageDir returns the directory holding the render package source.
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
