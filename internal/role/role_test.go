package role

import (
	"strings"
	"testing"
)

const validConfig = `
roles:
  - name: zeta
    driver: claude
    template: prompts/zeta.md
    review: true
  - name: alpha
    driver: aider
    template: prompts/alpha.md
  - name: beta
    driver: claude
    template: prompts/beta.md
    review: false
`

func mustParse(t *testing.T, data string) *Set {
	t.Helper()
	s, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse(%q) unexpected error: %v", data, err)
	}
	return s
}

func TestParseSortsByName(t *testing.T) {
	s := mustParse(t, validConfig)
	got := strings.Join(s.Names(), ",")
	if want := "alpha,beta,zeta"; got != want {
		t.Fatalf("Names() = %q, want %q", got, want)
	}
}

func TestResolveIsDeterministic(t *testing.T) {
	s := mustParse(t, validConfig)
	for i := 0; i < 50; i++ {
		r, err := s.Resolve("beta")
		if err != nil {
			t.Fatalf("Resolve(beta) error: %v", err)
		}
		if r.Name != "beta" || r.Driver != "claude" || r.Template != "prompts/beta.md" || r.Review {
			t.Fatalf("Resolve(beta) = %+v", r)
		}
	}
}

func TestResolveUnknownNamesIt(t *testing.T) {
	s := mustParse(t, validConfig)
	_, err := s.Resolve("ghost")
	if err == nil {
		t.Fatal("Resolve(ghost) = nil error, want error")
	}
	if !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("error %q does not name the unknown role", err)
	}
}

func TestReviewRolesDeterministicOrder(t *testing.T) {
	s := mustParse(t, validConfig)
	rr := s.ReviewRoles()
	if len(rr) != 1 || rr[0].Name != "zeta" {
		t.Fatalf("ReviewRoles() = %+v, want only zeta", rr)
	}
	// Stable across calls.
	for i := 0; i < 20; i++ {
		again := s.ReviewRoles()
		if len(again) != 1 || again[0].Name != "zeta" {
			t.Fatalf("ReviewRoles() drifted: %+v", again)
		}
	}
}

func TestReviewExplicitFalseIsAccepted(t *testing.T) {
	// review absent or explicitly false both mean a non-review role.
	s := mustParse(t, validConfig)
	if r, _ := s.Resolve("alpha"); r.Review {
		t.Fatalf("alpha Review = true, want false (field absent)")
	}
	if r, _ := s.Resolve("beta"); r.Review {
		t.Fatalf("beta Review = true, want false (review:false)")
	}
}

func TestParseStrictErrors(t *testing.T) {
	tests := []struct {
		name    string
		config  string
		wantSub string
	}{
		{
			name:    "unknown field",
			config:  "roles:\n  - name: a\n    driver: d\n    template: t\n    bogus: x\n",
			wantSub: "bogus",
		},
		{
			name:    "unknown top-level field",
			config:  "extra: 1\nroles: []\n",
			wantSub: "extra",
		},
		{
			name:    "duplicate name",
			config:  "roles:\n  - name: a\n    driver: d\n    template: t\n  - name: a\n    driver: e\n    template: u\n",
			wantSub: `duplicate role name "a"`,
		},
		{
			name:    "empty name",
			config:  "roles:\n  - name: \"\"\n    driver: d\n    template: t\n",
			wantSub: "empty name",
		},
		{
			name:    "empty driver",
			config:  "roles:\n  - name: a\n    template: t\n",
			wantSub: `role "a" has empty driver`,
		},
		{
			name:    "empty template",
			config:  "roles:\n  - name: a\n    driver: d\n",
			wantSub: `role "a" has empty template`,
		},
		{
			name:    "malformed yaml",
			config:  "roles: [",
			wantSub: "parse config",
		},
		{
			name:    "empty bytes",
			config:  "",
			wantSub: "parse config",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.config))
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want error containing %q", tc.config, tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not contain %q", err, tc.wantSub)
			}
		})
	}
}

func TestParseEmptyRoleListIsEmptySet(t *testing.T) {
	s := mustParse(t, "roles: []\n")
	if len(s.Names()) != 0 {
		t.Fatalf("Names() = %v, want empty", s.Names())
	}
	if len(s.ReviewRoles()) != 0 {
		t.Fatalf("ReviewRoles() = %v, want empty", s.ReviewRoles())
	}
}

type fakeDrivers map[string]bool

func (f fakeDrivers) Has(name string) bool { return f[name] }

func TestValidateDrivers(t *testing.T) {
	s := mustParse(t, validConfig)

	if err := s.ValidateDrivers(fakeDrivers{"claude": true, "aider": true}); err != nil {
		t.Fatalf("ValidateDrivers(all known) error: %v", err)
	}

	err := s.ValidateDrivers(fakeDrivers{"claude": true})
	if err == nil {
		t.Fatal("ValidateDrivers(missing aider) = nil error, want error")
	}
	for _, want := range []string{`role "alpha"`, `driver "aider"`} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestParseDoesNotRetainInputOrder(t *testing.T) {
	// Same roles, opposite declaration order -> identical deterministic set.
	a := mustParse(t, "roles:\n  - name: b\n    driver: d\n    template: t\n  - name: a\n    driver: d\n    template: t\n")
	b := mustParse(t, "roles:\n  - name: a\n    driver: d\n    template: t\n  - name: b\n    driver: d\n    template: t\n")
	if strings.Join(a.Names(), ",") != strings.Join(b.Names(), ",") {
		t.Fatalf("order not normalized: %v vs %v", a.Names(), b.Names())
	}
}
