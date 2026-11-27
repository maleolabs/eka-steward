package cmd

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The README restates two values that already have a single source in the
// repository: the steward build version (anvil.yaml project.version — the tag
// the release workflow builds and checks against) and the pinned eka-core
// version (go.mod). Both restatements are useful to a reader and dangerous to
// leave unchecked: the README was the only place restating the build version
// with nothing verifying it, so it drifts on a bump that forgets it. These
// tests pin the README's module-path line to those sources; scripts/bump.sh
// syncs the steward axis automatically.

var readmeModuleLine = regexp.MustCompile("Steward `([^`]+)` · `eka-core v([^`]+)`")

// readmeVersionClaims extracts (steward, ekaCore) from the README's
// module-path line: "... · Steward `X.Y.Z` · `eka-core vX.Y.Z`".
func readmeVersionClaims(t *testing.T) (steward, ekaCore string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "README.md"))
	if err != nil {
		t.Fatalf("README.md is unreadable: %v", err)
	}
	m := readmeModuleLine.FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatal("README.md no longer states its module-path line as " +
			"\"Steward `X.Y.Z` · `eka-core vX.Y.Z`\"; restore the shape so the claim can be verified")
	}
	return m[1], m[2]
}

// TestReadmeStatesProjectVersion fails when the README's steward version
// disagrees with the version anvil.yaml releases.
func TestReadmeStatesProjectVersion(t *testing.T) {
	steward, _ := readmeVersionClaims(t)
	if released := anvilProjectVersion(t); steward != released {
		t.Errorf("README states Steward %s, anvil.yaml releases %s", steward, released)
	}
}

// TestReadmeStatesRequiredEkaCoreVersion fails when the README's eka-core
// version disagrees with the version go.mod requires.
func TestReadmeStatesRequiredEkaCoreVersion(t *testing.T) {
	_, ekaCore := readmeVersionClaims(t)
	if want := requiredModuleVersion(t, "github.com/maleolabs/eka-core/v2"); ekaCore != want {
		t.Errorf("README states eka-core v%s, go.mod requires v%s", ekaCore, want)
	}
}

// anvilProjectVersion returns anvil.yaml's project.version.
func anvilProjectVersion(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "anvil.yaml"))
	if err != nil {
		t.Fatalf("anvil.yaml is unreadable: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.SplitN(strings.TrimSpace(line), ":", 2)
		if len(fields) == 2 && fields[0] == "version" {
			return strings.TrimSpace(fields[1])
		}
	}
	t.Fatal("anvil.yaml states no project version")
	return ""
}

// requiredModuleVersion returns the version go.mod requires for module path.
func requiredModuleVersion(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "go.mod"))
	if err != nil {
		t.Fatalf("go.mod is unreadable: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		// A require line reads "<path> v<version>".
		if len(fields) == 2 && fields[0] == path {
			return strings.TrimPrefix(fields[1], "v")
		}
	}
	t.Fatalf("go.mod does not require %s", path)
	return ""
}
