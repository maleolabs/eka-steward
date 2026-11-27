package ekawrite

// WorkerEnv returns the allow-listed environment a steward worker subprocess
// runs with. It is deliberately minimal and contains:
//
//   - no HOME: so the worker cannot resolve <home>/.eka (the default EKA
//     workspace location);
//   - no EKA_HOME: so the worker cannot be pointed at the steward's
//     workspace either;
//   - no PATH: so a bare-name `eka` lookup cannot resolve an authoring CLI;
//   - no token/secret variables (nothing matching TOKEN, KEY, SECRET,
//     CREDENTIAL, PASSWORD, AUTH): so the worker inherits no credential.
//
// The returned slice is the exact environment to hand to a child process; it
// is a fresh slice the caller may extend with the absolute paths it wants the
// worker to reach (a worker that must run a binary receives its absolute path,
// never a PATH lookup).
//
// What this can and cannot guarantee is stated in docs/write-path.md: an
// environment cannot stop a process from executing a binary it was handed by
// absolute path — that is a sandbox concern outside the environment's control.
// What it guarantees is that a worker with this environment has no default
// EKA-home path and no bare-name EKA tool on its search path, so the store is
// not reachable through the ambient environment.
func WorkerEnv() []string {
	return []string{
		"TZ=UTC",
		"LANG=C",
		"LC_ALL=C",
	}
}

// WorkerHomeUnresolvable reports whether, under the given environment, the
// EKA workspace root cannot be resolved the way workspace.HomeDir does:
// EKA_HOME (absolute) when set, else <user home>/.eka. It returns true when
// neither EKA_HOME nor HOME is present — the WorkerEnv property that keeps a
// worker off the default store path.
func WorkerHomeUnresolvable(env []string) bool {
	vars := envMap(env)
	if ekaHome := vars["EKA_HOME"]; ekaHome != "" {
		return false
	}
	return vars["HOME"] == ""
}

// envMap indexes an env slice by variable name (last occurrence wins, the
// same as the OS).
func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				m[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	return m
}
