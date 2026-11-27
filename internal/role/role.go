// Package role parses and validates steward worker-role config.
//
// A "role" is one configured worker role: it resolves deterministically
// to a registered driver name plus a prompt template. Roles are STEWARD
// CONFIG — they live in a steward config file, never in the EKA store,
// and no role reads or writes EKA. The config bytes are passed IN to
// Parse; this package performs no filesystem, network or subprocess I/O.
//
// This package is parsing + resolution + driver-name validation ONLY. It
// holds no role executor, no prompt rendering and no driver
// implementation — those are separate work items.
package role

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Role is one configured worker role.
type Role struct {
	Name     string `yaml:"name"`     // unique
	Driver   string `yaml:"driver"`   // must reference a registered driver by name
	Template string `yaml:"template"` // prompt template path or inline id
	Review   bool   `yaml:"review"`   // true => this is a review role
}

// Set is the validated, immutable role set from config. Roles are held
// sorted by Name; Names and ReviewRoles return that deterministic order.
type Set struct {
	roles []Role          // sorted by Name
	index map[string]Role // Name -> Role
}

// config is the on-disk document shape: a single "roles" list.
type config struct {
	Roles []Role `yaml:"roles"`
}

// Parse decodes and validates a role config. Parsing is strict:
//
//   - the document is a "roles" list; unknown fields are rejected;
//   - every role needs a non-empty name, driver and template;
//   - role names must be unique.
//
// The returned Set is sorted by role name. An empty config (no roles) is
// a valid empty set; a malformed or empty-byte document is an error.
func Parse(data []byte) (*Set, error) {
	var cfg config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("role: parse config: %w", err)
	}

	roles := make([]Role, len(cfg.Roles))
	copy(roles, cfg.Roles)
	sort.Slice(roles, func(i, j int) bool { return roles[i].Name < roles[j].Name })

	index := make(map[string]Role, len(roles))
	for _, r := range roles {
		switch {
		case r.Name == "":
			return nil, errors.New("role: role with empty name")
		case r.Driver == "":
			return nil, fmt.Errorf("role: role %q has empty driver", r.Name)
		case r.Template == "":
			return nil, fmt.Errorf("role: role %q has empty template", r.Name)
		}
		if _, dup := index[r.Name]; dup {
			return nil, fmt.Errorf("role: duplicate role name %q", r.Name)
		}
		index[r.Name] = r
	}
	return &Set{roles: roles, index: index}, nil
}

// Resolve returns the role named name. An unknown name is an error that
// names it.
func (s *Set) Resolve(name string) (Role, error) {
	r, ok := s.index[name]
	if !ok {
		return Role{}, fmt.Errorf("role: unknown role %q", name)
	}
	return r, nil
}

// Names returns every role name in deterministic (sorted) order.
func (s *Set) Names() []string {
	out := make([]string, len(s.roles))
	for i, r := range s.roles {
		out[i] = r.Name
	}
	return out
}

// ReviewRoles returns the roles with Review set, in deterministic order.
func (s *Set) ReviewRoles() []Role {
	var out []Role
	for _, r := range s.roles {
		if r.Review {
			out = append(out, r)
		}
	}
	return out
}

// DriverLookup is the seam that lets ValidateDrivers check role drivers
// against the driver registry WITHOUT importing it.
type DriverLookup interface {
	Has(name string) bool
}

// ValidateDrivers reports an error naming every role whose Driver is not
// known to d. Unknown drivers are reported in deterministic order.
func (s *Set) ValidateDrivers(d DriverLookup) error {
	var unknown []string
	for _, r := range s.roles {
		if !d.Has(r.Driver) {
			unknown = append(unknown, fmt.Sprintf("role %q references unknown driver %q", r.Name, r.Driver))
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("role: unknown driver(s): %s", strings.Join(unknown, "; "))
	}
	return nil
}
