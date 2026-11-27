package secrets

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// FileResolver resolves credentials from a StateDir-based dotenv file: one
// `NAME=value` per line, blank lines and `#` comments ignored. A missing file
// yields no credentials (Get returns false), never an error; a malformed line is
// an error at construction. The production caller passes a path under the
// steward's StateDir (e.g. <StateDir>/secrets.env).
type FileResolver struct {
	values map[string]string
}

// NewFileResolver reads path once. A missing file is not an error (no
// credentials configured).
func NewFileResolver(path string) (*FileResolver, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &FileResolver{values: map[string]string{}}, nil
		}
		return nil, fmt.Errorf("secrets: open %q: %w", path, err)
	}
	defer f.Close()

	values := map[string]string{}
	sc := bufio.NewScanner(f)
	for line := 1; sc.Scan(); line++ {
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		i := strings.IndexByte(text, '=')
		if i <= 0 {
			return nil, fmt.Errorf("secrets: %s:%d: not a NAME=value line", path, line)
		}
		name := strings.TrimSpace(text[:i])
		if !validEnvName(name) {
			return nil, fmt.Errorf("secrets: %s:%d: invalid name %q", path, line, name)
		}
		values[name] = strings.TrimSpace(text[i+1:])
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("secrets: read %q: %w", path, err)
	}
	return &FileResolver{values: values}, nil
}

// Get implements Resolver.
func (r *FileResolver) Get(name string) (string, bool) {
	v, ok := r.values[name]
	return v, ok
}
