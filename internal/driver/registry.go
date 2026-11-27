// Package driver holds the steward's deterministic driver registry.
//
// A *driver* is how the steward launches and talks to one coding-agent CLI.
// Drivers are STEWARD CONFIG: they live in a steward config file, never in
// the EKA store, and no driver reads or writes EKA. The config bytes are
// passed IN to Parse; the parsing/resolution/selection core performs no
// filesystem, network or subprocess I/O.
//
// # Config shape
//
// The document has a single "drivers" list. Every driver is declared as
// {tool, cmd, args}; the wire KIND (acp|cli) is NOT config — it is derived
// deterministically from the tool's registered adapter (see adapters in
// probe.go). Parsing is strict.
//
//	drivers:
//	  - tool: opencode      # must name a known tool (an adapter exists)
//	    cmd: opencode
//	    args: [acp]
//	  - tool: claude
//	    cmd: claude
//
// # Core vs edge
//
// This file (the pure core: Parse, Names, Resolve, Select, Has) imports no
// effectful package. Probing — the one operation that runs a subprocess —
// lives in probe.go behind the Runner seam and is unit-tested with a fake
// Runner (no real spawn). The production, escapehatch-backed Runner is
// isolated in exec_runner.go, the ONLY file in this package that imports
// internal/escapehatch (boundary rule 2). See docs/drivers.md.
package driver

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Kind is how the steward talks to a tool over the wire.
type Kind string

const (
	// KindACP is an Agent Client Protocol agent: the steward speaks
	// JSON-RPC over the child's stdio. Probing does one initialize
	// round-trip. See the ACP spark → internal/acp/probe.
	KindACP Kind = "acp"
	// KindCLI is a plain command-line tool with no ACP entry point;
	// probing runs `--version` and records the string.
	KindCLI Kind = "cli"
)

// ErrUnknownTool is returned (wrapped) when a tool has no registered
// adapter. Test with errors.Is.
var ErrUnknownTool = errors.New("driver: unknown tool")

// Spec is one declared driver: how to launch it and how to talk to it.
//
// Tool is the unique key (roles and selection reference a driver by it).
// Cmd is the executable base name — it is checked against the escape-hatch
// AllowList at run time. Args are passed verbatim to Cmd. Kind is derived
// by Parse from the tool's adapter, never read from config.
//
// Version and Checksum are the OPTIONAL pin: Parse accepts a driver
// without them, but guard.RequirePinned refuses any driver that lacks
// either, and verifies the resolved binary's sha256 against Checksum
// (lowercase hex). Pinning is a containment gate, not a parse rule.
type Spec struct {
	Tool     string
	Cmd      string
	Args     []string
	Kind     Kind
	Version  string
	Checksum string
}

// Capabilities is the recorded result of probing a driver. It is stored on
// the resolved Driver (see Registry.Probe / Registry.SelectProbed).
type Capabilities struct {
	Kind       Kind              // the adapter kind probed (acp|cli)
	Version    string            // agent/tool version string, if reported
	ACPVersion int               // ACP protocolVersion; 0 for a CLI adapter
	Raw        map[string]string // flattened probe detail (deterministic)
}

// Driver is a resolved driver: its launch Spec plus the Capabilities from
// its last probe. Caps is the zero value until the driver is probed.
type Driver struct {
	Spec Spec
	Caps Capabilities
}

// Registry is the validated, immutable driver set parsed from config.
// Drivers are held sorted by Tool; Names returns that deterministic order.
type Registry struct {
	specs []Spec          // sorted by Tool
	index map[string]Spec // Tool -> Spec
}

// config is the on-disk document shape: a single "drivers" list. The
// per-driver shape is deliberately {tool, cmd, args} only — the wire kind
// is derived, so it is not a decodable field.
type config struct {
	Drivers []driverConfig `yaml:"drivers"`
}

type driverConfig struct {
	Tool     string   `yaml:"tool"`
	Cmd      string   `yaml:"cmd"`
	Args     []string `yaml:"args"`
	Version  string   `yaml:"version"`
	Checksum string   `yaml:"checksum"`
}

// Parse decodes and validates a driver config. Parsing is strict:
//
//   - the document is a "drivers" list; unknown fields are rejected;
//   - every driver needs a non-empty tool and a non-empty cmd;
//   - tool names must be unique;
//   - every tool must have a registered adapter (adapters in probe.go).
//
// The returned Registry is sorted by tool, so Names and selection are
// deterministic. An empty driver list is a valid empty registry; malformed
// or empty-byte input is an error.
func Parse(data []byte) (*Registry, error) {
	var cfg config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("driver: parse config: %w", err)
	}

	specs := make([]Spec, len(cfg.Drivers))
	for i, d := range cfg.Drivers {
		specs[i] = Spec{Tool: d.Tool, Cmd: d.Cmd, Args: d.Args, Version: d.Version, Checksum: d.Checksum}
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Tool < specs[j].Tool })

	index := make(map[string]Spec, len(specs))
	for i := range specs {
		s := &specs[i]
		switch {
		case s.Tool == "":
			return nil, errors.New("driver: driver with empty tool")
		case s.Cmd == "":
			return nil, fmt.Errorf("driver: driver %q has empty cmd", s.Tool)
		}
		a, ok := adapters[s.Tool]
		if !ok {
			return nil, fmt.Errorf("%w %q", ErrUnknownTool, s.Tool)
		}
		s.Kind = a.kind
		if _, dup := index[s.Tool]; dup {
			return nil, fmt.Errorf("driver: duplicate tool %q", s.Tool)
		}
		index[s.Tool] = *s
	}
	return &Registry{specs: specs, index: index}, nil
}

// Names returns every registered tool name in deterministic (sorted) order.
func (r *Registry) Names() []string {
	out := make([]string, len(r.specs))
	for i, s := range r.specs {
		out[i] = s.Tool
	}
	return out
}

// Has reports whether tool is a registered driver. It is the seam that
// lets role.ValidateDrivers check role drivers against this registry
// WITHOUT importing it (role.DriverLookup).
func (r *Registry) Has(tool string) bool {
	_, ok := r.index[tool]
	return ok
}

// Resolve returns the Spec for tool. An unknown tool is an error that
// names it (wraps ErrUnknownTool).
func (r *Registry) Resolve(tool string) (Spec, error) {
	s, ok := r.index[tool]
	if !ok {
		return Spec{}, fmt.Errorf("%w %q", ErrUnknownTool, tool)
	}
	return s, nil
}

// Select returns the resolved Driver for tool, or false when tool is
// unknown. It is deterministic: the same tool always yields the same Spec,
// and an unknown tool never errors — the caller branches on the bool.
// Caps is the zero value; use Probe / SelectProbed to record capabilities.
func (r *Registry) Select(tool string) (Driver, bool) {
	s, ok := r.index[tool]
	if !ok {
		return Driver{}, false
	}
	return Driver{Spec: s}, true
}

// String renders a Spec compactly; it is stable and used in messages.
func (s Spec) String() string {
	if len(s.Args) == 0 {
		return fmt.Sprintf("%s (%s: %s)", s.Tool, s.Kind, s.Cmd)
	}
	return fmt.Sprintf("%s (%s: %s %s)", s.Tool, s.Kind, s.Cmd, strings.Join(s.Args, " "))
}
