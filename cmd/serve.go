package cmd

import (
	"net"
	"os"

	"github.com/maleolabs/eka-steward/v2/internal/app"
	"github.com/maleolabs/eka-steward/v2/internal/protocol"
	"github.com/spf13/cobra"
)

// newServeCommand builds `eka-steward serve`: it hosts the versioned machine
// protocol (JSON-RPC 2.0 over newline-delimited framing) on stdio by default,
// or on a local unix socket with --socket. Every method dispatches to the same
// assembled control loop `run` uses (internal/app) — the protocol adds no second
// loop wiring. A `steward/shutdown` request stops the server gracefully.
func newServeCommand() *cobra.Command {
	var opts app.Options
	var socket string
	var stdio bool

	c := &cobra.Command{
		Use:   "serve",
		Short: "Host the versioned machine protocol (JSON-RPC 2.0 over stdio or a unix socket)",
		Long: `Host the steward machine protocol so any language-agnostic consumer can
drive the steward: initialize, plan, step, run, resume, cancel, approve, status,
shutdown. Requests are JSON-RPC 2.0, one object per line; responses are
deterministic and idempotency keys make retries safe.

Transport is stdio by default (stdin=requests, stdout=responses, stderr=
diagnostics) or a local unix socket with --socket <path>. Every method dispatches
to the same control loop ` + "`run`" + ` drives; the protocol never wires a second
loop. A steward/shutdown request stops the server gracefully.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			opts.LogWriter = c.ErrOrStderr()
			a, err := app.Assemble(opts)
			if err != nil {
				return err
			}
			defer a.Close()

			srv := protocol.NewServer(a)
			if socket != "" {
				return serveSocket(srv, socket)
			}
			_ = stdio // stdio is the default; the flag is an accepted alias.
			return srv.Serve(c.InOrStdin(), c.OutOrStdout())
		},
	}

	f := c.Flags()
	f.StringVar(&opts.Project, "project", "", "EKA project id to drive (required)")
	f.StringVar(&opts.ConfigPath, "config", "", "path to the steward config file (required in production)")
	f.StringVar(&opts.Driver, "driver", "", "driver tool name (overrides the role's driver)")
	f.StringVar(&opts.Role, "role", "", "worker role name to dispatch as (required)")
	f.StringVar(&opts.Repo, "repo", "", "repository root the transitions are addressed from (walks up to its eka.yaml)")
	f.StringVar(&opts.StateDir, "state-dir", "", "steward state directory (default: $EKA_STEWARD_STATE_DIR, $EKA_HOME/steward, ~/.eka/steward)")
	f.StringVar(&opts.DispatchState, "dispatch-state", "todo", "state a dispatched item moves to (empty disables the write path)")
	f.StringVar(&socket, "socket", "", "host the protocol on a local unix socket at this path instead of stdio")
	f.BoolVar(&stdio, "stdio", false, "host the protocol on stdio (the default; accepted as an explicit alias)")
	_ = c.MarkFlagRequired("project")

	return c
}

// serveSocket hosts the protocol on a local unix socket, serving one connection
// at a time on the same server (so a reconnecting client keeps the session). It
// returns when a connection requested shutdown or the listener fails.
func serveSocket(srv *protocol.Server, path string) error {
	// Remove a stale socket file from a previous run; the listener owns the
	// path from here.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer ln.Close()

	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		serr := srv.Serve(conn, conn)
		conn.Close()
		if srv.Stopped() {
			return nil
		}
		if serr != nil {
			return serr
		}
	}
}
