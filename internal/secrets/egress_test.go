package secrets

import "testing"

// TestDefaultEgressPolicyIsValidAndAllowsModelEndpoints is AC3: the default
// model-endpoint allow-list validates and allows the endpoints, while a
// disallowed host is refused.
func TestDefaultEgressPolicyIsValidAndAllowsModelEndpoints(t *testing.T) {
	p := DefaultEgressPolicy()
	if err := p.Validate(); err != nil {
		t.Fatalf("DefaultEgressPolicy().Validate() = %v", err)
	}
	for _, host := range []string{"api.anthropic.com", "api.openai.com", "generativelanguage.googleapis.com", "opencode.ai"} {
		if !p.Allowed(host) {
			t.Errorf("Allowed(%q) = false, want true", host)
		}
	}
	for _, host := range []string{"evil.example.com", "169.254.169.254", "api.anthropic.com.evil.com", ""} {
		if p.Allowed(host) {
			t.Errorf("Allowed(%q) = true, want false", host)
		}
	}
}

// TestEgressMatchingRule pins the documented rule: exact host, and a
// leading-dot entry matches the domain itself and any subdomain, case-insensitive.
func TestEgressMatchingRule(t *testing.T) {
	p := EgressPolicy{Allow: []string{"exact.example.com", ".corp.example.com"}}
	cases := map[string]bool{
		"exact.example.com":     true,
		"sub.exact.example.com": false, // no leading dot: exact only
		"corp.example.com":      true,  // dot entry matches the bare domain
		"a.corp.example.com":    true,
		"x.corp.example.com":    true,
		"CORP.EXAMPLE.COM":      true, // case-insensitive
		"notcorp.example.com":   false,
		"corp.example.com.evil": false,
		"":                      false,
	}
	for host, want := range cases {
		if got := p.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestEgressValidateRefusesMalformedEntries checks policy validation.
func TestEgressValidateRefusesMalformedEntries(t *testing.T) {
	for _, entry := range []string{"", " ", "https://api.example.com", "api.example.com:443", "api example.com", ".", "api.example.com."} {
		if err := (EgressPolicy{Allow: []string{entry}}).Validate(); err == nil {
			t.Errorf("Validate() accepted bad entry %q", entry)
		}
	}
	if err := (EgressPolicy{Allow: []string{"api.example.com", ".corp.example.com"}}).Validate(); err != nil {
		t.Errorf("Validate() rejected a good policy: %v", err)
	}
}
