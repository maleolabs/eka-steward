package secrets

import (
	"errors"
	"fmt"
	"strings"
)

// EgressPolicy is the DECLARATIVE worker-egress allow-list: the set of hosts a
// worker's traffic may reach (the model endpoints). It is validated and
// deterministic, and a sandbox/network-namespace (or a firewall) enforces it.
//
// HONEST LIMIT: the steward itself does NOT block network egress. A worker
// process inherits the host network; no policy here can stop it. EgressPolicy
// is a validated, testable DESCRIPTION of the intended allow-list, the contract
// a future sandbox enforces — not an enforcement mechanism. See docs/secrets.md.
type EgressPolicy struct {
	// Allow is the list of allowed host patterns. An exact host matches only
	// itself; a pattern beginning with "." matches that domain and any
	// subdomain (".example.com" matches "example.com" and "a.example.com").
	Allow []string
}

// DefaultEgressPolicy returns the model-endpoint allow-list that the declared
// credentials correspond to: the Anthropic, OpenAI, Google and OpenCode model
// endpoints.
func DefaultEgressPolicy() EgressPolicy {
	return EgressPolicy{Allow: []string{
		"api.anthropic.com",
		"api.openai.com",
		"generativelanguage.googleapis.com",
		"opencode.ai",
	}}
}

// Allowed reports whether host is reachable under the policy. Matching is
// case-insensitive and applies the documented rule: exact host OR a
// leading-dot suffix entry (".example.com" matches "example.com" itself and any
// subdomain). An empty host is never allowed.
func (p EgressPolicy) Allowed(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return false
	}
	for _, e := range p.Allow {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if strings.HasPrefix(e, ".") {
			if h == e[1:] || strings.HasSuffix(h, e) {
				return true
			}
			continue
		}
		if h == e {
			return true
		}
	}
	return false
}

// Validate checks the policy: every entry is a bare, non-empty host pattern with
// no scheme, path, port or whitespace. Order is not required. It returns the
// first offending entry as a deterministic error.
func (p EgressPolicy) Validate() error {
	for _, e := range p.Allow {
		if err := validateHostEntry(e); err != nil {
			return err
		}
	}
	return nil
}

// validateHostEntry checks one allow-list entry.
func validateHostEntry(e string) error {
	if e == "" {
		return errors.New("secrets: egress allow entry is empty")
	}
	if strings.TrimSpace(e) != e {
		return fmt.Errorf("secrets: egress allow entry %q has surrounding whitespace", e)
	}
	if strings.ContainsAny(e, "/: \t") {
		return fmt.Errorf("secrets: egress allow entry %q must be a bare host (no scheme/path/port)", e)
	}
	body := e
	if strings.HasPrefix(e, ".") {
		body = e[1:]
	}
	if body == "" || strings.HasPrefix(body, ".") || strings.HasSuffix(body, ".") {
		return fmt.Errorf("secrets: egress allow entry %q is not a valid host pattern", e)
	}
	return nil
}
