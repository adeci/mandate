package gateway

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/adeci/sieve/internal/authfilter"
)

type SourceExposure struct {
	// Hiding a container also hides every container whose parent_id matches it.
	HideContainers []string `toml:"hide_containers"`
	HideSenders    []string `toml:"hide_senders"`
}

// Everything not hidden is visible; auth messages are always served without a body.
type Exposure struct {
	SMS      SourceExposure `toml:"sms"`
	WhatsApp SourceExposure `toml:"whatsapp"`
	Discord  SourceExposure `toml:"discord"`
	Auth     struct {
		ExtraSenderPatterns []string `toml:"extra_sender_patterns"`
		ExtraBodyPatterns   []string `toml:"extra_body_patterns"`
	} `toml:"auth"`
}

func (e *Exposure) source(name string) SourceExposure {
	switch name {
	case "sms":
		return e.SMS
	case "whatsapp":
		return e.WhatsApp
	case "discord":
		return e.Discord
	}
	return SourceExposure{}
}

type policy struct {
	exposure   Exposure
	classifier *authfilter.Classifier
}

// Unknown keys are errors, so a typo cannot silently un-hide something.
func parseExposure(data string) (*policy, error) {
	var e Exposure
	md, err := toml.Decode(data, &e)
	if err != nil {
		return nil, err
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return nil, fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
	}
	c, err := authfilter.New(e.Auth.ExtraSenderPatterns, e.Auth.ExtraBodyPatterns)
	if err != nil {
		return nil, err
	}
	return &policy{exposure: e, classifier: c}, nil
}

// A file that fails to parse fails every request until fixed (fail closed); a
// missing file means nothing is hidden.
type policyLoader struct {
	path string

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
			l.cur, l.err = parseExposure("")
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
	l.cur, l.err = parseExposure(string(data))
	if l.err != nil {
		l.err = fmt.Errorf("exposure config %s: %w", l.path, l.err)
	}
	l.modTime, l.size, l.loaded = st.ModTime(), st.Size(), true
	return l.cur, l.err
}
