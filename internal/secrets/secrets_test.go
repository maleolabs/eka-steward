package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// hostSecrets are the ambient host variables a worker must NEVER inherit.
var hostSecrets = []string{
	"GH_TOKEN", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "HOME",
}

// richResolver provides every declared value plus a pile of foreign host
// secrets, so a test can prove the broker returns ONLY what is declared.
func richResolver() MapResolver {
	return MapResolver{
		"ANTHROPIC_API_KEY": "sk-ant-AAA",
		"OPENAI_API_KEY":    "sk-openai-BBB",
		"OPENCODE_API_KEY":  "opencode-CCC",
		"GEMINI_API_KEY":    "AIza-GGG",
		// Foreign host secrets that must never be returned:
		"GH_TOKEN":              "ghp_host",
		"SSH_AUTH_SOCK":         "/run/host/agent.sock",
		"AWS_SECRET_ACCESS_KEY": "aws-secret",
		"HOME":                  "/home/host",
	}
}

func mustBroker(t *testing.T, r Resolver) *Broker {
	t.Helper()
	b, err := NewDriverBroker(r)
	if err != nil {
		t.Fatalf("NewDriverBroker: %v", err)
	}
	return b
}

// TestBrokerReturnsExactlyDeclaredSet is AC1: the broker returns exactly the
// declared names for a driver — a foreign secret name is absent.
func TestBrokerReturnsExactlyDeclaredSet(t *testing.T) {
	b := mustBroker(t, richResolver())
	got, err := b.EnvFor("claude")
	if err != nil {
		t.Fatalf("EnvFor(claude): %v", err)
	}
	want := map[string]string{"ANTHROPIC_API_KEY": "sk-ant-AAA"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("EnvFor(claude) = %v, want exactly %v", got, want)
	}
	for _, foreign := range []string{"OPENAI_API_KEY", "OPENCODE_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "HOME"} {
		if _, ok := got[foreign]; ok {
			t.Errorf("EnvFor(claude) leaked foreign secret %q", foreign)
		}
	}
}

// TestBrokerMissingCredentialIsDeterministicError is AC1: a missing required
// credential is a deterministic error naming the driver and the variable.
func TestBrokerMissingCredentialIsDeterministicError(t *testing.T) {
	b := mustBroker(t, MapResolver{}) // nothing available
	_, err := b.EnvFor("claude")
	if err == nil {
		t.Fatal("EnvFor(claude) with no credential: want error")
	}
	if !errors.Is(err, ErrNoCredential) {
		t.Fatalf("error %v does not wrap ErrNoCredential", err)
	}
	for _, want := range []string{"claude", "ANTHROPIC_API_KEY"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err.Error(), want)
		}
	}
	// A present-but-empty value is unavailable.
	if _, err := mustBroker(t, MapResolver{"ANTHROPIC_API_KEY": ""}).EnvFor("claude"); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("empty credential: err = %v, want ErrNoCredential", err)
	}
}

// TestBrokerUnknownToolIsRefused: a tool with no declaration at all fails.
func TestBrokerUnknownToolIsRefused(t *testing.T) {
	if _, err := mustBroker(t, richResolver()).EnvFor("nope"); err == nil {
		t.Fatal("EnvFor(unknown tool): want error")
	}
}

// TestBrokerIgnoresHostEnvironment is AC2: the broker reads only its resolver,
// never os.Environ().
func TestBrokerIgnoresHostEnvironment(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "host-value")
	// No resolver value -> the broker must NOT fall back to the host.
	if _, err := mustBroker(t, MapResolver{}).EnvFor("claude"); !errors.Is(err, ErrNoCredential) {
		t.Fatalf("broker consulted os.Environ(): err = %v, want ErrNoCredential", err)
	}
	// A resolver value wins over the host value.
	got, err := mustBroker(t, MapResolver{"ANTHROPIC_API_KEY": "declared-value"}).EnvFor("claude")
	if err != nil || got["ANTHROPIC_API_KEY"] != "declared-value" {
		t.Fatalf("EnvFor = %v, %v; want declared-value", got, err)
	}
}

// TestWorkerEnvAddsOnlyDeclaredCredentials is AC1/AC2: the composed worker env
// is the credential-free base plus ONLY the declared keys.
func TestWorkerEnvAddsOnlyDeclaredCredentials(t *testing.T) {
	for _, s := range hostSecrets {
		t.Setenv(s, "host-"+s)
	}
	base := []string{"TZ=UTC", "LANG=C", "LC_ALL=C"}
	env, err := mustBroker(t, richResolver()).WorkerEnv(base, "claude")
	if err != nil {
		t.Fatalf("WorkerEnv: %v", err)
	}
	names := nameSet(env)
	if !names["ANTHROPIC_API_KEY"] {
		t.Fatalf("worker env missing declared credential: %v", env)
	}
	for _, bad := range []string{"OPENAI_API_KEY", "OPENCODE_API_KEY", "GEMINI_API_KEY", "GH_TOKEN", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "HOME", "EKA_HOME", "PATH"} {
		if names[bad] {
			t.Errorf("worker env leaked %q: %v", bad, env)
		}
	}
}

// TestEnvIsDeterministic: the same inputs yield the same sorted env across runs.
func TestEnvIsDeterministic(t *testing.T) {
	b := mustBroker(t, richResolver())
	env, err := b.EnvFor("opencode")
	if err != nil {
		t.Fatal(err)
	}
	first := EnvSlice(env)
	for i := 0; i < 100; i++ {
		again, err := b.EnvFor("opencode")
		if err != nil || !reflect.DeepEqual(again, env) {
			t.Fatalf("EnvFor drifted: %v vs %v (%v)", again, env, err)
		}
		if got := EnvSlice(again); !reflect.DeepEqual(got, first) {
			t.Fatalf("EnvSlice drifted: %v vs %v", got, first)
		}
	}
	if !sort.StringsAreSorted(first) {
		t.Fatalf("EnvSlice not sorted: %v", first)
	}
}

// TestNewBrokerRejectsBadDeclaration: invalid declarations fail closed.
func TestNewBrokerRejectsBadDeclaration(t *testing.T) {
	if _, err := NewBroker(nil, nil); err == nil {
		t.Error("nil resolver accepted")
	}
	if _, err := NewBroker(MapResolver{}, map[string][]string{"": {"X"}}); err == nil {
		t.Error("empty tool name accepted")
	}
	if _, err := NewBroker(MapResolver{}, map[string][]string{"t": {"bad name"}}); err == nil {
		t.Error("invalid credential name accepted")
	}
}

// TestRedactorForMasksResolvedValues: obs.Redactor is reused so a logged
// transcript hides the exact secret values.
func TestRedactorForMasksResolvedValues(t *testing.T) {
	b := mustBroker(t, richResolver())
	red, err := b.RedactorFor("claude")
	if err != nil {
		t.Fatal(err)
	}
	line := "calling model with sk-ant-AAA in the header"
	if got := red.Redact(line); strings.Contains(got, "sk-ant-AAA") {
		t.Fatalf("redactor leaked the secret: %q", got)
	}
}

// TestFileResolver covers the StateDir dotenv resolver.
func TestFileResolver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets.env")
	body := "# comment\n\nANTHROPIC_API_KEY=sk-ant-file\nOPENAI_API_KEY = sk-openai-file \n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := NewFileResolver(path)
	if err != nil {
		t.Fatalf("NewFileResolver: %v", err)
	}
	if v, ok := r.Get("ANTHROPIC_API_KEY"); !ok || v != "sk-ant-file" {
		t.Fatalf("Get(ANTHROPIC_API_KEY) = %q, %v", v, ok)
	}
	if v, ok := r.Get("OPENAI_API_KEY"); !ok || v != "sk-openai-file" {
		t.Fatalf("Get(OPENAI_API_KEY) = %q, %v", v, ok)
	}
	// End to end: the file resolver drives the broker.
	if got, err := mustBroker(t, r).EnvFor("claude"); err != nil || got["ANTHROPIC_API_KEY"] != "sk-ant-file" {
		t.Fatalf("broker over file resolver = %v, %v", got, err)
	}

	// Missing file: no credentials, no error.
	if r, err := NewFileResolver(filepath.Join(dir, "absent.env")); err != nil {
		t.Fatalf("missing file errored: %v", err)
	} else if _, ok := r.Get("ANTHROPIC_API_KEY"); ok {
		t.Fatal("missing file yielded a credential")
	}

	// Malformed line: deterministic error.
	if err := os.WriteFile(path, []byte("not a kv line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileResolver(path); err == nil {
		t.Fatal("malformed file accepted")
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
