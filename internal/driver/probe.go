package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Runner executes ONE driver probe and returns the captured stdout. It is
// the seam that keeps capability probing testable and spawn-free: the
// production Runner is escapehatch-backed (ExecRunner in exec_runner.go),
// unit tests pass a fake.
//
// For a CLI adapter (CLIProbe) Run receives a Spec whose Args are the
// probe argv (`--version`) and returns the tool's stdout.
//
// For an ACP adapter (ACPProbe) Run receives the driver's Spec (e.g.
// `opencode acp`) and returns the agent's initialize RESPONSE line(s). The
// initialize REQUEST is delivered by the Runner — this package builds the
// parse, not the transport. The escapehatch Runner has no stdin pipe yet,
// so real ACP probing over it is deferred to the streaming ACP client; see
// docs/drivers.md.
type Runner interface {
	Run(ctx context.Context, spec Spec) ([]byte, error)
}

// adapter binds a known tool to its wire kind and its probe function.
type adapter struct {
	kind  Kind
	probe probeFunc
}

// probeFunc performs one adapter-specific probe against a Runner.
type probeFunc func(ctx context.Context, spec Spec, run Runner) (Capabilities, error)

// adapters is the fixed table of KNOWN TOOLS. This is where "known tool" is
// defined: a driver whose Tool is not a key here is rejected by Parse, and
// Resolve/Select/Probe on it fail with ErrUnknownTool.
//
// ACP-capable agents here are `opencode acp` and `gemini --acp` (both
// negotiate ACP v1 on the wire — ACP spike §2). `claude` and `codex` do NOT
// speak ACP, so they are CLI adapters probed via `--version`. Encoding the
// kind here (and copying it onto Spec.Kind) is how the difference is made
// explicit and machine-checkable.
var adapters = map[string]adapter{
	"opencode": {kind: KindACP, probe: ACPProbe},
	"gemini":   {kind: KindACP, probe: ACPProbe}, // optional extra ACP adapter
	"claude":   {kind: KindCLI, probe: CLIProbe},
	"codex":    {kind: KindCLI, probe: CLIProbe},
}

// Probe runs the adapter for tool and returns its recorded Capabilities.
// An unknown tool fails with a deterministic error naming it (wraps
// ErrUnknownTool). The Runner may be a fake in tests.
func (r *Registry) Probe(ctx context.Context, tool string, run Runner) (Capabilities, error) {
	spec, ok := r.index[tool]
	if !ok {
		return Capabilities{}, fmt.Errorf("%w %q", ErrUnknownTool, tool)
	}
	return probeSpec(ctx, spec, run)
}

// SelectProbed resolves tool, probes it via run, and returns the resolved
// Driver with Caps recorded on it. An unknown tool fails exactly as
// Resolve / Probe do. This is the "capability probe is stored on the
// resolved driver" flow.
func (r *Registry) SelectProbed(ctx context.Context, tool string, run Runner) (Driver, error) {
	spec, ok := r.index[tool]
	if !ok {
		return Driver{}, fmt.Errorf("%w %q", ErrUnknownTool, tool)
	}
	caps, err := probeSpec(ctx, spec, run)
	if err != nil {
		return Driver{}, err
	}
	return Driver{Spec: spec, Caps: caps}, nil
}

// probeSpec dispatches to the adapter probe for spec.Tool. Parse guarantees
// every registered spec has an adapter.
func probeSpec(ctx context.Context, spec Spec, run Runner) (Capabilities, error) {
	a, ok := adapters[spec.Tool]
	if !ok {
		return Capabilities{}, fmt.Errorf("%w %q", ErrUnknownTool, spec.Tool)
	}
	return a.probe(ctx, spec, run)
}

// CLIProbe probes a non-ACP tool by running `<cmd> --version` and recording
// the first non-empty line as the version. It is tiny on purpose: a CLI
// adapter has no protocol handshake to negotiate.
func CLIProbe(ctx context.Context, spec Spec, run Runner) (Capabilities, error) {
	probeSpec := Spec{Tool: spec.Tool, Cmd: spec.Cmd, Args: []string{"--version"}, Kind: KindCLI}
	out, err := run.Run(ctx, probeSpec)
	if err != nil {
		return Capabilities{}, fmt.Errorf("driver: cli probe %q: %w", spec.Tool, err)
	}
	v := firstLine(out)
	return Capabilities{
		Kind:    KindCLI,
		Version: v,
		Raw:     map[string]string{"version": v},
	}, nil
}

// ACPProbe probes an ACP agent. It parses the agent's initialize response —
// the line carrying `result.protocolVersion` — and records the negotiated
// protocolVersion, the agent version and a flattened view of
// agentCapabilities. It performs ONE round-trip then stops; the streaming
// ACP client is a separate work item.
//
// The initialize REQUEST is delivered by the Runner (see Runner docs); this
// probe owns only the response parse.
func ACPProbe(ctx context.Context, spec Spec, run Runner) (Capabilities, error) {
	out, err := run.Run(ctx, spec)
	if err != nil {
		return Capabilities{}, fmt.Errorf("driver: acp probe %q: %w", spec.Tool, err)
	}
	return parseACPInitialize(spec.Tool, out)
}

// rpcFrame is the subset of a JSON-RPC 2.0 frame the ACP probe reads.
type rpcFrame struct {
	Result struct {
		ProtocolVersion   int             `json:"protocolVersion"`
		AgentCapabilities json.RawMessage `json:"agentCapabilities"`
		AgentInfo         struct {
			Version string `json:"version"`
		} `json:"agentInfo"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// parseACPInitialize scans out for the initialize response frame and builds
// its Capabilities. A non-frame line is ignored; an error frame is an error;
// no response at all is an error. All errors name the tool.
func parseACPInitialize(tool string, out []byte) (Capabilities, error) {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var f rpcFrame
		if err := json.Unmarshal([]byte(line), &f); err != nil {
			continue // not a JSON-RPC frame
		}
		if f.Error != nil {
			return Capabilities{}, fmt.Errorf("driver: acp probe %q: agent error %d: %s", tool, f.Error.Code, f.Error.Message)
		}
		if f.Result.ProtocolVersion == 0 {
			continue // not the initialize response
		}
		raw := map[string]string{}
		flattenJSON("", f.Result.AgentCapabilities, raw)
		return Capabilities{
			Kind:       KindACP,
			Version:    f.Result.AgentInfo.Version,
			ACPVersion: f.Result.ProtocolVersion,
			Raw:        raw,
		}, nil
	}
	return Capabilities{}, fmt.Errorf("driver: acp probe %q: no initialize response", tool)
}

// flattenJSON writes a JSON value into out as deterministic dotted keys:
// objects recurse (".", sorted keys), scalars stringify. Arrays and other
// values stringify via encoding/json. Identical input → identical map.
func flattenJSON(prefix string, raw json.RawMessage, out map[string]string) {
	if len(raw) == 0 {
		return
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return
	}
	flattenValue(prefix, v, out)
}

func flattenValue(key string, val any, out map[string]string) {
	obj, ok := val.(map[string]any)
	if !ok {
		out[key] = scalarString(val)
		return
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		child := k
		if key != "" {
			child = key + "." + k
		}
		flattenValue(child, obj[k], out)
	}
}

// scalarString renders a JSON scalar deterministically.
func scalarString(val any) string {
	switch t := val.(type) {
	case nil:
		return "null"
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}

// firstLine returns the first non-empty, trimmed line of b (CLI version).
func firstLine(b []byte) string {
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}
