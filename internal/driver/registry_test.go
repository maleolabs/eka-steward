package driver

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const validConfig = `
drivers:
  - tool: opencode
    cmd: opencode
    args: [acp]
  - tool: codex
    cmd: codex
  - tool: claude
    cmd: claude
`

func mustParse(t *testing.T, data string) *Registry {
	t.Helper()
	r, err := Parse([]byte(data))
	if err != nil {
		t.Fatalf("Parse(%q) unexpected error: %v", data, err)
	}
	return r
}

func TestParseSortsByToolAndDerivesKind(t *testing.T) {
	r := mustParse(t, validConfig)
	if got, want := strings.Join(r.Names(), ","), "claude,codex,opencode"; got != want {
		t.Fatalf("Names() = %q, want %q", got, want)
	}

	kinds := map[string]Kind{"opencode": KindACP, "claude": KindCLI, "codex": KindCLI}
	for tool, want := range kinds {
		s, err := r.Resolve(tool)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", tool, err)
		}
		if s.Kind != want {
			t.Fatalf("Resolve(%q).Kind = %q, want %q", tool, s.Kind, want)
		}
	}

	// Args are preserved verbatim.
	s, _ := r.Resolve("opencode")
	if s.Cmd != "opencode" || len(s.Args) != 1 || s.Args[0] != "acp" {
		t.Fatalf("Resolve(opencode) = %+v", s)
	}
}

func TestGeminiIsOptionalACPAdapter(t *testing.T) {
	r := mustParse(t, "drivers:\n  - tool: gemini\n    cmd: gemini\n    args: [--acp]\n")
	s, err := r.Resolve("gemini")
	if err != nil {
		t.Fatalf("Resolve(gemini): %v", err)
	}
	if s.Kind != KindACP {
		t.Fatalf("gemini Kind = %q, want %q", s.Kind, KindACP)
	}
}

func TestSelectIsDeterministic(t *testing.T) {
	r := mustParse(t, validConfig)

	d, ok := r.Select("opencode")
	if !ok {
		t.Fatal("Select(opencode) ok = false, want true")
	}
	if d.Spec.Tool != "opencode" || d.Spec.Kind != KindACP {
		t.Fatalf("Select(opencode) = %+v", d)
	}
	// Same input -> same result, every call.
	for i := 0; i < 50; i++ {
		again, ok := r.Select("opencode")
		if !ok || !reflect.DeepEqual(again.Spec, d.Spec) {
			t.Fatalf("Select drifted: %+v vs %+v", again, d)
		}
	}
}

func TestNamesDoNotRetainDeclarationOrder(t *testing.T) {
	a := mustParse(t, "drivers:\n  - tool: codex\n    cmd: codex\n  - tool: claude\n    cmd: claude\n")
	b := mustParse(t, "drivers:\n  - tool: claude\n    cmd: claude\n  - tool: codex\n    cmd: codex\n")
	if strings.Join(a.Names(), ",") != strings.Join(b.Names(), ",") {
		t.Fatalf("order not normalized: %v vs %v", a.Names(), b.Names())
	}
	ca, _ := a.Resolve("claude")
	cb, _ := b.Resolve("claude")
	if !reflect.DeepEqual(ca, cb) {
		t.Fatalf("Resolve(claude) differs by declaration order: %+v vs %+v", ca, cb)
	}
}

func TestResolveUnknownToolNamesIt(t *testing.T) {
	r := mustParse(t, validConfig)
	_, err := r.Resolve("ghost")
	if err == nil {
		t.Fatal("Resolve(ghost) = nil error, want error")
	}
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
	if !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("error %q does not name the unknown tool", err)
	}

	if _, ok := r.Select("ghost"); ok {
		t.Fatal("Select(ghost) ok = true, want false")
	}
	if r.Has("ghost") {
		t.Fatal("Has(ghost) = true, want false")
	}
	if r.Has("claude") != true {
		t.Fatal("Has(claude) = false, want true")
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
			config:  "drivers:\n  - tool: claude\n    cmd: c\n    bogus: x\n",
			wantSub: "bogus",
		},
		{
			name:    "unknown top-level field",
			config:  "extra: 1\ndrivers: []\n",
			wantSub: "extra",
		},
		{
			name:    "kind is not configurable",
			config:  "drivers:\n  - tool: claude\n    cmd: c\n    kind: acp\n",
			wantSub: "kind",
		},
		{
			name:    "duplicate tool",
			config:  "drivers:\n  - tool: claude\n    cmd: c\n  - tool: claude\n    cmd: d\n",
			wantSub: `duplicate tool "claude"`,
		},
		{
			name:    "empty tool",
			config:  "drivers:\n  - cmd: claude\n",
			wantSub: "empty tool",
		},
		{
			name:    "empty cmd",
			config:  "drivers:\n  - tool: claude\n",
			wantSub: `driver "claude" has empty cmd`,
		},
		{
			name:    "unknown tool",
			config:  "drivers:\n  - tool: ghost\n    cmd: ghost\n",
			wantSub: `unknown tool "ghost"`,
		},
		{
			name:    "malformed yaml",
			config:  "drivers: [",
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

func TestParseUnknownToolIsErrUnknownTool(t *testing.T) {
	_, err := Parse([]byte("drivers:\n  - tool: ghost\n    cmd: ghost\n"))
	if !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v, want ErrUnknownTool", err)
	}
}

func TestParseEmptyDriverListIsEmptyRegistry(t *testing.T) {
	r := mustParse(t, "drivers: []\n")
	if len(r.Names()) != 0 {
		t.Fatalf("Names() = %v, want empty", r.Names())
	}
	if _, ok := r.Select("opencode"); ok {
		t.Fatal("empty registry selected a driver")
	}
}

func TestParseErrorsAreDeterministic(t *testing.T) {
	cfg := "drivers:\n  - tool: ghost\n    cmd: ghost\n"
	_, err1 := Parse([]byte(cfg))
	_, err2 := Parse([]byte(cfg))
	if err1 == nil || err2 == nil || err1.Error() != err2.Error() {
		t.Fatalf("parse error not deterministic: %v vs %v", err1, err2)
	}
}
