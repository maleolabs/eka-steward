package guard

import (
	"errors"
	"fmt"
	"strings"

	"github.com/maleolabs/eka-steward/v2/internal/ekawrite"
	"github.com/maleolabs/eka-steward/v2/internal/secrets"
)

// forbiddenEnvNames are variable names a worker environment must never
// carry: HOME/EKA_HOME would re-expose the default EKA store path, and
// PATH would let a bare-name tool resolve (a worker receives absolute
// paths instead, never a PATH lookup).
var forbiddenEnvNames = []string{"HOME", "EKA_HOME", "PATH"}

// credentialNeedles are substrings that mark a variable as credential-
// bearing; such a variable is refused in a worker environment.
var credentialNeedles = []string{"TOKEN", "KEY", "SECRET", "CREDENTIAL", "PASSWORD", "AUTH"}

// WorkerEnv returns the allow-listed environment a steward worker runs
// with, extended by the caller's extra vars. It REUSES ekawrite.WorkerEnv
// (the single source of the worker environment) and never duplicates it.
//
// Every extra var is checked: HOME, EKA_HOME and PATH are refused, and any
// credential-looking name (TOKEN/KEY/SECRET/CREDENTIAL/PASSWORD/AUTH) is
// refused. A refused var returns an error and no environment, so an unsafe
// extension fails closed.
func WorkerEnv(extra ...string) ([]string, error) {
	env := ekawrite.WorkerEnv()
	for _, kv := range extra {
		if err := checkEnvVar(kv); err != nil {
			return nil, err
		}
	}
	return append(env, extra...), nil
}

// checkEnvVar refuses a variable that would break the worker-environment
// invariants. It names the offending variable.
func checkEnvVar(kv string) error {
	name := kv
	if i := strings.IndexByte(kv, '='); i >= 0 {
		name = kv[:i]
	}
	if name == "" {
		return errors.New("guard: empty environment variable name")
	}
	for _, f := range forbiddenEnvNames {
		if name == f {
			return fmt.Errorf("guard: worker environment may not set %s", name)
		}
	}
	upper := strings.ToUpper(name)
	for _, n := range credentialNeedles {
		if strings.Contains(upper, n) {
			return fmt.Errorf("guard: worker environment may not set credential variable %q", name)
		}
	}
	return nil
}

// WorkerEnvFor returns the worker environment for tool: the credential-free
// allow-listed base (WorkerEnv, never carrying a credential) plus ONLY the
// credentials the tool's driver DECLARES, resolved by the broker. It is the one
// sanctioned way a credential reaches a worker — the base carries none, and the
// broker returns exactly the declared names, never os.Environ(). Tool that has
// no declaration, or is missing a declared credential, fails closed.
func WorkerEnvFor(b *secrets.Broker, tool string) ([]string, error) {
	base, err := WorkerEnv()
	if err != nil {
		return nil, err
	}
	return b.WorkerEnv(base, tool)
}
