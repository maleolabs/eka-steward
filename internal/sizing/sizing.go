// Package sizing holds the steward's pure, deterministic worker-count
// sizing rule: given a snapshot of machine Resources and a Config it
// returns how many workers may run at once.
//
// It is deliberately inert and stdlib-only. Sizing is a pure function of
// its input — the same Resources and Config in, the same count out — with
// no clock, no cgroup reads, no environment and NO LLM input. Filling
// Resources (reading cgroup v2 memory.max / cpu.max) is a separate,
// later control-loop work item: this package only consumes the struct.
// See docs/sizing.md.
package sizing

import "errors"

// GiB is one gibibyte, 1<<30. It is the unit of the memory term in the
// sizing formula.
const GiB = 1 << 30

// bytesPerWorker is the memory budget one worker is allowed: 4 GiB
// (4*GiB). The formula divides total memory by it.
const bytesPerWorker = 4 * GiB

// Resources is the machine snapshot sizing reads. It is filled by the
// caller (the control loop, from cgroup v2 or an equivalent source); this
// package never reads it itself.
type Resources struct {
	// CPUCores is the number of logical CPUs available to the steward.
	// Fractional values are permitted (a cgroup CPU quota may be
	// fractional).
	CPUCores float64
	// MemoryBytes is the memory limit available to the steward, in bytes.
	MemoryBytes uint64
}

// Config is the operator-owned sizing policy.
type Config struct {
	// MaxWorkers is the hard upper bound on concurrency and must be >= 1.
	// A value <= 0 is a configuration error (there is no sensible default
	// to guess, so sizing refuses rather than inventing one).
	MaxWorkers int
	// CostCeiling optionally caps the result further. 0 means "no
	// ceiling"; a value > 0 can only lower the count, never raise it.
	CostCeiling int
}

// errMaxWorkers is the deterministic error returned for an invalid
// MaxWorkers. A fixed message keeps the result byte-identical across
// runs.
var errMaxWorkers = errors.New("sizing: MaxWorkers must be >= 1")

// Workers returns the deterministic worker count for the given resources
// and config:
//
//	base = min(floor(CPUCores/2), floor(MemoryBytes/(4*GiB)))
//	n    = clamp(base, 1, MaxWorkers)
//	if CostCeiling > 0 && CostCeiling < n { n = CostCeiling }
//	n    = max(n, 1)
//
// Rules and edge behaviour:
//   - A machine too small for the formula (CPUCores <= 0 or zero memory)
//     clamps to 1 worker. This is never an error: a degraded but working
//     count is safer than refusing to run, and the minimum is 1 by
//     definition (the steward itself must run).
//   - A CostCeiling of 0 means "no ceiling", so it can never force the
//     count to 0; a ceiling of 1 yields 1.
//   - MaxWorkers <= 0 is a configuration error.
//
// Workers reads no clock, cgroup, environment or LLM — it is a pure
// function of its arguments.
func Workers(res Resources, cfg Config) (int, error) {
	if cfg.MaxWorkers <= 0 {
		return 0, errMaxWorkers
	}

	// Start from the hard upper bound and only ever lower it. Working in
	// the [1, MaxWorkers] window first keeps the uint64 base safe to
	// convert: a huge base cannot overflow int because it is compared
	// against MaxWorkers before the conversion.
	n := cfg.MaxWorkers
	if b := baseWorkers(res); b < uint64(n) {
		n = int(b)
	}

	// CostCeiling may only reduce; 0 means no ceiling.
	if cfg.CostCeiling > 0 && cfg.CostCeiling < n {
		n = cfg.CostCeiling
	}

	// Nothing may return fewer than one worker.
	if n < 1 {
		n = 1
	}
	return n, nil
}

// baseWorkers computes the unclamped base count as a uint64. A machine at
// or below the floor for either term yields 0, which Workers clamps up
// to 1.
func baseWorkers(res Resources) uint64 {
	if res.CPUCores <= 0 || res.MemoryBytes == 0 {
		return 0
	}
	// uint64(float)/truncation floors for the non-negative values admitted
	// above. ponytail: assumes < 2^64 cores and < 2^64 bytes (real
	// machines); revisit only if a hostile Resources source appears.
	byCPU := uint64(res.CPUCores / 2)
	byMem := res.MemoryBytes / bytesPerWorker
	if byMem < byCPU {
		return byMem
	}
	return byCPU
}
