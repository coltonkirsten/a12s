package gce

import (
	"testing"

	"github.com/coltonkirsten/a12s/internal/driver/gce/gcefake"
)

func TestPickShape(t *testing.T) {
	const gi = 1 << 30
	tests := []struct {
		fam       string
		cpu, mem  int64
		want      string
		wantError bool
	}{
		{"e2", 0, 0, "e2-micro", false},
		{"e2", 250, gi, "e2-micro", false},
		{"e2", 500, 2 * gi, "e2-small", false},
		{"e2", 1000, 2 * gi, "e2-medium", false},        // shared cores count as their sustained CPU
		{"e2", 2000, 8 * gi, "e2-standard-2", false},    // predefined wins a tie with the same custom shape
		{"e2", 2000, 6 * gi, "e2-custom-2-6144", false}, // custom is cheaper than the next predefined
		{"e2", 3000, 4 * gi, "e2-custom-4-4096", false},
		{"n2d", 500, 2 * gi, "n2d-standard-2", false},
		{"t2d", 1000, 4 * gi, "t2d-standard-1", false},
		{"t2a", 2000, 8 * gi, "t2a-standard-2", false},
		{"n2d", 64000, gi, "", true},
		{"c4", 1000, gi, "", true},
	}
	for _, tt := range tests {
		got, err := PickShape(tt.fam, tt.cpu, tt.mem, gcefake.MachineTypes)
		if (err != nil) != tt.wantError || got != tt.want {
			t.Errorf("PickShape(%s, %d, %d) = %q, %v; want %q", tt.fam, tt.cpu, tt.mem, got, err, tt.want)
		}
	}
}
