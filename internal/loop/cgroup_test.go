package loop

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/sizing"
)

// writeCgroup writes a fixture cgroup directory with the given file bodies
// ("" means the file is omitted).
func writeCgroup(t *testing.T, memoryMax, cpuMax string) string {
	t.Helper()
	dir := t.TempDir()
	if memoryMax != "" {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(memoryMax), 0o644); err != nil {
			t.Fatalf("write memory.max: %v", err)
		}
	}
	if cpuMax != "" {
		if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(cpuMax), 0o644); err != nil {
			t.Fatalf("write cpu.max: %v", err)
		}
	}
	return dir
}

// TestCgroupReaderResources asserts the parsed snapshot for numeric, "max" and
// missing variants. Unknown values become 0 (the conservative fallback), never
// host totals.
func TestCgroupReaderResources(t *testing.T) {
	gib := uint64(1 << 30)
	cases := []struct {
		name       string
		memoryMax  string
		cpuMax     string
		wantCPU    float64
		wantMemory uint64
	}{
		{"numeric", "17179869184\n", "800000 100000\n", 8, 16 * gib},
		{"memory-max", "max\n", "800000 100000\n", 8, 0},
		{"cpu-max", "17179869184\n", "max 100000\n", 0, 16 * gib},
		{"both-max", "max\n", "max 100000\n", 0, 0},
		{"missing", "", "", 0, 0},
		{"malformed", "not-a-number\n", "800000\n", 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeCgroup(t, tc.memoryMax, tc.cpuMax)
			res := CgroupReader{Prefix: dir}.Resources()
			if res.CPUCores != tc.wantCPU || res.MemoryBytes != tc.wantMemory {
				t.Fatalf("Resources = %+v, want CPUCores=%v MemoryBytes=%d", res, tc.wantCPU, tc.wantMemory)
			}
		})
	}
}

// TestCgroupReaderSizingClamps asserts the snapshot composes with internal/
// sizing and clamps to the configured maximum; an unknown snapshot yields the
// 1-worker floor.
func TestCgroupReaderSizingClamps(t *testing.T) {
	cases := []struct {
		name        string
		memoryMax   string
		cpuMax      string
		maxWorkers  int
		wantWorkers int
	}{
		// 16 cores, 16 GiB → base min(8, 4) = 4, clamped to 3.
		{"clamp-to-max", "17179869184\n", "1600000 100000\n", 3, 3},
		// 16 cores, 16 GiB → base 4, under a max of 8.
		{"under-max", "17179869184\n", "1600000 100000\n", 8, 4},
		// "max" cpu → 0 CPU → 1-worker floor.
		{"cpu-max-floor", "17179869184\n", "max 100000\n", 8, 1},
		// missing files → 1-worker floor.
		{"missing-floor", "", "", 8, 1},
		// tiny memory (2 GiB) → memory term 0 → 1-worker floor.
		{"small-memory", "2147483648\n", "1600000 100000\n", 8, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeCgroup(t, tc.memoryMax, tc.cpuMax)
			l := New(nil, nil, nil, &Store{Path: filepath.Join(t.TempDir(), "s.json")}, Config{
				MaxWorkers:   tc.maxWorkers,
				CgroupPrefix: dir,
			})
			got, err := l.Workers()
			if err != nil {
				t.Fatalf("Workers: %v", err)
			}
			if got != tc.wantWorkers {
				t.Fatalf("Workers = %d, want %d", got, tc.wantWorkers)
			}
			// Cross-check against the pure sizing formula over the parsed
			// snapshot: the loop must not invent its own bytes.
			res := CgroupReader{Prefix: dir}.Resources()
			want, err := sizing.Workers(res, sizing.Config{MaxWorkers: tc.maxWorkers})
			if err != nil {
				t.Fatalf("sizing.Workers: %v", err)
			}
			if got != want {
				t.Fatalf("Workers = %d, sizing.Workers = %d", got, want)
			}
		})
	}
}

// TestWorkersRejectsInvalidMax asserts sizing's configuration error surfaces.
func TestWorkersRejectsInvalidMax(t *testing.T) {
	dir := writeCgroup(t, "17179869184\n", "800000 100000\n")
	l := New(nil, nil, nil, &Store{Path: filepath.Join(t.TempDir(), "s.json")}, Config{
		MaxWorkers:   0,
		CgroupPrefix: dir,
	})
	if _, err := l.Workers(); err == nil {
		t.Fatal("Workers with MaxWorkers=0 must error")
	}
}

// TestParseCPUMaxFractional asserts a fractional quota yields a fractional core
// count (a cgroup quota need not be a whole core).
func TestParseCPUMaxFractional(t *testing.T) {
	got, ok := parseCPUMax([]byte("50000 100000\n"))
	if !ok || got != 0.5 {
		t.Fatalf("parseCPUMax(50000/100000) = %v, %v; want 0.5, true", got, ok)
	}
}
