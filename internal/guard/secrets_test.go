package guard

import (
	"strings"
	"testing"

	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
	"github.com/maleolabs/eka-steward/v2/internal/secrets"
)

// hostSecrets are ambient host variables a worker must NEVER inherit.
var hostSecrets = []string{"GH_TOKEN", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "HOME"}

func brokerFor(t *testing.T, r secrets.Resolver) *secrets.Broker {
	t.Helper()
	b, err := secrets.NewDriverBroker(r)
	if err != nil {
		t.Fatalf("NewDriverBroker: %v", err)
	}
	return b
}

// TestWorkerEnvForAddsOnlyDeclaredCredentials is AC1/AC2: the worker env is the
// credential-free base plus ONLY the driver's declared credentials; every host
// secret and every other tool's credential is absent.
func TestWorkerEnvForAddsOnlyDeclaredCredentials(t *testing.T) {
	for _, s := range hostSecrets {
		t.Setenv(s, "host-"+s)
	}
	b := brokerFor(t, secrets.MapResolver{
		"ANTHROPIC_API_KEY":     "sk-ant-AAA",
		"OPENAI_API_KEY":        "sk-openai-BBB",
		"GH_TOKEN":              "ghp_host",
		"SSH_AUTH_SOCK":         "/run/host/agent.sock",
		"AWS_SECRET_ACCESS_KEY": "aws-secret",
		"HOME":                  "/home/host",
	})

	env, err := WorkerEnvFor(b, "claude")
	if err != nil {
		t.Fatalf("WorkerEnvFor(claude): %v", err)
	}
	names := nameSet(env)
	if !names["ANTHROPIC_API_KEY"] {
		t.Fatalf("worker env missing declared credential: %v", env)
	}
	for _, bad := range []string{
		"OPENAI_API_KEY", // declared for codex, NOT claude
		"GH_TOKEN", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "HOME",
		"EKA_HOME", "PATH",
	} {
		if names[bad] {
			t.Errorf("worker env leaked %q: %v", bad, env)
		}
	}
	// The base invariant still holds: no home resolvable.
	if !ekawrite.WorkerHomeUnresolvable(env) {
		t.Error("worker env must still leave the EKA home unresolvable")
	}
}

// TestRemoteGitEnvUsesScopedCredentials is AC4: the steward's own git remote op
// env carries the scoped credential and NOT the host GH_TOKEN/SSH_AUTH_SOCK.
func TestRemoteGitEnvUsesScopedCredentials(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_host")
	t.Setenv("SSH_AUTH_SOCK", "/run/host/agent.sock")

	b := brokerFor(t, secrets.MapResolver{
		secrets.GitAskPassVar: "/usr/local/libexec/eka-askpass",
		secrets.GitTokenVar:   "scoped-token-xyz",
		"GH_TOKEN":            "ghp_host",
		"SSH_AUTH_SOCK":       "/run/host/agent.sock",
	})
	env, err := RemoteGitEnv(b, "origin")
	if err != nil {
		t.Fatalf("RemoteGitEnv: %v", err)
	}
	names := nameSet(env)
	for _, want := range []string{secrets.GitAskPassVar, secrets.GitTokenVar, secrets.GitRemoteVar, "GIT_TERMINAL_PROMPT", "GIT_CONFIG_NOSYSTEM"} {
		if !names[want] {
			t.Errorf("remote git env missing %q: %v", want, env)
		}
	}
	for _, bad := range []string{"GH_TOKEN", "SSH_AUTH_SOCK", "HOME", "PATH"} {
		if names[bad] {
			t.Errorf("remote git env leaked host var %q: %v", bad, env)
		}
	}
}

// TestRemoteGitEnvMissingScopedCredentialFails: no scoped token -> fail closed.
func TestRemoteGitEnvMissingScopedCredentialFails(t *testing.T) {
	if _, err := RemoteGitEnv(brokerFor(t, secrets.MapResolver{}), "origin"); err == nil {
		t.Fatal("RemoteGitEnv with no scoped credential: want error")
	}
}

// TestWorkerEnvForMissingDeclaredCredentialFails: fail closed.
func TestWorkerEnvForMissingDeclaredCredentialFails(t *testing.T) {
	if _, err := WorkerEnvFor(brokerFor(t, secrets.MapResolver{}), "claude"); err == nil {
		t.Fatal("WorkerEnvFor without the declared credential: want error")
	}
}

// nameSet indexes env entries by variable name.
func nameSet(env []string) map[string]bool {
	m := make(map[string]bool, len(env))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		m[name] = true
	}
	return m
}
