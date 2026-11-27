package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRunner is the test double for the Runner seam: it returns canned
// stdout (or an error) and records every Spec it was handed. It NEVER
// spawns a process, so unit tests exercise probing with no subprocess.
type fakeRunner struct {
	out []byte
	err error
	got []Spec
}

func (f *fakeRunner) Run(_ context.Context, spec Spec) ([]byte, error) {
	f.got = append(f.got, spec)
	return f.out, f.err
}

// opencodeInitialize is the ACTUAL initialize response captured from
// `opencode acp` 2.4.1 in the ACP spike (docs/acp-spike.md §2.1).
const opencodeInitialize = `{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{"loadSession":true,"mcpCapabilities":{"http":true,"sse":false},"promptCapabilities":{"embeddedContext":true,"image":true},"sessionCapabilities":{"additionalDirectories":{},"close":{},"delete":{},"fork":{},"list":{},"resume":{}},"_meta":{"opencode/child-session-updates":true}},"authMethods":[{"description":"Run opencode auth login","name":"Login with opencode","id":"opencode-login"}],"_meta":{"steering":{"supported":true}},"agentInfo":{"name":"OpenCode","version":"2.4.1"}}}`

func TestCLIProbeRunsVersionAndRecordsString(t *testing.T) {
	r := mustParse(t, validConfig)
	f := &fakeRunner{out: []byte("1.2.3\n")}

	caps, err := r.Probe(context.Background(), "claude", f)
	if err != nil {
		t.Fatalf("Probe(claude): %v", err)
	}
	if caps.Kind != KindCLI || caps.Version != "1.2.3" || caps.ACPVersion != 0 {
		t.Fatalf("caps = %+v", caps)
	}
	if caps.Raw["version"] != "1.2.3" {
		t.Fatalf("Raw = %v", caps.Raw)
	}

	// The runner must have received the probe argv: <cmd> --version.
	if len(f.got) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(f.got))
	}
	gs := f.got[0]
	if gs.Tool != "claude" || gs.Cmd != "claude" || len(gs.Args) != 1 || gs.Args[0] != "--version" {
		t.Fatalf("probe spec = %+v", gs)
	}
}

func TestCLIProbeTakesFirstNonEmptyLine(t *testing.T) {
	r := mustParse(t, "drivers:\n  - tool: codex\n    cmd: codex\n")
	f := &fakeRunner{out: []byte("\n  \ncodex-cli 0.9.0\n(the rest)\n")}
	caps, err := r.Probe(context.Background(), "codex", f)
	if err != nil {
		t.Fatalf("Probe(codex): %v", err)
	}
	if caps.Version != "codex-cli 0.9.0" {
		t.Fatalf("Version = %q", caps.Version)
	}
}

func TestACPProbeRecordsProtocolAndCapabilities(t *testing.T) {
	r := mustParse(t, validConfig)
	f := &fakeRunner{out: []byte(opencodeInitialize + "\n")}

	caps, err := r.Probe(context.Background(), "opencode", f)
	if err != nil {
		t.Fatalf("Probe(opencode): %v", err)
	}
	if caps.Kind != KindACP {
		t.Fatalf("Kind = %q, want %q", caps.Kind, KindACP)
	}
	if caps.ACPVersion != 1 {
		t.Fatalf("ACPVersion = %d, want 1", caps.ACPVersion)
	}
	if caps.Version != "2.4.1" {
		t.Fatalf("Version = %q, want 2.4.1", caps.Version)
	}
	want := map[string]string{
		"loadSession":                          "true",
		"mcpCapabilities.http":                 "true",
		"mcpCapabilities.sse":                  "false",
		"promptCapabilities.image":             "true",
		"promptCapabilities.embeddedContext":   "true",
		"_meta.opencode/child-session-updates": "true",
	}
	for k, v := range want {
		if caps.Raw[k] != v {
			t.Fatalf("Raw[%q] = %q, want %q (full: %v)", k, caps.Raw[k], v, caps.Raw)
		}
	}
}

func TestACPProbeIsDeterministic(t *testing.T) {
	r := mustParse(t, validConfig)
	var first Capabilities
	for i := 0; i < 20; i++ {
		f := &fakeRunner{out: []byte(opencodeInitialize)}
		caps, err := r.Probe(context.Background(), "opencode", f)
		if err != nil {
			t.Fatalf("Probe: %v", err)
		}
		if i > 0 && !equalCaps(first, caps) {
			t.Fatalf("probe drifted: %+v vs %+v", first, caps)
		}
		first = caps
	}
}

func TestACPProbeSurfacesAgentError(t *testing.T) {
	r := mustParse(t, "drivers:\n  - tool: gemini\n    cmd: gemini\n    args: [--acp]\n")
	f := &fakeRunner{out: []byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"gated"}}`)}
	_, err := r.Probe(context.Background(), "gemini", f)
	if err == nil {
		t.Fatal("Probe(gemini) = nil error, want agent error")
	}
	for _, want := range []string{"gemini", "-32000", "gated"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}

func TestACPProbeNoResponseIsError(t *testing.T) {
	r := mustParse(t, validConfig)
	f := &fakeRunner{out: []byte("not json\n")}
	_, err := r.Probe(context.Background(), "opencode", f)
	if err == nil || !strings.Contains(err.Error(), "no initialize response") {
		t.Fatalf("err = %v, want no-initialize-response", err)
	}
}

func TestProbeRunnerErrorPropagates(t *testing.T) {
	r := mustParse(t, validConfig)
	boom := errors.New("boom")
	f := &fakeRunner{err: boom}
	_, err := r.Probe(context.Background(), "claude", f)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wrapped boom", err)
	}
}

func TestProbeUnknownToolNamesIt(t *testing.T) {
	r := mustParse(t, validConfig)
	_, err := r.Probe(context.Background(), "ghost", &fakeRunner{})
	if !errors.Is(err, ErrUnknownTool) || !strings.Contains(err.Error(), `"ghost"`) {
		t.Fatalf("err = %v, want ErrUnknownTool naming ghost", err)
	}
}

func TestSelectProbedStoresCapsOnResolvedDriver(t *testing.T) {
	r := mustParse(t, validConfig)
	f := &fakeRunner{out: []byte(opencodeInitialize)}

	d, err := r.SelectProbed(context.Background(), "opencode", f)
	if err != nil {
		t.Fatalf("SelectProbed: %v", err)
	}
	if d.Spec.Tool != "opencode" {
		t.Fatalf("Spec = %+v", d.Spec)
	}
	if d.Caps.Kind != KindACP || d.Caps.ACPVersion != 1 {
		t.Fatalf("Caps not stored on driver: %+v", d.Caps)
	}
}

// equalCaps compares the deterministic fields of two Capabilities.
func equalCaps(a, b Capabilities) bool {
	if a.Kind != b.Kind || a.Version != b.Version || a.ACPVersion != b.ACPVersion || len(a.Raw) != len(b.Raw) {
		return false
	}
	for k, v := range a.Raw {
		if b.Raw[k] != v {
			return false
		}
	}
	return true
}

// TestRealCLIProbeAgainstInstalledTools is a real-binary integration test:
// it probes whichever CLI-adapter tool is actually installed. It is SKIPPED
// per-tool when the binary is absent (lookPath), so CI without agent CLIs
// stays green. It only exercises CLI adapters (`--version`); the ACP
// round-trip needs a stdin pipe escapehatch does not have yet and is
// deferred to the streaming client (docs/drivers.md).
func TestRealCLIProbeAgainstInstalledTools(t *testing.T) {
	r := mustParse(t, "drivers:\n  - tool: claude\n    cmd: claude\n  - tool: codex\n    cmd: codex\n")
	run := r.Runner()

	for _, tool := range []string{"claude", "codex"} {
		t.Run(tool, func(t *testing.T) {
			if _, ok := lookPath(tool); !ok {
				t.Skipf("%s not installed; skipping real-binary probe", tool)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			caps, err := r.Probe(ctx, tool, run)
			if err != nil {
				t.Fatalf("real probe %s: %v", tool, err)
			}
			if caps.Kind != KindCLI || caps.Version == "" {
				t.Fatalf("real probe %s caps = %+v", tool, caps)
			}
			t.Logf("real probe %s -> %q", tool, caps.Version)
		})
	}
}

// lookPath resolves name on PATH WITHOUT importing os/exec (which the
// boundary keeps out of this repo outside internal/escapehatch).
func lookPath(name string) (string, bool) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if info, err := os.Stat(p); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return p, true
		}
	}
	return "", false
}
