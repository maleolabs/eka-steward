// Package app is the steward's run surface: it assembles the production seams
// (config, the EKA runtime, the read-only Source, the single write path, the
// ACP worker Dispatcher, the observability Recorder and the TTY-free approval
// channel) into one control loop and drives it to quiescence under the
// single-instance lock.
//
// It is a CONSUMER of eka-core: it imports only github.com/maleolabs/eka-core/v2/api.
// Reads go through api.Knowledge (via loop.StoreSource); the ONLY mutation path
// is internal/ekawrite (via loop.EkawriteWriter), so cmd/boundary_arch_test.go
// stays green — this package names no api.Authoring method and never requests
// an api.Authoring value.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/maleolabs/eka-core/v2/api"
	"github.com/maleolabs/eka-steward/v2/internal/acp"
	"github.com/maleolabs/eka-steward/v2/internal/approval"
	"github.com/maleolabs/eka-steward/v2/internal/config"
	"github.com/maleolabs/eka-steward/v2/internal/driver"
	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
	"github.com/maleolabs/eka-steward/v2/internal/guard"
	"github.com/maleolabs/eka-steward/v2/internal/loop"
	"github.com/maleolabs/eka-steward/v2/internal/obs"
	"github.com/maleolabs/eka-steward/v2/internal/role"
	"github.com/maleolabs/eka-steward/v2/internal/secrets"
)

// lockFileName is the single-instance lock file inside the state directory.
// The lock pins one steward per state dir (docs/control-loop.md §Single-instance
// lock); the file name is local to the run surface and has no doc-pinned value.
const lockFileName = "steward.lock"

// Options is the run surface's operator input. Project is required. A caller
// that injects Dispatcher (and optionally Source) is a test: production leaves
// them nil and supplies ConfigPath + Role.
type Options struct {
	// Project is the EKA project id to drive (required).
	Project string
	// ConfigPath is the steward config document. Required in production (a
	// nil Dispatcher needs it for the driver, roles and loop settings); test
	// injection may omit it.
	ConfigPath string
	// Role is the worker role name to dispatch as (production: required).
	Role string
	// Driver overrides the role's driver tool name.
	Driver string
	// Repo is the repository root the transitions are addressed from (the
	// engine walks up to its eka.yaml).
	Repo string
	// StateDir overrides the steward state directory. Empty resolves the
	// documented precedence (loop.StateDir).
	StateDir string
	// DispatchState is the state a dispatched item moves to through the
	// Writer. Empty disables the write path (plan/dispatch only, no
	// checkpoint). The loop does not re-derive transition legality: the item
	// must be in a state from which the engine permits it.
	DispatchState string
	// Dispatcher is the injectable worker seam (nil = build the ACP driver).
	Dispatcher loop.Dispatcher
	// Source is the injectable read seam (nil = the store-backed StoreSource).
	Source loop.Source
	// LogWriter, when set, receives the structured step log (one JSON record
	// per step). nil disables logging.
	LogWriter io.Writer
	// Crash is the injectable crash-injection seam (nil in production).
	Crash loop.CrashPoint
	// Now is the injectable clock for the observer's step durations.
	Now func() time.Time
}

// App is an assembled run: the wired loop plus the resources it owns.
type App struct {
	rt       api.Runtime
	state    *loop.Store
	loop     *loop.Loop
	recorder *obs.Recorder
	lockPath string
	// stateDir is the steward state directory (the approval channel's
	// directory). It is the dir of lockPath, kept so Approve can materialize a
	// decision into the same file/queue channel the loop polls.
	stateDir string
}

// Assemble wires the run seams from opts. It opens the EKA runtime, builds the
// Source, the single write path, the Dispatcher (unless injected), the Recorder
// and the TTY-free approval channel, and constructs the control loop. It
// performs no dispatch and takes no lock — Run does.
func Assemble(opts Options) (*App, error) {
	if opts.Project == "" {
		return nil, errors.New("app: project is required")
	}

	stateDir := opts.StateDir
	if stateDir == "" {
		d, err := loop.EnsureStateDir()
		if err != nil {
			return nil, err
		}
		stateDir = d
	} else if err := os.MkdirAll(stateDir, 0o755); err != nil {
		return nil, fmt.Errorf("app: create state dir %s: %w", stateDir, err)
	}

	var cfg *config.Config
	if opts.ConfigPath != "" {
		b, err := os.ReadFile(opts.ConfigPath)
		if err != nil {
			return nil, fmt.Errorf("app: read config %s: %w", opts.ConfigPath, err)
		}
		cfg, err = config.ParseTrusted(b)
		if err != nil {
			return nil, err
		}
	}

	// The credential broker: it resolves the selected tool's DECLARED
	// credentials from <stateDir>/secrets.env. It seeds the Recorder's
	// redaction deny-list (so a resolved credential is masked in every log and
	// report byte) without requiring a credential to be configured — a
	// pre-authenticated agent runs with none.
	resolver, err := secrets.NewStateFileResolver(stateDir)
	if err != nil {
		return nil, err
	}
	broker, err := secrets.NewDriverBroker(resolver)
	if err != nil {
		return nil, err
	}

	rt, err := api.Ensure()
	if err != nil {
		return nil, fmt.Errorf("app: open EKA runtime: %w", err)
	}

	src := opts.Source
	if src == nil {
		src = loop.StoreSource{Knowledge: rt.Knowledge(), Project: opts.Project}
	}
	writer := loop.Writer(loop.EkawriteWriter{Writer: ekawrite.NewSteward(rt), RepoPath: opts.Repo})

	approver := approval.FileApprover{Dir: stateDir}

	tool, roleName := "", ""
	disp := opts.Dispatcher
	review := loop.ReviewPolicy{}
	if disp == nil {
		if cfg == nil {
			rt.Close()
			return nil, errors.New("app: a config file is required unless a dispatcher is injected")
		}
		d, t, rn, rv, err := buildDispatcher(opts, cfg, approver, broker)
		if err != nil {
			rt.Close()
			return nil, err
		}
		disp, tool, roleName, review = d, t, rn, rv
	}

	// Resolve the selected tool's credential values for redaction (best
	// effort: a missing credential is not an assemble failure so a
	// pre-authenticated agent still runs).
	var deny []string
	if tool != "" {
		if creds, err := broker.EnvFor(tool); err == nil {
			for _, v := range creds {
				deny = append(deny, v)
			}
		}
	}

	var sink obs.Sink
	if opts.LogWriter != nil {
		sink = obs.NewLogger(opts.LogWriter)
	}
	rec := obs.NewRecorder(obs.Config{Driver: tool, Role: roleName, Secrets: deny}, sink)

	loopCfg := loop.Config{
		MaxWorkers:    1,
		DispatchState: opts.DispatchState,
		Review:        review,
		// The approval channel is the TTY-free file/queue approver; the
		// escalation stage stays OFF until a Request is supplied
		// (ApprovalPolicy.Request nil), so the run never waits on a human by
		// default and never touches a terminal.
		Approval: loop.ApprovalPolicy{
			Approver: approver,
			Policy:   guard.Policy{AllowReadWithinRoot: true},
		},
		Metrics:  rec,
		Observer: rec,
		Crash:    opts.Crash,
		Now:      opts.Now,
	}
	if cfg != nil {
		loopCfg.MaxWorkers = cfg.Sizing.MaxWorkers
		loopCfg.QueueLimit = cfg.Loop.QueueLimit
		loopCfg.AttemptTimeout = time.Duration(cfg.Loop.AttemptTimeout)
		loopCfg.Retry = loop.RetryPolicy{MaxAttempts: cfg.Loop.MaxAttempts}
	}

	st := &loop.Store{Path: filepath.Join(stateDir, loop.CheckpointFile)}
	return &App{
		rt:       rt,
		state:    st,
		loop:     loop.New(src, disp, writer, st, loopCfg),
		recorder: rec,
		lockPath: filepath.Join(stateDir, lockFileName),
		stateDir: stateDir,
	}, nil
}

// buildDispatcher resolves the role's ACP driver from config and builds the
// production Dispatcher (and the review policy the role set implies). It
// returns the selected tool and role names for the Recorder labels.
func buildDispatcher(opts Options, cfg *config.Config, approver approval.FileApprover, broker *secrets.Broker) (loop.Dispatcher, string, string, loop.ReviewPolicy, error) {
	if opts.Role == "" {
		return nil, "", "", loop.ReviewPolicy{}, errors.New("app: role is required")
	}
	roleSet, err := role.ParseRoles(cfg.Roles)
	if err != nil {
		return nil, "", "", loop.ReviewPolicy{}, err
	}
	r, err := roleSet.Resolve(opts.Role)
	if err != nil {
		return nil, "", "", loop.ReviewPolicy{}, err
	}
	tool := opts.Driver
	if tool == "" {
		tool = r.Driver
	}
	reg, err := driver.ParseDrivers(cfg.Drivers)
	if err != nil {
		return nil, "", "", loop.ReviewPolicy{}, err
	}
	spec, err := reg.Resolve(tool)
	if err != nil {
		return nil, "", "", loop.ReviewPolicy{}, err
	}
	if spec.Kind != driver.KindACP {
		return nil, "", "", loop.ReviewPolicy{}, fmt.Errorf("app: driver %q is not an ACP adapter (kind %q)", spec.Tool, spec.Kind)
	}
	// The worker environment: the credential-free base (no HOME/EKA_HOME/PATH)
	// plus ONLY this tool's declared credentials, so the agent authenticates
	// through its driver declaration and inherits nothing else.
	env, err := broker.WorkerEnv(ekawrite.WorkerEnv(), tool)
	if err != nil {
		return nil, "", "", loop.ReviewPolicy{}, err
	}
	d, err := acp.NewDispatcher(acp.DispatcherConfig{
		Driver: spec,
		Dir:    opts.Repo,
		Env:    env,
		Client: acp.Config{
			Info:          acp.ClientInfo{Name: "eka-steward", Version: "dev"},
			Policy:        guard.Policy{AllowReadWithinRoot: true},
			Approver:      approver,
			WorkspaceRoot: opts.Repo,
		},
	})
	if err != nil {
		return nil, "", "", loop.ReviewPolicy{}, err
	}
	review := loop.ReviewPolicy{}
	if len(roleSet.ReviewRoles()) > 0 {
		review = loop.ReviewPolicy{Roles: roleSet, Author: r.Name}
	}
	return d, tool, r.Name, review, nil
}

// Run acquires the single-instance lock and drives the loop to quiescence: it
// reconciles any in-flight intent (Resume), then runs until nothing is
// dispatchable. With QueueLimit == 0 one Run reaches quiescence; with the
// bounded pool it admits repeated admission windows. A stalled window (no
// completion while candidates remain dispatchable) is a deterministic error,
// never an infinite loop. The lock is released when Run returns, including on a
// crash-injection panic.
func (a *App) Run(ctx context.Context) error {
	lock, err := loop.Acquire(a.lockPath)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	done := 0
	for {
		if err := a.loop.Run(ctx); err != nil {
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		st, err := a.state.Load()
		if err != nil {
			return err
		}
		steps, err := a.loop.Plan(ctx)
		if err != nil {
			return err
		}
		if len(steps) == 0 {
			return nil
		}
		if len(st.Done) == done {
			return fmt.Errorf("app: run stalled with %d candidate(s) still dispatchable and no progress", len(steps))
		}
		done = len(st.Done)
	}
}

// Report returns the deterministic run report (steps + EKA writes).
func (a *App) Report() obs.Report { return a.recorder.Report() }

// WriteReport renders the run report to w (deterministic JSON, one line).
func (a *App) WriteReport(w io.Writer) error { return a.Report().Write(w) }

// Status is the deterministic instantaneous view a machine consumer reads: the
// recorder's counters and the accumulated run report. Both are copies, so the
// returned value is stable for the caller.
type Status struct {
	Snapshot obs.Snapshot
	Report   obs.Report
}

// Status returns the current counters and report. It is safe to call
// concurrently with a run in progress (the Recorder is mutex-guarded).
func (a *App) Status() Status {
	return Status{Snapshot: a.recorder.Snapshot(), Report: a.recorder.Report()}
}

// Plan is the thin protocol delegation to the owned loop's pure planning pass:
// the deterministic step sequence from the current source set, skipping work
// already recorded done. It never dispatches. See loop.Plan.
func (a *App) Plan(ctx context.Context) ([]loop.Step, error) { return a.loop.Plan(ctx) }

// Step is the thin protocol delegation to the owned loop's one-step call: it
// plans and runs ONE checkpointed candidate, returning ok=false when nothing is
// dispatchable. See loop.Step.
func (a *App) Step(ctx context.Context) (loop.Step, bool, error) { return a.loop.Step(ctx) }

// Cancel is the thin protocol delegation to the owned loop's batch cancel: it
// cancels every in-flight dispatch of the run in progress. Idempotent and safe
// to call concurrently with Run. See loop.Cancel.
func (a *App) Cancel() { a.loop.Cancel() }

// RunSteps acquires the single-instance lock and drives at most maxSteps
// dispatch steps through the loop's public Resume/Step seams, then returns. A
// non-positive maxSteps drives to quiescence exactly like Run (same lock, same
// driver, same cleanup). A step-capped run is sequential — the bounded queue
// does not apply — because the cap is a per-step count, not a queue window. A
// cancelled ctx is a clean stop (nil), mirroring Run.
func (a *App) RunSteps(ctx context.Context, maxSteps int) error {
	if maxSteps <= 0 {
		return a.Run(ctx)
	}
	lock, err := loop.Acquire(a.lockPath)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	if err := a.loop.Resume(ctx); err != nil {
		return err
	}
	for i := 0; i < maxSteps; i++ {
		if ctx.Err() != nil {
			return nil
		}
		_, ok, err := a.loop.Step(ctx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
	}
	return nil
}

// Approve materializes one human decision for the approval request with the
// given deterministic request id (approval.RequestFor's Key) into the
// file/queue channel the loop polls. It is the consumer side of the approver
// seam: a pending escalation gate observes the response and its bounded wait
// resolves. This never touches the EKA store — the loop records the decision
// through the single write path.
func (a *App) Approve(requestID, decision string) error {
	if requestID == "" {
		return errors.New("app: approve needs a request id")
	}
	var verdict approval.Verdict
	switch decision {
	case string(approval.Approve):
		verdict = approval.Approve
	case string(approval.Deny):
		verdict = approval.Deny
	default:
		return fmt.Errorf("app: unknown decision %q (want approve|deny)", decision)
	}
	return approval.WriteResponse(a.stateDir, approval.Response{Key: requestID, Verdict: verdict})
}

// Close releases the EKA runtime. Idempotent.
func (a *App) Close() error {
	if a.rt == nil {
		return nil
	}
	return a.rt.Close()
}
