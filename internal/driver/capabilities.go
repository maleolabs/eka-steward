package driver

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// CapabilitiesSchema is the schema version of the capabilities.json payload.
const CapabilitiesSchema = 1

// CapabilityRecord is the recorded, deterministic capability record for one
// driver. It carries BOTH the pin (PinnedVersion + Checksum, the trust
// fields) and the PROBED result (Version + Protocol + Capabilities), so a
// single artifact is the audit of "what we trust" plus "what it reported".
type CapabilityRecord struct {
	// Tool is the driver key (unique; records are sorted by it).
	Tool string `json:"tool"`
	// Version is the PROBED agent/tool version, if reported.
	Version string `json:"version"`
	// PinnedVersion is the required pin (Spec.Version) — the trusted version.
	PinnedVersion string `json:"pinnedVersion"`
	// Checksum is the pinned sha256 (Spec.Checksum) of the resolved binary.
	Checksum string `json:"checksum"`
	// Protocol is "acp/vN" for an ACP adapter or "cli" for a CLI adapter.
	Protocol string `json:"protocol"`
	// Capabilities is the flattened probe detail as sorted "key=value"
	// entries (no map in the payload, so serialization is byte-stable).
	Capabilities []string `json:"capabilities"`
	// ProbedAt is a caller-injected timestamp (RFC3339 text or empty). It is
	// NEVER read from the wall clock here, so the payload stays a pure
	// function of its inputs (an empty value is omitted).
	ProbedAt string `json:"probedAt,omitempty"`
}

// CapabilitiesFile is the fixed capabilities.json document: a schema tag and
// the driver records.
type CapabilitiesFile struct {
	Schema  int                `json:"schema"`
	Drivers []CapabilityRecord `json:"drivers"`
}

// BuildCapabilities assembles the capabilities document from a registry and
// the probe results keyed by tool. Every declared driver must have a probe
// result (a missing one is an error naming the tool) — the artifact records
// the whole trusted set, never a partial one. Records and capability entries
// are sorted, and probedAt is passed in (never read from a clock), so the
// same inputs always yield the same struct.
func BuildCapabilities(reg *Registry, probed map[string]Capabilities, probedAt string) (CapabilitiesFile, error) {
	names := reg.Names() // sorted
	recs := make([]CapabilityRecord, 0, len(names))
	for _, tool := range names {
		spec, err := reg.Resolve(tool)
		if err != nil {
			return CapabilitiesFile{}, err // Names comes from reg; cannot happen
		}
		caps, ok := probed[tool]
		if !ok {
			return CapabilitiesFile{}, fmt.Errorf("driver: capabilities for tool %q were not probed", tool)
		}
		recs = append(recs, CapabilityRecord{
			Tool:          tool,
			Version:       caps.Version,
			PinnedVersion: spec.Version,
			Checksum:      spec.Checksum,
			Protocol:      protocolOf(caps),
			Capabilities:  sortedEntries(caps.Raw),
			ProbedAt:      probedAt,
		})
	}
	return CapabilitiesFile{Schema: CapabilitiesSchema, Drivers: recs}, nil
}

// protocolOf renders the wire protocol deterministically: "acp/vN" for an
// ACP adapter, "cli" for a CLI adapter.
func protocolOf(caps Capabilities) string {
	if caps.Kind == KindACP {
		return fmt.Sprintf("acp/v%d", caps.ACPVersion)
	}
	return string(caps.Kind)
}

// sortedEntries flattens a probe detail map into sorted "key=value" entries,
// so no map iteration order can reach the serialized payload.
func sortedEntries(raw map[string]string) []string {
	out := make([]string, 0, len(raw))
	for k, v := range raw {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// WriteCapabilities serializes f deterministically as indented JSON (struct
// field order, slices already sorted — no map). It performs NO file I/O (the
// driver package stays pure): the caller writes the returned bytes. It
// validates f first, so an invalid document is never written.
func WriteCapabilities(f CapabilitiesFile) ([]byte, error) {
	if err := ValidateCapabilities(f); err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("driver: marshal capabilities: %w", err)
	}
	return append(b, '\n'), nil
}

// ReadCapabilities parses and validates a capabilities.json document. It
// performs NO file I/O. A schema mismatch or a malformed record is a
// deterministic error.
func ReadCapabilities(data []byte) (CapabilitiesFile, error) {
	var f CapabilitiesFile
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return CapabilitiesFile{}, fmt.Errorf("driver: parse capabilities: %w", err)
	}
	if err := ValidateCapabilities(f); err != nil {
		return CapabilitiesFile{}, err
	}
	return f, nil
}

// ValidateCapabilities checks the fixed schema: the schema tag must match,
// records must be unique and non-empty, protocol must be "cli" or "acp/vN",
// and every entry must be a "key=value" pair. Errors are deterministic and
// name the offending tool.
func ValidateCapabilities(f CapabilitiesFile) error {
	if f.Schema != CapabilitiesSchema {
		return fmt.Errorf("driver: capabilities schema %d is not supported (want %d)", f.Schema, CapabilitiesSchema)
	}
	seen := make(map[string]bool, len(f.Drivers))
	for _, r := range f.Drivers {
		switch {
		case r.Tool == "":
			return fmt.Errorf("driver: capability record with empty tool")
		case seen[r.Tool]:
			return fmt.Errorf("driver: duplicate capability record for tool %q", r.Tool)
		case r.Protocol != "cli" && !strings.HasPrefix(r.Protocol, "acp/v"):
			return fmt.Errorf("driver: capability record for tool %q has invalid protocol %q", r.Tool, r.Protocol)
		}
		seen[r.Tool] = true
		for _, e := range r.Capabilities {
			if !strings.Contains(e, "=") {
				return fmt.Errorf("driver: capability record for tool %q has a malformed entry %q", r.Tool, e)
			}
		}
	}
	return nil
}
