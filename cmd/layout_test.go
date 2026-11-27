package cmd

import (
	"os"
	"sort"
	"strings"
	"testing"
)

// expectedRootEntries is the PINNED top-level layout of the module. The
// test fails on any drift: a new top-level file or directory must be
// added here deliberately (and justified), and a removed one must be
// dropped. This is the hardcoded layout manifest — repurposing
// eka-cli's top-level layout and dropping the CLI-only entries
// (bootstrap/, templates/, EKA, standardembed*.go, hooksembed.go,
// eka.exe, testdata/, exchange/, feedback/, .eka/) that do not apply to
// a standalone orchestrator.
var expectedRootEntries = []string{
	".anvil",
	".gitattributes",
	".github",
	".gitignore",
	"CONTRIBUTING.md",
	"LICENSE",
	"README.md",
	"RELEASE_NOTES.md",
	"anvil.yaml",
	"cmd",
	"docs",
	"go.mod",
	"go.sum",
	"install.ps1",
	"install.sh",
	"internal",
	"scripts",
}

// ignoredRootEntries are build/VCS artifacts that can appear at the
// module root at test time but are not part of the pinned source layout
// (all are git-ignored). Anything else is drift and fails the test.
var ignoredRootEntries = map[string]bool{
	".git":            true,
	".idea":           true,
	"dist":            true,
	"eka-steward":     true,
	"eka-steward.exe": true,
}

// TestRootLayoutIsPinned asserts the EXACT set of top-level entries of
// the module root. Any addition or removal is a deliberate layout change
// that must update expectedRootEntries.
func TestRootLayoutIsPinned(t *testing.T) {
	root := moduleRoot(t)
	des, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("read module root: %v", err)
	}

	var got []string
	for _, de := range des {
		if ignoredRootEntries[de.Name()] {
			continue
		}
		got = append(got, de.Name())
	}
	sort.Strings(got)

	want := append([]string(nil), expectedRootEntries...)
	sort.Strings(want)

	if strings.Join(got, "\n") == strings.Join(want, "\n") {
		return
	}

	gotSet := map[string]bool{}
	for _, n := range got {
		gotSet[n] = true
	}
	wantSet := map[string]bool{}
	for _, n := range want {
		wantSet[n] = true
	}
	var added, removed []string
	for _, n := range got {
		if !wantSet[n] {
			added = append(added, n)
		}
	}
	for _, n := range want {
		if !gotSet[n] {
			removed = append(removed, n)
		}
	}
	t.Fatalf("module root layout drifted from the pinned manifest:\n  unexpected (found, not expected): %s\n  missing (expected, not found):     %s\n\nUpdate expectedRootEntries in cmd/layout_test.go deliberately.",
		strings.Join(added, ", "), strings.Join(removed, ", "))
}
