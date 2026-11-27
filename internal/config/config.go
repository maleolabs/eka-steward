// Package config parses and validates the steward's single combined
// configuration document: the roles, drivers, sizing, sandbox and
// control-loop sections under one versioned schema.
//
// It COMPOSES the per-section strict parsers (internal/role, internal/driver)
// and the sizing rule (internal/sizing) rather than re-implementing them, so
// a value accepted here is accepted by the standalone parser and vice versa.
// The document is decoded STRICTLY (yaml KnownFields): an unknown field
// anywhere — top-level, inside a role, a driver, sizing, sandbox or loop — is
// refused.
//
// The document carries a schema `version`. Parse gates on it: a known older
// version is migrated to the current one through the documented path
// (Migrate), and an unknown or future version is refused deterministically —
// never silently accepted. See docs/config.md.
//
// Config is STEWARD CONFIG. This package performs no filesystem, network or
// subprocess I/O; the config bytes are passed IN to Parse.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/maleolabs/eka-steward/v2/internal/driver"
	"github.com/maleolabs/eka-steward/v2/internal/role"
	"github.com/maleolabs/eka-steward/v2/internal/sizing"
	"gopkg.in/yaml.v3"
)

// Version is the current config schema version. A document that declares it
// is validated as-is; a known older version is migrated to it (see Migrate);
// any other version is refused.
const Version = 1

// oldestVersion is the oldest schema version Parse still migrates from.
// Version 0 is the pre-schema document: roles + drivers only.
const oldestVersion = 0

// Config is the decoded, validated, current-version steward config. Every
// section is validated: roles and drivers by their own strict parsers, sizing
// by internal/sizing, and the sandbox is contained by construction (there is
// no opt-out field). Roles and Drivers are held in canonical (sorted) order
// and each driver carries its adapter-derived Kind, so the returned value is
// the normalized, validated form.
type Config struct {
	Version int           `yaml:"version"`
	Roles   []role.Role   `yaml:"roles"`
	Drivers []driver.Spec `yaml:"drivers"`
	Sizing  SizingConfig  `yaml:"sizing"`
	Sandbox SandboxConfig `yaml:"sandbox"`
	Loop    LoopConfig    `yaml:"loop"`
}

// SizingConfig is the operator-owned worker sizing policy (docs/sizing.md).
// MaxWorkers is the hard upper bound and must be >= 1; CostCeiling optionally
// caps the count further (0 means "no ceiling").
type SizingConfig struct {
	MaxWorkers  int `yaml:"maxWorkers"`
	CostCeiling int `yaml:"costCeiling"`
}

// Sizing projects the section onto internal/sizing.Config so a caller wires
// the loop with exactly the rule this package validated.
func (s SizingConfig) Sizing() sizing.Config {
	return sizing.Config{MaxWorkers: s.MaxWorkers, CostCeiling: s.CostCeiling}
}

// validate reuses internal/sizing's rule (MaxWorkers >= 1) so config and the
// sizing package can never disagree, and names the offending field.
func (s SizingConfig) validate() error {
	if _, err := sizing.Workers(sizing.Resources{}, s.Sizing()); err != nil {
		return fmt.Errorf("config: sizing.maxWorkers: %w", err)
	}
	return nil
}

// SandboxConfig is the sandbox section. Containment is ON BY DEFAULT and has
// NO opt-out: there is deliberately no disable/bypass field, and the strict
// parser refuses one (an unknown field). Contained always reports true, which
// mirrors guard.Config (docs/guardrails.md).
type SandboxConfig struct {
	// WorktreeBase is the parent directory for per-worker worktrees. Empty
	// means the caller supplies one.
	WorktreeBase string `yaml:"worktreeBase"`
}

// Contained reports whether containment is in force. It is always true: the
// zero value is contained and the schema has no opt-out.
func (s SandboxConfig) Contained() bool { return true }

// validate requires a configured worktree base to be absolute, so a relative
// base cannot silently resolve against an unexpected working directory.
func (s SandboxConfig) validate() error {
	if s.WorktreeBase != "" && !filepath.IsAbs(s.WorktreeBase) {
		return fmt.Errorf("config: sandbox.worktreeBase must be an absolute path, got %q", s.WorktreeBase)
	}
	return nil
}

// LoopConfig is the operator-owned slice of the control-loop config
// (docs/control-loop.md): bounded retry, the queue backpressure bound and the
// per-attempt timeout. Its zero value is valid — retry is clamped to one
// attempt, no queue bound, no per-attempt deadline.
type LoopConfig struct {
	MaxAttempts    int      `yaml:"maxAttempts"`
	QueueLimit     int      `yaml:"queueLimit"`
	AttemptTimeout Duration `yaml:"attemptTimeout"`
}

// validate refuses a negative value in any loop field; each error names the
// offending field.
func (l LoopConfig) validate() error {
	switch {
	case l.MaxAttempts < 0:
		return fmt.Errorf("config: loop.maxAttempts must be >= 0, got %d", l.MaxAttempts)
	case l.QueueLimit < 0:
		return fmt.Errorf("config: loop.queueLimit must be >= 0, got %d", l.QueueLimit)
	case l.AttemptTimeout < 0:
		return fmt.Errorf("config: loop.attemptTimeout must be >= 0, got %s", l.AttemptTimeout)
	}
	return nil
}

// Duration is a config duration: a Go duration string ("30s", "1m30s") or an
// integer count of nanoseconds. The string form is the documented one.
type Duration time.Duration

// String renders the duration in Go duration-string form.
func (d Duration) String() string { return time.Duration(d).String() }

// UnmarshalYAML accepts a Go duration string or an integer nanosecond count.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.Tag == "!!int" {
		n, err := strconv.ParseInt(node.Value, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", node.Value, err)
		}
		*d = Duration(time.Duration(n))
		return nil
	}
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	*d = Duration(v)
	return nil
}

// Parse decodes and fully validates a config document. Parsing is strict —
// every unknown field is refused — the `version` is gated and, when older,
// migrated to the current schema (Migrate), and every section is validated
// (roles/drivers via their own strict parsers, sizing via internal/sizing).
// Errors name the offending field.
//
// Parse is deterministic: the same bytes always yield the same result, and no
// map is iterated to produce it.
//
// A document with no `version` key decodes as version 0 (the documented
// default) and is migrated; a current-version document must carry an explicit
// `version: 1`.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config: parse: %w", err)
	}
	if err := c.Migrate(c.Version); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Migrate upgrades c in place from the schema version `from` to the current
// Version, applying the documented migrations in order. It is deterministic
// and, for an already-current document, a no-op. An unknown or future `from`
// is a deterministic error — an unrecognised version is never silently
// accepted.
func (c *Config) Migrate(from int) error {
	switch {
	case from < oldestVersion:
		return fmt.Errorf("config: version %d is not supported (oldest supported is %d, current is %d)", from, oldestVersion, Version)
	case from > Version:
		return fmt.Errorf("config: version %d is newer than this steward supports (current is %d)", from, Version)
	}
	for v := from; v < Version; v++ {
		switch v {
		case 0:
			migrateV0toV1(c)
		}
	}
	c.Version = Version
	return nil
}

// migrateV0toV1 fills the defaults for the sections a version-0 document
// could not carry. Version 0 was roles + drivers only: it had no `sizing`
// (added in v1), while `sandbox` and `loop` (also added in v1) already have
// the correct zero value — contained, no-op. The sizing default is the
// conservative sizing floor of 1 worker, never a guessed higher count.
func migrateV0toV1(c *Config) {
	if c.Sizing.MaxWorkers == 0 {
		c.Sizing.MaxWorkers = 1
	}
}

// validate checks every section and their cross-field rules. It delegates the
// per-record rules to the standalone parsers so config and standalone
// behaviour agree, then applies the config-only rules: at least one role, and
// every role's driver resolves to a declared driver.
func (c *Config) validate() error {
	if err := c.Sizing.validate(); err != nil {
		return err
	}
	if err := c.Sandbox.validate(); err != nil {
		return err
	}
	if err := c.Loop.validate(); err != nil {
		return err
	}

	roles, err := role.ParseRoles(c.Roles)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	c.Roles = roles.Roles()
	drivers, err := driver.ParseDrivers(c.Drivers)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	c.Drivers = drivers.Specs()

	if len(c.Roles) == 0 {
		return errors.New("config: roles: at least one role is required")
	}
	if err := roles.ValidateDrivers(drivers); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}
