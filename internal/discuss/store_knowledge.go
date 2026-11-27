package discuss

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/maleolabs/eka-core/v2/api"
)

// StoreKnowledge is the READ-ONLY EKA view of a discussion: the subject's
// context and the decisions already recorded for it. It reads through
// api.Resolver and api.Knowledge (the read-only contract) and never mutates —
// every mutation stays in internal/ekawrite, reached through the Recorder
// seam. See docs/discuss.md.
type StoreKnowledge struct {
	// Knowledge is the read-only knowledge service (rt.Knowledge()).
	Knowledge api.Knowledge
	// Resolver is the read-only identity resolver (rt.Resolver()).
	Resolver api.Resolver
	// Project is the project scope to read.
	Project string
}

// Subject reads the work item under discussion (its title and brief) through
// the resolver.
func (s StoreKnowledge) Subject(_ context.Context, line string) (Subject, error) {
	if s.Resolver == nil {
		return Subject{}, fmt.Errorf("discuss: StoreKnowledge has no resolver")
	}
	ns, typ, id, err := parseLine(line)
	if err != nil {
		return Subject{}, err
	}
	u, found, err := s.Resolver.Resolve(line)
	if err != nil {
		return Subject{}, fmt.Errorf("discuss: resolve subject %s: %w", line, err)
	}
	if !found {
		return Subject{}, fmt.Errorf("discuss: subject %s not found", line)
	}
	brief := contentField(u.ContentPayload, "description")
	return Subject{
		Line:      line,
		Namespace: ns,
		Type:      typ,
		ID:        id,
		Title:     firstLine(brief),
		Brief:     brief,
	}, nil
}

// Decisions reads the decisions recorded for the subject back from EKA. It
// filters the project's dec- knowledge by the subject's decision-id prefix and
// returns them in deterministic round order.
func (s StoreKnowledge) Decisions(_ context.Context, subjectLine string) ([]Decision, error) {
	if s.Knowledge == nil {
		return nil, fmt.Errorf("discuss: StoreKnowledge has no knowledge service")
	}
	subj, err := s.Subject(context.Background(), subjectLine)
	if err != nil {
		return nil, err
	}
	units, err := s.Knowledge.Search(api.SearchQuery{
		ProjectID: s.Project,
		Namespace: subj.Namespace,
		Type:      DecisionType,
	})
	if err != nil {
		return nil, fmt.Errorf("discuss: read decisions of %s: %w", subjectLine, err)
	}
	prefix := subj.ID + "-dec-r"
	out := make([]Decision, 0, len(units))
	for _, u := range units {
		if !strings.HasPrefix(u.Identity.ID, prefix) {
			continue
		}
		out = append(out, decisionFromUnit(u))
	}
	sortDecisions(out)
	return out, nil
}

// decisionFromUnit projects a persisted dec- unit into a Decision.
func decisionFromUnit(u *api.KnowledgeUnit) Decision {
	return Decision{
		ID:           u.Identity.ID,
		Round:        roundFromID(u.Identity.ID),
		Context:      contentField(u.ContentPayload, "context"),
		Decision:     contentField(u.ContentPayload, "decision"),
		Consequences: contentField(u.ContentPayload, "consequences"),
		Alternatives: contentStrings(u.ContentPayload, "alternativesConsidered"),
	}
}

// parseLine splits a qualified line form "<ns>/<type>:<id>".
func parseLine(line string) (ns, typ, id string, err error) {
	slash := strings.IndexByte(line, '/')
	colon := strings.LastIndexByte(line, ':')
	if slash <= 0 || colon <= slash+1 || colon == len(line)-1 {
		return "", "", "", fmt.Errorf("discuss: %q is not a qualified line (<ns>/<type>:<id>)", line)
	}
	return line[:slash], line[slash+1 : colon], line[colon+1:], nil
}

// roundFromID parses the round number from "<id>-dec-r<NN>"; 0 when absent.
func roundFromID(id string) int {
	i := strings.LastIndex(id, "-dec-r")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(id[i+len("-dec-r"):])
	if err != nil {
		return 0
	}
	return n
}

// contentField decodes one string content field from a unit's payload.
func contentField(payload []byte, key string) string {
	fields := decodeContent(payload)
	if s, ok := fields[key].(string); ok {
		return s
	}
	return ""
}

// contentStrings decodes one string-list content field from a unit's payload.
func contentStrings(payload []byte, key string) []string {
	fields := decodeContent(payload)
	raw, ok := fields[key].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// decodeContent unmarshals a unit content payload; a malformed payload yields
// an empty map (individual fields read as zero values).
func decodeContent(payload []byte) map[string]any {
	if len(payload) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil
	}
	return m
}

// firstLine returns the first non-empty line of s (a short title).
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return ""
}

// sortDecisions orders decisions by (round, id) deterministically.
func sortDecisions(ds []Decision) {
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].Round != ds[j].Round {
			return ds[i].Round < ds[j].Round
		}
		return ds[i].ID < ds[j].ID
	})
}
