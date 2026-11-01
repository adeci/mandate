package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/ncruces/go-sqlite3"

	"github.com/adeci/mandate/internal/authfilter"
)

type sourceExposure struct {
	// Hiding a container hides everything under it; hiding a sender hides
	// their messages and their DMs.
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
	sources map[string]string // name to database path

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

	names := slices.Sorted(maps.Keys(l.sources))
	st, err := os.Stat(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		if !l.loaded || !l.modTime.IsZero() {
			l.cur, l.err = parseExposure("", names)
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
	l.cur, l.err = parseExposure(string(data), names)
	if l.err != nil {
		l.err = fmt.Errorf("exposure config %s: %w", l.path, l.err)
	}
	l.modTime, l.size, l.loaded = st.ModTime(), st.Size(), true
	if l.err == nil {
		l.warnUnmatched()
	}
	return l.cur, l.err
}

// warnUnmatched logs hidden IDs that name nothing stored, such as a typo or a
// WhatsApp LID where messages carry the phone number.
func (l *policyLoader) warnUnmatched() {
	for _, name := range slices.Sorted(maps.Keys(l.cur.sources)) {
		misses, err := unmatched(l.sources[name], l.cur.sources[name])
		if err != nil {
			slog.Error("check hidden IDs", "source", name, "err", err)
			continue
		}
		for _, m := range misses {
			slog.Warn("hidden ID matches nothing stored", "source", name, "key", m.key, "id", m.id)
		}
	}
}

type miss struct{ key, id string }

// unmatched lists the hidden IDs that match no stored container or sender.
// Without a database yet there is nothing to compare with.
func unmatched(path string, e sourceExposure) ([]miss, error) {
	if len(e.HideContainers)+len(e.HideSenders) == 0 {
		return nil, nil
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	db, err := sqlite3.OpenFlags(path, sqlite3.OPEN_READONLY)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := db.BusyTimeout(time.Second); err != nil {
		return nil, err
	}
	stmt, _, err := db.Prepare(`
SELECT 'hide_containers', value FROM json_each(?1) AS j
WHERE NOT EXISTS (SELECT 1 FROM containers WHERE id = j.value)
UNION ALL
SELECT 'hide_senders', value FROM json_each(?2) AS j
WHERE NOT EXISTS (SELECT 1 FROM messages WHERE sender_id = j.value)
  AND NOT EXISTS (SELECT 1 FROM containers WHERE id = j.value)
  AND NOT EXISTS (SELECT 1 FROM containers AS c, json_each(c.participants_json) AS p WHERE p.value ->> 'id' = j.value)`)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()
	for i, ids := range [][]string{e.HideContainers, e.HideSenders} {
		list, err := jsonList(ids)
		if err != nil {
			return nil, err
		}
		if err := stmt.BindText(i+1, list); err != nil {
			return nil, err
		}
	}
	var misses []miss
	for stmt.Step() {
		misses = append(misses, miss{stmt.ColumnText(0), stmt.ColumnText(1)})
	}
	return misses, stmt.Err()
}
