package sizing

import (
	"errors"
	"testing"
)

// machine builds a Resources snapshot in GiB for terse table rows.
func machine(cores, memGiB float64) Resources {
	return Resources{CPUCores: cores, MemoryBytes: uint64(memGiB * float64(GiB))}
}

func TestWorkers(t *testing.T) {
	tests := []struct {
		name    string
		res     Resources
		cfg     Config
		want    int
		wantErr bool
	}{
		{
			name: "one core clamps to one",
			res:  machine(1, 8),
			cfg:  Config{MaxWorkers: 8},
			want: 1,
		},
		{
			name: "64 cores but 2GiB RAM clamps to one",
			res:  machine(64, 2),
			cfg:  Config{MaxWorkers: 64},
			want: 1,
		},
		{
			name: "high MaxWorkers on small machine still one",
			res:  machine(2, 4),
			cfg:  Config{MaxWorkers: 128},
			want: 1,
		},
		{
			name: "balanced machine uses the min term",
			res:  machine(8, 16),
			cfg:  Config{MaxWorkers: 16},
			want: 4,
		},
		{
			name: "cpu term limited when cores are scarce",
			res:  machine(6, 64),
			cfg:  Config{MaxWorkers: 32},
			want: 3,
		},
		{
			name: "memory term limited when RAM is scarce",
			res:  machine(64, 24),
			cfg:  Config{MaxWorkers: 64},
			want: 6,
		},
		{
			name: "MaxWorkers caps a large machine",
			res:  machine(64, 256),
			cfg:  Config{MaxWorkers: 5},
			want: 5,
		},
		{
			name: "MaxWorkers of one forces one",
			res:  machine(64, 256),
			cfg:  Config{MaxWorkers: 1},
			want: 1,
		},
		{
			name: "cost ceiling below the result reduces it",
			res:  machine(16, 64),
			cfg:  Config{MaxWorkers: 16, CostCeiling: 3},
			want: 3,
		},
		{
			name: "cost ceiling above the result never raises it",
			res:  machine(16, 64),
			cfg:  Config{MaxWorkers: 16, CostCeiling: 20},
			want: 8,
		},
		{
			name: "cost ceiling of one yields one",
			res:  machine(16, 64),
			cfg:  Config{MaxWorkers: 16, CostCeiling: 1},
			want: 1,
		},
		{
			name: "cost ceiling zero means no ceiling",
			res:  machine(16, 64),
			cfg:  Config{MaxWorkers: 16, CostCeiling: 0},
			want: 8,
		},
		{
			name: "cost ceiling cannot force zero on a tiny machine",
			res:  machine(1, 1),
			cfg:  Config{MaxWorkers: 8, CostCeiling: 0},
			want: 1,
		},
		{
			name: "fractional cores floor the cpu term",
			res:  Resources{CPUCores: 4.5, MemoryBytes: 64 * GiB},
			cfg:  Config{MaxWorkers: 16},
			want: 2,
		},
		{
			name: "zero resources clamp to one",
			res:  Resources{CPUCores: 0, MemoryBytes: 0},
			cfg:  Config{MaxWorkers: 8},
			want: 1,
		},
		{
			name: "negative cores clamp to one",
			res:  Resources{CPUCores: -4, MemoryBytes: 64 * GiB},
			cfg:  Config{MaxWorkers: 8},
			want: 1,
		},
		{
			name: "zero memory clamps to one",
			res:  Resources{CPUCores: 64, MemoryBytes: 0},
			cfg:  Config{MaxWorkers: 8},
			want: 1,
		},
		{
			name:    "MaxWorkers zero is a config error",
			res:     machine(8, 16),
			cfg:     Config{MaxWorkers: 0},
			wantErr: true,
		},
		{
			name:    "negative MaxWorkers is a config error",
			res:     machine(8, 16),
			cfg:     Config{MaxWorkers: -3},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Workers(tc.res, tc.cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Workers(%+v, %+v) = %d, want error", tc.res, tc.cfg, got)
				}
				if !errors.Is(err, errMaxWorkers) {
					t.Fatalf("Workers error = %v, want errMaxWorkers", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Workers(%+v, %+v) unexpected error: %v", tc.res, tc.cfg, err)
			}
			if got != tc.want {
				t.Fatalf("Workers(%+v, %+v) = %d, want %d", tc.res, tc.cfg, got, tc.want)
			}
		})
	}
}

// TestWorkersIsPure asserts the same input yields the same output across
// repeated calls — the observable guarantee of a deterministic, LLM-free
// function.
func TestWorkersIsPure(t *testing.T) {
	res := machine(12, 32)
	cfg := Config{MaxWorkers: 8, CostCeiling: 5}
	first, err := Workers(res, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 0; i < 100; i++ {
		got, err := Workers(res, cfg)
		if err != nil || got != first {
			t.Fatalf("call %d = (%d, %v), want (%d, nil)", i, got, err, first)
		}
	}
}
