package driver

// Credential declarations. A driver tells the steward, declaratively, the
// environment-variable NAMES its tool needs. The names are the WHOLE
// declaration: nothing else is implied, and a driver that needs no credential
// declares an empty list.
//
// This table is the steward's least-privilege source of truth for what a
// worker may hold. internal/secrets resolves exactly these names — never
// os.Environ() — so a worker receives ONLY what its tool declares.
//
// The set is per TOOL (a tool uniquely resolves to one adapter). A tool with
// more than one possible model provider is out of scope: it declares the single
// key for the endpoint it is configured against. Extend this table deliberately
// when a tool's credential need changes.
var credentialDecls = map[string][]string{
	"claude":   {"ANTHROPIC_API_KEY"},
	"codex":    {"OPENAI_API_KEY"},
	"opencode": {"OPENCODE_API_KEY"},
	"gemini":   {"GEMINI_API_KEY"},
}

// DeclaredCredentials returns a deep copy of the fixed credential declaration
// table, keyed by tool. Deterministic: the same table every call, and the
// caller cannot mutate the package table.
func DeclaredCredentials() map[string][]string {
	out := make(map[string][]string, len(credentialDecls))
	for tool, names := range credentialDecls {
		out[tool] = append([]string(nil), names...)
	}
	return out
}

// CredentialsFor returns the declared credential names for tool (a copy), or
// nil when the tool declares none (or is unknown).
func CredentialsFor(tool string) []string {
	return append([]string(nil), credentialDecls[tool]...)
}
