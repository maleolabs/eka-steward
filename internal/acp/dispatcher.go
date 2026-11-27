package acp

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/maleolabs/eka-steward/v2/internal/decision"
	"github.com/maleolabs/eka-steward/v2/internal/driver"
	"github.com/maleolabs/eka-steward/v2/internal/escapehatch"
	"github.com/maleolabs/eka-steward/v2/internal/loop"
)

// DispatcherConfig configures a production Dispatcher.
type DispatcherConfig struct {
	// Driver is the resolved ACP driver to launch. Its Kind must be
	// driver.KindACP and its Cmd is the escape-hatch allow-listed executable.
	Driver driver.Spec
	// Client is the per-connection client configuration (version, policy,
	// approval channel, workspace root).
	Client Config
	// Dir is the agent's working directory and the session cwd ("" = the
	// dispatcher process's directory).
	Dir string
	// Env is the child environment handed to the agent. A nil Env means an
	// EMPTY environment (escapehatch's contract), never the parent's. Callers
	// pass the credential-free worker base plus only the driver-declared
	// credentials (secrets.Broker.WorkerEnv), so no ambient variable leaks.
	Env []string
	// Prompt builds the instruction for a candidate. nil uses the candidate
	// ID. It is a pure mapping: the dispatcher makes no control decision.
	Prompt func(decision.Candidate) string
}

// Dispatcher is the production loop.Dispatcher: it launches the configured
// ACP driver through internal/escapehatch and drives one prompt turn per
// candidate, returning the streamed agent output. It performs NO EKA writes —
// the loop's Writer owns them — so a cancel leaves no partial write behind.
type Dispatcher struct {
	allow  escapehatch.AllowList
	spec   escapehatch.Spec
	cfg    Config
	dir    string
	prompt func(decision.Candidate) string
}

// NewDispatcher builds a Dispatcher. A non-ACP driver or an empty command is a
// deterministic error.
func NewDispatcher(dc DispatcherConfig) (*Dispatcher, error) {
	if dc.Driver.Kind != driver.KindACP {
		return nil, fmt.Errorf("acp: driver %q is not an ACP adapter (kind %q)", dc.Driver.Tool, dc.Driver.Kind)
	}
	if dc.Driver.Cmd == "" {
		return nil, fmt.Errorf("acp: driver %q has no command", dc.Driver.Tool)
	}
	prompt := dc.Prompt
	if prompt == nil {
		prompt = func(c decision.Candidate) string { return c.ID }
	}
	cfg := dc.Client
	if cfg.WorkspaceRoot == "" {
		// Default the session workspace root to the agent's cwd so an
		// AllowReadWithinRoot policy protects the directory the session runs
		// in without the caller restating it.
		cfg.WorkspaceRoot = dc.Dir
	}
	return &Dispatcher{
		allow:  escapehatch.NewAllowList(filepath.Base(dc.Driver.Cmd)),
		spec:   escapehatch.Spec{Name: dc.Driver.Cmd, Args: dc.Driver.Args, Dir: dc.Dir, Env: dc.Env},
		cfg:    cfg,
		dir:    dc.Dir,
		prompt: prompt,
	}, nil
}

// compile-time checks: a Dispatcher satisfies both worker seams.
var (
	_ loop.Dispatcher       = (*Dispatcher)(nil)
	_ loop.ReviewDispatcher = (*Dispatcher)(nil)
)

// Dispatch spawns the agent, negotiates, opens a session and runs one prompt
// turn for c, returning the agent-message output. A cancel or failure returns
// an error (the loop rolls back its intent; no EKA write happens here).
func (d *Dispatcher) Dispatch(ctx context.Context, c decision.Candidate) (loop.DispatchResult, error) {
	out, err := d.turn(ctx, d.prompt(c))
	if err != nil {
		return loop.DispatchResult{}, err
	}
	return loop.DispatchResult{CandidateID: c.ID, Output: out}, nil
}

// DispatchReview runs the review stage as a second prompt turn against the
// same driver. The reviewer must emit a line "VERDICT: approve" or
// "VERDICT: changes-requested"; anything else is changes-requested (never
// approve by accident).
func (d *Dispatcher) DispatchReview(ctx context.Context, req loop.ReviewRequest) (loop.ReviewOutcome, error) {
	out, err := d.turn(ctx, reviewInstruction(req))
	if err != nil {
		return loop.ReviewOutcome{}, err
	}
	return parseVerdict(out), nil
}

// turn launches one fresh agent, runs one prompt, and returns the output. The
// process is killed on return, so no turn outlives its Dispatch.
func (d *Dispatcher) turn(ctx context.Context, instruction string) (string, error) {
	proc, err := d.allow.Start(ctx, d.spec)
	if err != nil {
		return "", fmt.Errorf("acp: start %s: %w", d.spec.Name, err)
	}
	defer func() { _ = proc.Kill() }()
	go func() { _, _ = io.Copy(io.Discard, proc.Stderr()) }()

	cli := NewClient(Conn{Reader: proc.Stdout(), Writer: proc.Stdin()}, d.cfg)
	cli.Start()
	defer func() { _ = cli.Close() }()

	if _, err := cli.Initialize(ctx); err != nil {
		return "", err
	}
	sid, err := cli.SessionNew(ctx, d.dir)
	if err != nil {
		return "", err
	}
	res, err := cli.Prompt(ctx, sid, instruction)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// reviewInstruction builds the deterministic review prompt.
func reviewInstruction(req loop.ReviewRequest) string {
	return fmt.Sprintf(
		"You are the reviewer %q. The author %q produced the output below for candidate %q.\n"+
			"Reply with exactly one verdict line: \"VERDICT: approve\" or \"VERDICT: changes-requested\".\n\n"+
			"--- AUTHOR OUTPUT ---\n%s\n--- END AUTHOR OUTPUT ---",
		req.Reviewer, req.Author, req.Candidate.ID, req.Output,
	)
}

// parseVerdict extracts the verdict marker from reviewer output. A missing or
// unrecognised marker is changes-requested (safe default, never approve).
func parseVerdict(out string) loop.ReviewOutcome {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && strings.EqualFold(fields[0], "VERDICT:") {
			switch strings.ToLower(strings.TrimSpace(fields[1])) {
			case loop.VerdictApprove:
				return loop.ReviewOutcome{Verdict: loop.VerdictApprove}
			case loop.VerdictChangesRequested:
				return loop.ReviewOutcome{Verdict: loop.VerdictChangesRequested}
			}
		}
	}
	return loop.ReviewOutcome{Verdict: loop.VerdictChangesRequested}
}
