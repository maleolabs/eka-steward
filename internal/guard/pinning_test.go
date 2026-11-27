package guard

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/driver"
)

func sha256hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func mustRegistry(t *testing.T, cfg string) *driver.Registry {
	t.Helper()
	reg, err := driver.Parse([]byte(cfg))
	if err != nil {
		t.Fatalf("driver.Parse: %v", err)
	}
	return reg
}

func TestRequirePinnedAcceptsMatchingChecksum(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude-bin")
	content := []byte("#!/bin/sh\necho pinned\n")
	if err := os.WriteFile(bin, content, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("drivers:\n  - tool: claude\n    cmd: %s\n    version: 1.2.3\n    checksum: %s\n",
		bin, sha256hex(content))
	if err := RequirePinned(mustRegistry(t, cfg)); err != nil {
		t.Fatalf("RequirePinned refused a correctly pinned driver: %v", err)
	}
}

func TestRequirePinnedRefusesUnpinned(t *testing.T) {
	tests := []struct {
		name string
		cfg  string
		want string
	}{
		{
			name: "missing version",
			cfg:  "drivers:\n  - tool: claude\n    cmd: claude\n    checksum: deadbeef\n",
			want: "version",
		},
		{
			name: "missing checksum",
			cfg:  "drivers:\n  - tool: claude\n    cmd: claude\n    version: 1.0.0\n",
			want: "checksum",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := RequirePinned(mustRegistry(t, tc.cfg))
			if !errors.Is(err, ErrUnpinned) {
				t.Fatalf("err = %v, want ErrUnpinned", err)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"claude"`) {
				t.Fatalf("error %q must name the tool and the missing %q", err, tc.want)
			}
		})
	}
}

func TestRequirePinnedRefusesChecksumMismatch(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude-bin")
	if err := os.WriteFile(bin, []byte("real content"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("drivers:\n  - tool: claude\n    cmd: %s\n    version: 1.0.0\n    checksum: %s\n",
		bin, sha256hex([]byte("different content")))
	err := RequirePinned(mustRegistry(t, cfg))
	if !errors.Is(err, ErrUnpinned) || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("err = %v, want an ErrUnpinned checksum mismatch", err)
	}
}

func TestRequirePinnedRefusesUnresolvableBinary(t *testing.T) {
	cfg := "drivers:\n  - tool: claude\n    cmd: " + filepath.Join(t.TempDir(), "absent") +
		"\n    version: 1.0.0\n    checksum: " + sha256hex([]byte("x")) + "\n"
	err := RequirePinned(mustRegistry(t, cfg))
	if !errors.Is(err, ErrUnpinned) || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("err = %v, want an ErrUnpinned not-found error", err)
	}
}

func TestRequirePinnedEmptyRegistryIsPinned(t *testing.T) {
	if err := RequirePinned(mustRegistry(t, "drivers: []\n")); err != nil {
		t.Fatalf("empty registry must be vacuously pinned, got %v", err)
	}
}

func TestRequirePinnedErrorsAreDeterministic(t *testing.T) {
	reg := mustRegistry(t, "drivers:\n  - tool: claude\n    cmd: claude\n")
	e1, e2 := RequirePinned(reg), RequirePinned(reg)
	if e1 == nil || e2 == nil || e1.Error() != e2.Error() {
		t.Fatalf("error not deterministic: %v vs %v", e1, e2)
	}
}
