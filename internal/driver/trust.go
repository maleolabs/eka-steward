package driver

import (
	"errors"
	"fmt"
	"sort"
)

// Trust is the steward's KNOWN-vs-UNKNOWN classification of a driver.
//
// A driver is KNOWN only when it carries BOTH an exact Version and a
// sha256 Checksum (the pin). Anything else — a missing pin, or a tool with
// no adapter at all — is UNKNOWN and is refused at the acceptance points
// (see RequireKnown / ParseTrusted). Classification here is purely
// structural (it hashes nothing): the checksum MATCH is verified by
// guard.RequirePinned, which is the one place that reads the binary.
type Trust int

const (
	// TrustUnknown is a driver that is not pinned (missing Version or
	// Checksum) or whose tool has no adapter.
	TrustUnknown Trust = iota
	// TrustKnown is a driver carrying an exact Version AND a Checksum.
	TrustKnown
)

// String renders the trust class deterministically ("known" / "unknown").
func (t Trust) String() string {
	if t == TrustKnown {
		return "known"
	}
	return "unknown"
}

// ErrUntrusted is returned (wrapped) when a driver is refused because it is
// not KNOWN — it lacks an exact Version or a Checksum. It is the structural
// counterpart of guard.ErrUnpinned: ErrUntrusted is a pure classification
// failure (no binary read), ErrUnpinned additionally verifies the pin
// against the resolved binary. Test with errors.Is.
var ErrUntrusted = errors.New("driver: driver not pinned")

// Trust classifies s: KNOWN iff it carries both an exact Version and a
// Checksum. It is pure and deterministic (no I/O, no clock).
func (s Spec) Trust() Trust {
	if s.Version != "" && s.Checksum != "" {
		return TrustKnown
	}
	return TrustUnknown
}

// RequireKnown is the registry-build acceptance gate: it refuses the whole
// set when ANY driver is not KNOWN, so an unpinned driver cannot reach the
// loop. It is fail-closed and pure (it reads no binary — that is
// guard.RequirePinned's job); the error names the offending tool.
//
// Iteration is over a sorted copy, so the same (multi-driver) input always
// yields the same first offender and the same message.
func RequireKnown(specs []Spec) error {
	sorted := make([]Spec, len(specs))
	copy(sorted, specs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Tool < sorted[j].Tool })
	for i := range sorted {
		s := sorted[i]
		if s.Tool == "" {
			return fmt.Errorf("%w: a driver has an empty tool", ErrUntrusted)
		}
		if s.Trust() != TrustKnown {
			return fmt.Errorf("%w: tool %q has no pin: an exact version and a checksum are required", ErrUntrusted, s.Tool)
		}
	}
	return nil
}

// ParseTrusted is the fail-closed registry-build entry point: it parses AND
// enforces the KNOWN rule (RequireKnown), so a registry obtained from it
// cannot contain an unpinned driver. Parse (the permissive parse) is
// unchanged; ParseTrusted is the acceptance step a caller uses before the
// loop. An unknown tool still fails with ErrUnknownTool (from Parse).
func ParseTrusted(data []byte) (*Registry, error) {
	reg, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := RequireKnown(reg.Specs()); err != nil {
		return nil, err
	}
	return reg, nil
}

// RequireKnown is the per-registry acceptance gate: it refuses a registry
// that contains any UNKNOWN (unpinned) driver. Same rule and ordering as the
// free RequireKnown.
func (r *Registry) RequireKnown() error {
	return RequireKnown(r.Specs())
}
