package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/adeci/mandate/internal/authfilter"
)

type sourceExposure struct {
	// Hiding a container also hides every container whose parent_id matches it.
	HideContainers []string `toml:"hide_containers"`
	HideSenders    []string `toml:"hide_senders"`
}

type authExposure struct {
	ExtraSenderPatterns []string `toml:"extra_sender_patterns"`
	ExtraBodyPatterns   []string `toml:"extra_body_patterns"`
}

// policy is the parsed exposure file: one section per configured source plus
// [auth]. Everything not hidden is visible; auth messages are always served
// without a body.
type policy struct {
	sources    map[string]sourceExposure
	classifier *authfilter.Classifier
}

// Unknown keys and sections are errors, so a typo cannot silently un-hide
// something.
func parseExposure(data string, sources []string) (*policy, error) {
	var sections map[string]toml.Primitive
	md, err := toml.Decode(data, &sections)
	if err != nil {
		return nil, err
	}
	pol := &policy{sources: map[string]sourceExposure{}}
	var auth authExposure
	for _, name := range slices.Sorted(maps.Keys(sections)) {
		switch {
		case name == "auth":
			err = md.PrimitiveDecode(sections[name], &auth)
		case slices.Contains(sources, name):
			var e sourceExposure
			err = md.PrimitiveDecode(sections[name], &e)
			pol.sources[name] = e
		default:
			err = fmt.Errorf("section [%s] is not a configured source (sources: %s)", name, strings.Join(sources, ", "))
		}
		if err != nil {
			return nil, err
		}
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	pol.classifier, err = authfilter.New(auth.ExtraSenderPatterns, auth.ExtraBodyPatterns)
	if err != nil {
		return nil, err
	}
	return pol, nil
}

// A file that fails to parse fails every request until fixed (fail closed); a
// missing file means nothing is hidden.
type policyLoader struct {
	path    string
	sources []string

	mu      sync.Mutex
	modTime time.Time
	size    int64
	loaded  bool
	cur     *policy
	err     error
}

func (l *policyLoader) get() (*policy, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	st, err := os.Stat(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		if !l.loaded || !l.modTime.IsZero() {
			l.cur, l.err = parseExposure("", l.sources)
			l.modTime, l.size, l.loaded = time.Time{}, 0, true
		}
		return l.cur, l.err
	}
	if err != nil {
		return nil, fmt.Errorf("exposure config: %w", err)
	}
	if l.loaded && st.ModTime().Equal(l.modTime) && st.Size() == l.size {
		return l.cur, l.err
	}
	data, err := os.ReadFile(l.path)
	if err != nil {
		return nil, fmt.Errorf("exposure config: %w", err)
	}
	l.cur, l.err = parseExposure(string(data), l.sources)
	if l.err != nil {
		l.err = fmt.Errorf("exposure config %s: %w", l.path, l.err)
	}
	l.modTime, l.size, l.loaded = st.ModTime(), st.Size(), true
	return l.cur, l.err
}
