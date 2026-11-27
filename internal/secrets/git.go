package secrets

import "fmt"

// The steward's OWN git remote operations (e.g. `git push`) must not ride on the
// host's credentials. They authenticate with a SCOPED, broker-provided token:
// git is pointed at an askpass helper (GIT_ASKPASS) that reads a token bound to
// one configured remote. The token is resolved from the broker — never
// os.Environ() — so the host GH_TOKEN / SSH_AUTH_SOCK are not involved.
//
// This is the plumbing only: it produces the environment for a git remote op
// deterministically. No network call is made or assumed here.
const (
	// ScopedGitTool names the steward actor's own git credential declaration.
	// It is not a driver.
	ScopedGitTool = "steward-git"
	// GitAskPassVar names the askpass helper git invokes for a remote prompt.
	GitAskPassVar = "GIT_ASKPASS"
	// GitTokenVar carries the scoped token the askpass helper reads.
	GitTokenVar = "EKA_STEWARD_GIT_TOKEN"
	// GitRemoteVar records the remote the scoped token is bound to, so the
	// helper only ever answers for that remote.
	GitRemoteVar = "EKA_STEWARD_GIT_REMOTE"
)

// GitCreds resolves the scoped git credentials for remote as sorted
// "NAME=value" entries: the askpass helper and the scoped token (both required)
// plus the remote the token is bound to. It never returns GH_TOKEN or
// SSH_AUTH_SOCK and never consults os.Environ(). A missing helper or token is a
// deterministic error.
func (b *Broker) GitCreds(remote string) ([]string, error) {
	ask, ok := b.resolver.Get(GitAskPassVar)
	if !ok || ask == "" {
		return nil, fmt.Errorf("%w: scoped git requires %s", ErrNoCredential, GitAskPassVar)
	}
	tok, ok := b.resolver.Get(GitTokenVar)
	if !ok || tok == "" {
		return nil, fmt.Errorf("%w: scoped git requires %s", ErrNoCredential, GitTokenVar)
	}
	return EnvSlice(map[string]string{
		GitAskPassVar: ask,
		GitTokenVar:   tok,
		GitRemoteVar:  remote,
	}), nil
}
