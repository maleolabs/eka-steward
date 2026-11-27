package loop

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/maleolabs/eka-steward/v2/internal/sizing"
)

// DefaultCgroupPrefix is the conventional cgroup v2 mount root. Tests override
// it (Config.CgroupPrefix) so they never read the host cgroup.
const DefaultCgroupPrefix = "/sys/fs/cgroup"

// CgroupReader fills a sizing.Resources snapshot from a cgroup v2 hierarchy.
// It reads only <Prefix>/memory.max and <Prefix>/cpu.max — NEVER host totals
// (/proc/meminfo, runtime.NumCPU): the steward must size to its own cgroup
// limit, not the machine it happens to run on.
//
// Fallback is deterministic and conservative: a value that is "max"
// (unlimited), missing, unreadable or malformed becomes 0 for that term. A
// zero term makes internal/sizing clamp to its 1-worker floor, so an unknown
// limit yields exactly 1 worker rather than a guessed (possibly over-large)
// count. See docs/control-loop.md.
type CgroupReader struct {
	// Prefix is the cgroup v2 mount root. Empty means DefaultCgroupPrefix.
	Prefix string
}

// Resources reads memory.max and cpu.max under the reader's prefix and returns
// the sizing snapshot. It never errors: an unknown value is reported as 0 (the
// documented conservative fallback), not as a failure.
func (r CgroupReader) Resources() sizing.Resources {
	prefix := r.Prefix
	if prefix == "" {
		prefix = DefaultCgroupPrefix
	}
	mem, _ := readMemoryMax(filepath.Join(prefix, "memory.max"))
	cpu, _ := readCPUMax(filepath.Join(prefix, "cpu.max"))
	return sizing.Resources{CPUCores: cpu, MemoryBytes: mem}
}

// readMemoryMax reads and parses <path>/memory.max. ok is false when the file
// is absent/unreadable, holds "max", or is not a byte count.
func readMemoryMax(path string) (uint64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	return parseMemoryMax(b)
}

// readCPUMax reads and parses <path>/cpu.max. ok is false when the file is
// absent/unreadable, holds an unlimited quota ("max <period>"), or is
// otherwise not parsesable.
func readCPUMax(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	return parseCPUMax(b)
}

// parseMemoryMax parses a cgroup v2 memory.max body: a byte count or "max"
// (unlimited). "max", a non-numeric body or a zero body yield ok=false.
func parseMemoryMax(b []byte) (uint64, bool) {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "max" {
		return 0, false
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n == 0 {
		return 0, false
	}
	return n, true
}

// parseCPUMax parses a cgroup v2 cpu.max body "<quota> <period>" (both in
// microseconds). A quota of "max" means unlimited. The result is quota/period
// as a fractional core count; a missing/zero period or any malformed field
// yields ok=false.
func parseCPUMax(b []byte) (float64, bool) {
	fields := strings.Fields(string(b))
	if len(fields) != 2 || fields[0] == "max" {
		return 0, false
	}
	quota, err := strconv.ParseUint(fields[0], 10, 64)
	if err != nil {
		return 0, false
	}
	period, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil || period == 0 {
		return 0, false
	}
	return float64(quota) / float64(period), true
}
