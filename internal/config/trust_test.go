package config

import (
	"errors"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/driver"
)

// pinnedV1 is validV1 with the claude driver carrying a full pin.
const pinnedV1 = `version: 1
roles:
  - name: impl
    driver: claude
    template: prompts/impl.md
drivers:
  - tool: claude
    cmd: claude
    version: 1.0.0
    checksum: aa
sizing:
  maxWorkers: 1
`

// TestParseTrustedIsFailClosed is AC1 at the config-load acceptance point: an
// unpinned driver is refused by ParseTrusted (wrapping driver.ErrUntrusted and
// naming the tool), while Parse still accepts the same document.
func TestParseTrustedIsFailClosed(t *testing.T) {
	if _, err := ParseTrusted([]byte(pinnedV1)); err != nil {
		t.Fatalf("ParseTrusted refused a pinned config: %v", err)
	}
	if _, err := Parse([]byte(validV1)); err != nil {
		t.Fatalf("Parse must still accept an unpinned config: %v", err)
	}
	_, err := ParseTrusted([]byte(validV1))
	if !errors.Is(err, driver.ErrUntrusted) {
		t.Fatalf("ParseTrusted(unpinned) = %v, want driver.ErrUntrusted", err)
	}
	if !strings.Contains(err.Error(), `"claude"`) {
		t.Fatalf("error %q must name the unpinned tool", err)
	}
}

// TestRequireKnownDriversNamesTheTool pins the config method directly.
func TestRequireKnownDriversNamesTheTool(t *testing.T) {
	c, err := Parse([]byte(validV1))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := c.RequireKnownDrivers(); !errors.Is(err, driver.ErrUntrusted) || !strings.Contains(err.Error(), `"claude"`) {
		t.Fatalf("RequireKnownDrivers() = %v, want ErrUntrusted naming claude", err)
	}
}

// TestParseTrustedKeepsUnknownToolRefusal pins that the fail-closed entry
// point does not mask the unknown-tool failure.
func TestParseTrustedKeepsUnknownToolRefusal(t *testing.T) {
	doc := strings.Replace(pinnedV1, "tool: claude", "tool: ghost", 1)
	_, err := ParseTrusted([]byte(doc))
	if !errors.Is(err, driver.ErrUnknownTool) {
		t.Fatalf("ParseTrusted(unknown tool) = %v, want driver.ErrUnknownTool", err)
	}
}
