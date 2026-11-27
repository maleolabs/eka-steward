package config

import (
	"fmt"

	"github.com/maleolabs/eka-steward/v2/internal/driver"
)

// RequireKnownDrivers is the config-load acceptance gate: it refuses the
// whole config when any declared driver is not KNOWN (it lacks an exact
// version or a checksum), so an unpinned driver cannot reach the loop. It is
// pure (no filesystem I/O) and reuses driver.RequireKnown; the error wraps
// driver.ErrUntrusted and names the offending tool.
//
// Parse (the permissive parse) is unchanged on purpose — pinning is a
// containment gate, not a parse rule — so a caller that needs the
// fail-closed acceptance point uses ParseTrusted or calls this directly.
func (c *Config) RequireKnownDrivers() error {
	if err := driver.RequireKnown(c.Drivers); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}

// ParseTrusted is the fail-closed config-load entry point: it parses AND
// enforces the KNOWN driver rule, so a config obtained from it cannot carry
// an unpinned driver into the loop. Errors from Parse (unknown field, unknown
// tool, version gate) are returned unchanged; an unpinned driver fails with
// driver.ErrUntrusted naming the tool.
func ParseTrusted(data []byte) (*Config, error) {
	c, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := c.RequireKnownDrivers(); err != nil {
		return nil, err
	}
	return c, nil
}
