package guard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/driver"
)

// TestVerifyBinaryAcceptsMatchingChecksum is AC1's binary half at the
// per-Spec acceptance point: a pinned Spec whose resolved binary matches its
// checksum is accepted.
func TestVerifyBinaryAcceptsMatchingChecksum(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude-bin")
	content := []byte("#!/bin/sh\necho pinned\n")
	if err := os.WriteFile(bin, content, 0o755); err != nil {
		t.Fatal(err)
	}
	spec := driver.Spec{Tool: "claude", Cmd: bin, Version: "1.0.0", Checksum: sha256hex(content)}
	if err := VerifyBinary(spec); err != nil {
		t.Fatalf("VerifyBinary refused a matching pin: %v", err)
	}
}

// TestVerifyBinaryRefusesUnpinnedAndMismatch pins the per-Spec refusals:
// missing version, missing checksum, and a checksum mismatch each wrap
// ErrUnpinned and name the tool.
func TestVerifyBinaryRefusesUnpinnedAndMismatch(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "claude-bin")
	if err := os.WriteFile(bin, []byte("real"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		spec driver.Spec
		want string
	}{
		{"no version", driver.Spec{Tool: "claude", Cmd: bin, Checksum: "aa"}, "version"},
		{"no checksum", driver.Spec{Tool: "claude", Cmd: bin, Version: "1.0.0"}, "checksum"},
		{"mismatch", driver.Spec{Tool: "claude", Cmd: bin, Version: "1.0.0", Checksum: sha256hex([]byte("other"))}, "mismatch"},
		{"unresolvable", driver.Spec{Tool: "claude", Cmd: filepath.Join(t.TempDir(), "absent"), Version: "1.0.0", Checksum: "aa"}, "not found"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := VerifyBinary(tc.spec)
			if !errors.Is(err, ErrUnpinned) {
				t.Fatalf("err = %v, want ErrUnpinned", err)
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"claude"`) {
				t.Fatalf("error %q must name the tool and %q", err, tc.want)
			}
		})
	}
}
