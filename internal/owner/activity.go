package owner

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adeci/mandate/record"
)

// activity is one entry of the owner's history: a read the gateway
// reports (kind read, the verb as detail), or a step the owner side took.
// The kinds are listed in owner-api.md.
type activity struct {
	At        time.Time `json:"at"`
	Kind      string    `json:"kind"`
	Identity  string    `json:"identity,omitempty"`
	Connector string    `json:"connector,omitempty"`
	Chats     []chat    `json:"chats,omitempty"`
	Count     int       `json:"count,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	// Reason and Terms are a request's; Note is a proposal's.
	Reason string `json:"reason,omitempty"`
	Terms  []term `json:"terms,omitempty"`
	Note   string `json:"note,omitempty"`
	// Device is the display name of the device that decided.
	Device string `json:"device,omitempty"`
}

// gatewayRead is a read as the gateway posts it.
type gatewayRead struct {
	At        time.Time `json:"at"`
	Identity  string    `json:"identity"`
	Connector string    `json:"connector"`
	Verb      string    `json:"verb"`
	Chats     []chat    `json:"chats"`
	Count     int       `json:"count"`
}

func activityDir(dir string) string { return filepath.Join(dir, "activity") }

// Entries go to the file of their day, so a day's file holds exactly that
// day's entries.
func activityFile(dir string, day time.Time) string {
	return filepath.Join(activityDir(dir), day.UTC().Format(time.DateOnly)+".jsonl")
}

func appendActivity(dir string, entries []activity) error {
	if err := os.MkdirAll(activityDir(dir), 0o700); err != nil {
		return err
	}
	files := map[string][]byte{}
	for _, a := range entries {
		b, err := json.Marshal(a)
		if err != nil {
			return err
		}
		name := activityFile(dir, a.At)
		files[name] = append(append(files[name], b...), '\n')
	}
	for name, b := range files {
		f, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return err
		}
		_, err = f.Write(b)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("append activity: %w", err)
		}
	}
	return nil
}

// note appends what the owner side just did to the history, decided by the
// device of key by, if any. A failure is only logged: the step itself took
// effect. The caller holds s.mu.
func (s *Server) note(a activity, by string) {
	if a.At.IsZero() {
		a.At = s.now()
	}
	if k := s.trusted(by); k != nil {
		a.Device = cmp.Or(s.st.KeyNames[by], k.Name)
	}
	if err := appendActivity(s.dir, []activity{a}); err != nil {
		s.log.Error("note activity", "kind", a.Kind, "err", err)
	}
}

func (s *Server) postActivity(w http.ResponseWriter, r *http.Request) {
	var reads []gatewayRead
	if !decode(w, r, &reads) {
		return
	}
	entries := make([]activity, len(reads))
	for i, e := range reads {
		entries[i] = activity{At: e.At, Kind: "read", Identity: e.Identity, Connector: e.Connector, Chats: e.Chats, Count: e.Count, Detail: e.Verb}
	}
	if err := appendActivity(s.dir, entries); err != nil {
		s.log.Error("append activity", "err", err)
		failErr(w, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range entries {
		s.seen(a.Identity, a.At)
	}
	if err := s.commit("now"); err != nil {
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, struct{}{})
}

// cursor is an entry's place in the history: its time, then its offset
// in its day's file, so entries of the same second keep their order.
type cursor struct {
	at  time.Time
	off int64
}

func (c cursor) before(d cursor) bool {
	if n := c.at.Compare(d.at); n != 0 {
		return n < 0
	}
	return c.off < d.off
}

func (c cursor) String() string { return fmt.Sprintf("%d.%d", c.at.Unix(), c.off) }

func parseCursor(s string) (cursor, bool) {
	sec, off, ok := strings.Cut(s, ".")
	t, err1 := strconv.ParseInt(sec, 10, 64)
	o, err2 := strconv.ParseInt(off, 10, 64)
	return cursor{time.Unix(t, 0).UTC(), o}, ok && err1 == nil && err2 == nil && o >= 0
}

func (s *Server) getActivity(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			fail(w, http.StatusBadRequest, "limit is 1 to 500")
			return
		}
		limit = n
	}
	var before *cursor
	if v := q.Get("before"); v != "" {
		c, ok := parseCursor(v)
		if !ok {
			fail(w, http.StatusBadRequest, "before is a next from an earlier page")
			return
		}
		before = &c
	}
	match := func(activity) bool { return true }
	if text := strings.ToLower(strings.TrimSpace(q.Get("q"))); text != "" {
		s.mu.Lock()
		names := map[string]string{}
		for id, i := range s.st.Identities {
			names[id] = i.Name
		}
		s.mu.Unlock()
		match = func(a activity) bool { return a.matches(text, names[a.Identity]) }
	}
	entries, next, err := readActivity(s.dir, before, limit, match)
	if err != nil {
		s.log.Error("read activity", "err", err)
		failErr(w, err)
		return
	}
	reply(w, http.StatusOK, struct {
		Entries []activity `json:"entries"`
		Next    string     `json:"next,omitempty"`
	}{entries, next})
}

// placed is an entry with its place in the history.
type placed struct {
	activity
	at cursor
}

// matches reports whether the entry mentions text, which is lowercase:
// in its identity (by ID or as identity names it), connector, chats, terms,
// detail, reason, note or device.
func (a activity) matches(text, identity string) bool {
	fields := []string{a.Identity, identity, a.Connector, a.Detail, a.Reason, a.Note, a.Device}
	for _, c := range a.Chats {
		fields = append(fields, c.ID, c.Name)
	}
	for _, t := range a.Terms {
		fields = append(fields, t.Term)
		for _, c := range t.Chats {
			fields = append(fields, c.ID, c.Name)
		}
	}
	return slices.ContainsFunc(fields, func(f string) bool { return strings.Contains(strings.ToLower(f), text) })
}

// readActivity is up to limit entries before before (if set) that match,
// newest first, and the cursor of the page after them if there is one.
func readActivity(dir string, before *cursor, limit int, match func(activity) bool) ([]activity, string, error) {
	des, err := os.ReadDir(activityDir(dir))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, "", err
	}
	var days []string
	for _, de := range des {
		if day, ok := strings.CutSuffix(de.Name(), ".jsonl"); ok {
			days = append(days, day)
		}
	}
	// Newest day first; the names sort as dates.
	slices.SortFunc(days, func(a, b string) int { return strings.Compare(b, a) })
	var out []placed
	for _, day := range days {
		if before != nil && day > before.at.Format(time.DateOnly) {
			continue
		}
		es, err := readDay(filepath.Join(activityDir(dir), day+".jsonl"))
		if err != nil {
			return nil, "", err
		}
		es = slices.DeleteFunc(es, func(e placed) bool { return before != nil && !e.at.before(*before) || !match(e.activity) })
		slices.SortFunc(es, func(a, b placed) int {
			if a.at.before(b.at) {
				return 1
			}
			return -1
		})
		out = append(out, es...)
		if len(out) > limit {
			break
		}
	}
	entries := make([]activity, 0, min(len(out), limit))
	for _, p := range out[:min(len(out), limit)] {
		entries = append(entries, p.activity)
	}
	next := ""
	if len(out) > limit {
		next = out[limit-1].at.String()
	}
	return entries, next, nil
}

// readDay reads a day's file with each entry's place.
func readDay(path string) ([]placed, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []placed
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, maxBody)
	var off int64
	for sc.Scan() {
		line := sc.Bytes()
		at := off
		off += int64(len(line)) + 1
		var a struct {
			activity
			Verb string `json:"verb"` // a read kept before entries had kinds
		}
		if err := json.Unmarshal(line, &a); err != nil {
			continue // a line torn by a crash
		}
		if a.Kind == "" {
			a.Kind, a.Detail = "read", a.Verb
		}
		out = append(out, placed{a.activity, cursor{a.At, at}})
	}
	return out, sc.Err()
}

// span says seconds as people do: 90m, 2h, 7d.
func span(seconds int) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	case d >= time.Hour && d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d >= time.Minute && d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	}
	return strconv.Itoa(seconds) + "s"
}

// grantDetail says what a grant opens: its window and how far back.
func grantDetail(g record.Grant) string {
	window := "routine"
	if g.Schedule == nil {
		window = span(int(g.Until.Sub(g.From.Time).Seconds())) + " window"
	}
	history := "all history"
	if g.Horizon > 0 {
		history = span(g.Horizon) + " of history"
	}
	return window + ", " + history
}

// grantActivity is an entry about g, with the names of its chats.
func grantActivity(kind string, g record.Grant, names map[string]string, detail string) activity {
	a := activity{Kind: kind, Identity: g.Identity, Connector: g.Connector, Detail: detail}
	for _, id := range g.Chats {
		a.Chats = append(a.Chats, chat{id, names[id]})
	}
	return a
}

// displayText is the text of a write's signed display.
func displayText(fs []record.Field) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = f.Value
		if f.Label != "" {
			parts[i] = f.Label + ": " + f.Value
		}
	}
	return strings.Join(parts, "; ")
}
