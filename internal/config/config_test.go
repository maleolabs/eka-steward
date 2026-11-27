package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/maleolabs/eka-steward/v2/internal/driver"
)

// validV1 is a minimal, valid current-version document. Individual tests
// mutate one part of it to isolate a single failure.
const validV1 = `version: 1
roles:
  - name: impl
    driver: claude
    template: prompts/impl.md
drivers:
  - tool: claude
    cmd: claude
sizing:
  maxWorkers: 1
`

func testdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return b
}

// TestParseValidGolden pins the golden fixture: every section is decoded and
// validated, and both role and driver lists are in deterministic (sorted)
// order with driver kinds derived from their adapters.
func TestParseValidGolden(t *testing.T) {
	c, err := Parse(testdata(t, "valid.yaml"))
	if err != nil {
		t.Fatalf("Parse(valid.yaml) = %v, want nil", err)
	}

	if c.Version != Version {
		t.Fatalf("Version = %d, want %d", c.Version, Version)
	}

	if len(c.Roles) != 2 {
		t.Fatalf("roles = %d, want 2", len(c.Roles))
	}
	if c.Roles[0].Name != "implementer" || c.Roles[0].Driver != "opencode" || c.Roles[0].Template != "prompts/implementer.md" {
		t.Fatalf("roles[0] = %+v, want the sorted first role", c.Roles[0])
	}
	if c.Roles[1].Name != "reviewer" || !c.Roles[1].Review {
		t.Fatalf("roles[1] = %+v, want reviewer/review:true", c.Roles[1])
	}

	if len(c.Drivers) != 2 {
		t.Fatalf("drivers = %d, want 2", len(c.Drivers))
	}
	if c.Drivers[0].Tool != "claude" || c.Drivers[0].Kind != driver.KindCLI {
		t.Fatalf("drivers[0] = %+v, want claude/cli", c.Drivers[0])
	}
	if c.Drivers[1].Tool != "opencode" || c.Drivers[1].Kind != driver.KindACP || !reflect.DeepEqual(c.Drivers[1].Args, []string{"acp"}) {
		t.Fatalf("drivers[1] = %+v, want opencode/acp args [acp]", c.Drivers[1])
	}

	if c.Sizing.MaxWorkers != 4 || c.Sizing.CostCeiling != 2 {
		t.Fatalf("sizing = %+v, want {4, 2}", c.Sizing)
	}
	if c.Sandbox.WorktreeBase != "/var/lib/eka-steward/worktrees" || !c.Sandbox.Contained() {
		t.Fatalf("sandbox = %+v, want the pinned base and contained", c.Sandbox)
	}
	if c.Loop.MaxAttempts != 3 || c.Loop.QueueLimit != 8 || c.Loop.AttemptTimeout != Duration(30*time.Second) {
		t.Fatalf("loop = %+v, want {3, 8, 30s}", c.Loop)
	}
}

// TestParseRefusesStrictly covers the strict-parse and per-section validation
// failures. Every case must be refused with an error that names the offending
// field or value.
func TestParseRefusesStrictly(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string // required substring of the error
	}{
		{
			name: "unknown top-level field",
			doc:  "version: 1\nbogus: 1\n",
			want: "not found",
		},
		{
			name: "unknown field inside a role",
			doc:  strings.Replace(validV1, "    template: prompts/impl.md", "    template: t\n    bogus: 1", 1),
			want: "not found",
		},
		{
			name: "driver kind is not configurable",
			doc:  strings.Replace(validV1, "    cmd: claude", "    cmd: claude\n    kind: cli", 1),
			want: "not found",
		},
		{
			name: "sandbox has no opt-out field",
			doc:  validV1 + "sandbox:\n  disable: true\n",
			want: "not found",
		},
		{
			name: "role references an unknown driver",
			doc:  strings.Replace(validV1, "driver: claude", "driver: ghost", 1),
			want: `unknown driver "ghost"`,
		},
		{
			name: "driver names an unknown tool",
			doc: strings.Replace(validV1,
				"drivers:\n  - tool: claude\n    cmd: claude", "drivers:\n  - tool: ghost\n    cmd: ghost", 1),
			want: "unknown tool",
		},
		{
			name: "sizing maxWorkers < 1",
			doc:  strings.Replace(validV1, "  maxWorkers: 1", "  maxWorkers: 0", 1),
			want: "sizing.maxWorkers",
		},
		{
			name: "sizing maxWorkers negative",
			doc:  strings.Replace(validV1, "  maxWorkers: 1", "  maxWorkers: -3", 1),
			want: "sizing.maxWorkers",
		},
		{
			name: "missing roles",
			doc:  strings.Replace(validV1, "roles:\n  - name: impl\n    driver: claude\n    template: prompts/impl.md", "roles: []", 1),
			want: "at least one role",
		},
		{
			name: "role with empty template",
			doc:  strings.Replace(validV1, "    template: prompts/impl.md", `    template: ""`, 1),
			want: "empty template",
		},
		{
			name: "relative sandbox worktree base",
			doc:  validV1 + "sandbox:\n  worktreeBase: worktrees\n",
			want: "must be an absolute path",
		},
		{
			name: "negative loop attemptTimeout",
			doc:  validV1 + "loop:\n  attemptTimeout: -1s\n",
			want: "loop.attemptTimeout",
		},
		{
			name: "negative loop queueLimit",
			doc:  validV1 + "loop:\n  queueLimit: -1\n",
			want: "loop.queueLimit",
		},
		{
			name: "malformed duration",
			doc:  validV1 + "loop:\n  attemptTimeout: not-a-duration\n",
			want: "invalid duration",
		},
		{
			name: "malformed yaml",
			doc:  "version: 1\nroles: [unclosed\n",
			want: "config: parse",
		},
		{
			name: "empty document",
			doc:  "",
			want: "config: parse",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.doc))
			if err == nil {
				t.Fatalf("Parse(%q) = %+v, want a refusal", tc.name, c)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParseVersionGate pins the version rules: a future/unknown version is
// refused deterministically, and the refusal is stable across runs.
func TestParseVersionGate(t *testing.T) {
	for _, doc := range []string{
		strings.Replace(validV1, "version: 1", "version: 2", 1),
		strings.Replace(validV1, "version: 1", "version: 99", 1),
	} {
		first := func() string {
			_, err := Parse([]byte(doc))
			if err == nil {
				t.Fatal("future version accepted, want a refusal")
			}
			return err.Error()
		}
		got := first()
		if !strings.Contains(got, "newer than") {
			t.Fatalf("error %q does not name a future version", got)
		}
		if again := first(); again != got {
			t.Fatalf("version error not deterministic: %q vs %q", got, again)
		}
	}

	if _, err := Parse([]byte(strings.Replace(validV1, "version: 1", "version: -1", 1))); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("version -1 error = %v, want a deterministic 'not supported' refusal", err)
	}
}

// TestMigrateV0ToCurrent pins the documented 0 -> 1 migration: a version-0
// document (roles + drivers only) parses, migrates to the current version and
// gains the sizing default. An omitted version defaults to 0 (documented) and
// migrates the same way.
func TestMigrateV0ToCurrent(t *testing.T) {
	check := func(t *testing.T, doc []byte) *Config {
		t.Helper()
		c, err := Parse(doc)
		if err != nil {
			t.Fatalf("Parse = %v, want nil", err)
		}
		if c.Version != Version {
			t.Fatalf("Version = %d, want migrated %d", c.Version, Version)
		}
		if c.Sizing.MaxWorkers != 1 {
			t.Fatalf("migrated Sizing.MaxWorkers = %d, want the conservative floor 1", c.Sizing.MaxWorkers)
		}
		if !c.Sandbox.Contained() {
			t.Fatal("migrated sandbox not contained")
		}
		if len(c.Roles) != 1 || c.Roles[0].Name != "implementer" {
			t.Fatalf("migrated roles = %+v, want [implementer]", c.Roles)
		}
		return c
	}

	t.Run("explicit version 0", func(t *testing.T) {
		check(t, testdata(t, "v0.yaml"))
	})

	t.Run("absent version defaults to 0", func(t *testing.T) {
		c := check(t, []byte("roles:\n  - name: implementer\n    driver: claude\n    template: t\ndrivers:\n  - tool: claude\n    cmd: claude\n"))
		if c.Sizing.MaxWorkers != 1 {
			t.Fatalf("Sizing.MaxWorkers = %d, want 1", c.Sizing.MaxWorkers)
		}
	})
}

// TestMigrateRejectsUnsupported pins Migrate's refusals directly: an unknown
// or future version errors, while any supported version lands on the current
// one.
func TestMigrateRejectsUnsupported(t *testing.T) {
	if err := (&Config{}).Migrate(2); err == nil || !strings.Contains(err.Error(), "newer than") {
		t.Fatalf("Migrate(2) = %v, want a future-version refusal", err)
	}
	if err := (&Config{}).Migrate(-1); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("Migrate(-1) = %v, want an unsupported-version refusal", err)
	}

	c := &Config{Sizing: SizingConfig{MaxWorkers: 7}}
	if err := c.Migrate(0); err != nil {
		t.Fatalf("Migrate(0) = %v, want nil", err)
	}
	if c.Version != Version {
		t.Fatalf("Migrate(0) left Version = %d, want %d", c.Version, Version)
	}
	if c.Sizing.MaxWorkers != 7 {
		t.Fatalf("Migrate(0) overwrote an explicit MaxWorkers: %d, want 7", c.Sizing.MaxWorkers)
	}
}

// TestParseDeterministic pins the stability property: Parse is a pure function
// of its bytes — identical input yields deeply-equal configs and
// byte-identical errors, with no map iteration leaking into the result.
func TestParseDeterministic(t *testing.T) {
	doc := testdata(t, "valid.yaml")
	first, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse = %v, want nil", err)
	}
	for i := 0; i < 3; i++ {
		got, err := Parse(doc)
		if err != nil {
			t.Fatalf("Parse run %d = %v, want nil", i, err)
		}
		if !reflect.DeepEqual(first, got) {
			t.Fatalf("Parse run %d differs from the first parse", i)
		}
	}

	bad := []byte(strings.Replace(validV1, "driver: claude", "driver: ghost", 1))
	_, e1 := Parse(bad)
	for i := 0; i < 3; i++ {
		_, e := Parse(bad)
		if e == nil || e.Error() != e1.Error() {
			t.Fatalf("bad-doc error run %d = %v, want stable %q", i, e, e1)
		}
	}
}

// TestSandboxContainedByDefault pins the containment property: an empty
// sandbox section is contained, and there is no field that turns it off
// (an opt-out field is refused as unknown).
func TestSandboxContainedByDefault(t *testing.T) {
	if !(SandboxConfig{}).Contained() {
		t.Fatal("zero SandboxConfig is not contained")
	}
	c, err := Parse([]byte(validV1 + "sandbox: {}\n"))
	if err != nil {
		t.Fatalf("Parse with empty sandbox = %v, want nil", err)
	}
	if !c.Sandbox.Contained() {
		t.Fatal("parsed empty sandbox is not contained")
	}
}
