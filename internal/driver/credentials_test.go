package driver

import "testing"

// TestCredentialDeclarationsCoverKnownTools is the declaration-set pin: every
// registered adapter tool has a declaration entry (possibly empty), the
// declaration set is exactly the expected one, and every declared name is a
// legal environment-variable name.
func TestCredentialDeclarationsCoverKnownTools(t *testing.T) {
	decls := DeclaredCredentials()

	// Every adapter has a declaration entry, and vice versa.
	for tool := range adapters {
		if _, ok := decls[tool]; !ok {
			t.Errorf("adapter %q has no credential declaration", tool)
		}
	}
	for tool := range decls {
		if _, ok := adapters[tool]; !ok {
			t.Errorf("credential declaration for unknown tool %q", tool)
		}
	}

	want := map[string]string{
		"claude":   "ANTHROPIC_API_KEY",
		"codex":    "OPENAI_API_KEY",
		"opencode": "OPENCODE_API_KEY",
		"gemini":   "GEMINI_API_KEY",
	}
	if len(decls) != len(want) {
		t.Fatalf("declaration set has %d tools, want %d", len(decls), len(want))
	}
	for tool, name := range want {
		got := CredentialsFor(tool)
		if len(got) != 1 || got[0] != name {
			t.Errorf("CredentialsFor(%q) = %v, want [%s]", tool, got, name)
		}
	}
}

// TestDeclaredCredentialsReturnACopy pins that the accessor cannot leak the
// package table: mutating a returned slice leaves the next call unchanged.
func TestDeclaredCredentialsReturnACopy(t *testing.T) {
	first := DeclaredCredentials()
	first["claude"][0] = "TAMPERED"
	first["newtool"] = []string{"X"}

	if got := CredentialsFor("claude"); len(got) != 1 || got[0] != "ANTHROPIC_API_KEY" {
		t.Fatalf("table leaked to caller: claude = %v", got)
	}
	if _, ok := DeclaredCredentials()["newtool"]; ok {
		t.Fatal("map leaked to caller: newtool present")
	}
}
