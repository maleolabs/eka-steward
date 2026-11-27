package driver

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

// allowedImports is the pinned import allow-list for the driver package's
// NON-test files. Every entry is pure or a bounded, deterministic seam:
//
//   - bytes/errors/fmt/sort/strings/yaml+3: the pure parse/resolve core;
//   - context/encoding/json: the probe parse (probe.go);
//   - internal/escapehatch: the ONE subprocess seam, permitted in a SINGLE
//     file (execRunnerFile) so no other file can spawn.
//
// The package never imports an EKA package, and never imports os, os/exec,
// net or io/fs: it parses config bytes it was handed and probes only through
// the Runner seam. This is the purity/boundary guard for the package.
var allowedImports = map[string]bool{
	"bytes":            true,
	"context":          true,
	"encoding/json":    true,
	"errors":           true,
	"fmt":              true,
	"sort":             true,
	"strconv":          true,
	"strings":          true,
	"gopkg.in/yaml.v3": true,
	escapeHatchImport:  true, // only in execRunnerFile, checked below
}

// escapeHatchImport is the audited subprocess adapter path. Exactly one
// non-test file of this package may import it.
const escapeHatchImport = "github.com/maleolabs/eka-steward/v2/internal/escapehatch"

// execRunnerFile is the ONLY non-test file allowed to import escapehatch.
// Keeping it to one file keeps the escapehatch-backed Runner in the
// smallest possible place, so the core (registry.go) and the probe parse
// (probe.go) stay free of any subprocess dependency.
const execRunnerFile = "exec_runner.go"

// pureCoreFiles are the files that must NOT import context or escapehatch:
// the parse/resolve/select core is a pure function of config bytes.
var pureCoreFiles = map[string]bool{"registry.go": true}

// ekaCorePrefix is assembled so a plain grep for the EKA module path stays
// empty in this package — this is the only place the string appears, and
// only to reject it.
const ekaCorePrefix = "github.com/maleolabs/" + "eka" + "-core"

func TestDriverPackageImportsAreBounded(t *testing.T) {
	dir := packageDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}

	checked := 0
	execRunnerImportsEscape := false
	var violations []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		checked++
		path := filepath.Join(dir, e.Name())
		for _, imp := range fileImports(t, path) {
			switch {
			case strings.HasPrefix(imp, ekaCorePrefix):
				violations = append(violations, e.Name()+" imports "+imp+" (forbidden: no driver reads or writes EKA)")
			case imp == escapeHatchImport && e.Name() != execRunnerFile:
				violations = append(violations, e.Name()+" imports escapehatch (only "+execRunnerFile+" may spawn subprocesses)")
			case !allowedImports[imp]:
				violations = append(violations, e.Name()+" imports "+imp+" (not in allowedImports; effectful imports are forbidden here)")
			}
		}
		if e.Name() == execRunnerFile && importsPath(t, path, escapeHatchImport) {
			execRunnerImportsEscape = true
		}
		if pureCoreFiles[e.Name()] {
			if importsPath(t, path, "context") {
				violations = append(violations, e.Name()+" imports context (the pure core must be context-free)")
			}
			if importsPath(t, path, escapeHatchImport) {
				violations = append(violations, e.Name()+" imports escapehatch (the pure core must not spawn)")
			}
		}
	}
	if checked == 0 {
		t.Fatal("no non-test .go files found in the driver package")
	}
	if !execRunnerImportsEscape {
		t.Fatalf("stale pin: %s no longer imports escapehatch; update or remove execRunnerFile", execRunnerFile)
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("driver package must stay bounded (no EKA, no os/exec outside the one Runner file); violations:\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// packageDir returns the directory holding the driver package source.
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

// importsPath reports whether the file at path imports importPath.
func importsPath(t *testing.T, path, importPath string) bool {
	t.Helper()
	for _, p := range fileImports(t, path) {
		if p == importPath {
			return true
		}
	}
	return false
}
