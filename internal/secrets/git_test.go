package secrets

import (
	"errors"
	"strings"
	"testing"
)

// TestGitCredsAreScopedAndHostFree is AC4: the steward git-op env carries the
// scoped credential and NOT the host GH_TOKEN/SSH_AUTH_SOCK.
func TestGitCredsAreScopedAndHostFree(t *testing.T) {
	t.Setenv("GH_TOKEN", "ghp_host")
	t.Setenv("SSH_AUTH_SOCK", "/run/host/agent.sock")

	resolver := MapResolver{
		GitAskPassVar:   "/usr/local/libexec/eka-askpass",
		GitTokenVar:     "scoped-token-xyz",
		"GH_TOKEN":      "ghp_host",
		"SSH_AUTH_SOCK": "/run/host/agent.sock",
	}
	env, err := mustBroker(t, resolver).GitCreds("origin")
	if err != nil {
		t.Fatalf("GitCreds: %v", err)
	}
	names := nameSet(env)
	for _, want := range []string{GitAskPassVar, GitTokenVar, GitRemoteVar} {
		if !names[want] {
			t.Errorf("scoped git env missing %q: %v", want, env)
		}
	}
	for _, bad := range []string{"GH_TOKEN", "SSH_AUTH_SOCK", "HOME"} {
		if names[bad] {
			t.Errorf("scoped git env leaked host var %q: %v", bad, env)
		}
	}
	// The remote the token is bound to is recorded, so scope is observable.
	if got := valueOf(env, GitRemoteVar); got != "origin" {
		t.Errorf("%s = %q, want %q", GitRemoteVar, got, "origin")
	}
}

// TestGitCredsMissingScopedCredentialIsDeterministicError: no token -> fail.
func TestGitCredsMissingScopedCredentialIsDeterministicError(t *testing.T) {
	_, err := mustBroker(t, MapResolver{GitAskPassVar: "/helper"}).GitCreds("origin")
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("err = %v, want ErrNoCredential", err)
	}
	if !strings.Contains(err.Error(), GitTokenVar) {
		t.Errorf("error %q does not name %q", err.Error(), GitTokenVar)
	}
}

// valueOf returns the value of name in env.
func valueOf(env []string, name string) string {
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i >= 0 && kv[:i] == name {
			return kv[i+1:]
		}
	}
	return ""
}
