// Package secrets is the steward's credential broker: it resolves ONLY the
// credential names a driver DECLARES into a minimal environment, from an
// injected resolver.
//
// Least privilege is the whole point. The steward NEVER blankets a worker with
// os.Environ(): the broker reads exactly the declared names from its Resolver
// and returns exactly those, so GH_TOKEN, SSH_AUTH_SOCK, AWS_* and HOME are
// absent unless a tool declares them. missing/empty required credential is a
// deterministic error naming the tool and the variable.
//
// The broker also carries the DECLARATIVE egress allow-list ([EgressPolicy])
// and the SCOPED git credentials for the steward's OWN remote operations. See
// docs/secrets.md for the model and the honest limits (the steward cannot
// enforce network egress without a sandbox — the policy is a validated,
// testable description a sandbox enforces).
//
// This package imports no os/exec and no eka-core package: it is transport of
// credential VALUES into an env map, nothing more.
package secrets

import (
	"errors"
	"fmt"
	"sort"

	"github.com/maleolabs/eka-steward/v2/internal/driver"
	"github.com/maleolabs/eka-steward/v2/internal/obs"
)

// ErrNoCredential is returned (wrapped) when a declared credential is not
// available from the resolver. Test with errors.Is.
var ErrNoCredential = errors.New("secrets: declared credential is not available")

// Resolver resolves a credential NAME to its value. It is the ONLY source the
// broker reads: os.Environ() is never consulted.
type Resolver interface {
	// Get returns the value for name and whether it is available. A
	// present-but-empty value counts as unavailable to the broker.
	Get(name string) (string, bool)
}

// MapResolver is an in-memory Resolver (tests, injected maps).
type MapResolver map[string]string

// Get implements Resolver.
func (m MapResolver) Get(name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}

// Broker resolves the declared credentials for a driver. It is immutable after
// construction, so it is safe to share.
type Broker struct {
	decls    map[string][]string // tool -> sorted declared names
	resolver Resolver
}

// NewBroker builds a Broker over the given declarations and resolver. Every
// declaration name must be a valid, non-empty environment-variable name; the
// declarations are copied and sorted, so the broker is immutable and every
// lookup is deterministic. A nil resolver is refused.
func NewBroker(resolver Resolver, decls map[string][]string) (*Broker, error) {
	if resolver == nil {
		return nil, errors.New("secrets: nil resolver")
	}
	frozen := make(map[string][]string, len(decls))
	for tool, names := range decls {
		if tool == "" {
			return nil, errors.New("secrets: declaration for empty tool name")
		}
		cp := append([]string(nil), names...)
		sort.Strings(cp)
		for _, n := range cp {
			if !validEnvName(n) {
				return nil, fmt.Errorf("secrets: invalid credential name %q for tool %q", n, tool)
			}
		}
		frozen[tool] = cp
	}
	return &Broker{decls: frozen, resolver: resolver}, nil
}

// NewDriverBroker builds a Broker from the driver package's fixed credential
// declaration table. It is the standard wiring: the declarations come from the
// registry, the values from the injected resolver.
func NewDriverBroker(resolver Resolver) (*Broker, error) {
	return NewBroker(resolver, driver.DeclaredCredentials())
}

// EnvFor resolves exactly the credentials tool declares into a name->value map.
// It NEVER returns a key outside the declaration and NEVER consults
// os.Environ(). A missing or empty declared value is a deterministic error
// naming the tool and the variable; a tool with no declaration at all is
// refused.
func (b *Broker) EnvFor(tool string) (map[string]string, error) {
	names, ok := b.decls[tool]
	if !ok {
		return nil, fmt.Errorf("secrets: no credential declaration for tool %q", tool)
	}
	out := make(map[string]string, len(names))
	for _, n := range names {
		v, ok := b.resolver.Get(n)
		if !ok || v == "" {
			return nil, fmt.Errorf("%w: tool %q requires %s", ErrNoCredential, tool, n)
		}
		out[n] = v
	}
	return out, nil
}

// WorkerEnv composes the worker environment for tool: base (the caller's
// credential-free worker environment, e.g. guard.WorkerEnv()) plus ONLY tool's
// declared credentials, as sorted "NAME=value" entries. The result is
// deterministic (ordered, no map iteration).
func (b *Broker) WorkerEnv(base []string, tool string) ([]string, error) {
	creds, err := b.EnvFor(tool)
	if err != nil {
		return nil, err
	}
	return append(append([]string(nil), base...), EnvSlice(creds)...), nil
}

// RedactorFor returns an obs.Redactor seeded with the resolved values of tool's
// declared credentials, so a transcript logged for that tool masks exactly the
// secrets the worker holds. A missing credential is the same deterministic
// error as EnvFor.
func (b *Broker) RedactorFor(tool string) (*obs.Redactor, error) {
	creds, err := b.EnvFor(tool)
	if err != nil {
		return nil, err
	}
	vals := make([]string, 0, len(creds))
	for _, v := range creds {
		vals = append(vals, v)
	}
	return obs.NewRedactor(vals...), nil
}

// EnvSlice renders a name->value map as sorted "NAME=value" entries, so the
// result is deterministic (no map iteration order can leak into the env).
func EnvSlice(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// validEnvName reports whether name is a legal environment-variable name:
// [A-Za-z_][A-Za-z0-9_]*.
func validEnvName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}
