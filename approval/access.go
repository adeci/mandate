package approval

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	_ "time/tzdata" // schedules name IANA zones; the services may have no zoneinfo
)

// Kinds of signed access records.
const (
	KindAgent = "agent"
	KindGrant = "grant"
)

// Agent is a caller of the gateway, known by the SHA-256 of its token.
type Agent struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	TokenSHA256 string    `json:"token_sha256"` // hex
	CreatedAt   time.Time `json:"created_at"`
}

// Schedule opens a window at Start on each of Days (every day when empty),
// local to Zone, for Length.
type Schedule struct {
	Days   []time.Weekday `json:"days,omitempty"`
	Start  string         `json:"start"` // "HH:MM"
	Length time.Duration  `json:"length"`
	Zone   string         `json:"zone"`
}

// Grant lets Agent read Chats (every visible chat when empty) of Connector
// from From until Until, or in the windows of Schedule, back to Horizon
// before the moment of reading (all history when 0).
type Grant struct {
	ID        string        `json:"id"`
	Agent     string        `json:"agent"`
	Connector string        `json:"connector"`
	Chats     []string      `json:"chats,omitempty"`
	Horizon   time.Duration `json:"horizon"`
	From      time.Time     `json:"from,omitzero"`
	Until     time.Time     `json:"until,omitzero"`
	Schedule  *Schedule     `json:"schedule,omitempty"`
	Note      string        `json:"note,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

// Signed is an access record and the owner's passkey assertion over
// AccessChallenge(Kind, Record, Nonce, IssuedAt).
type Signed struct {
	Kind      string          `json:"kind"`
	Record    []byte          `json:"record"`
	Nonce     []byte          `json:"nonce"`
	IssuedAt  time.Time       `json:"issued_at"`
	Assertion json.RawMessage `json:"assertion"`
}

// AccessChallenge is what a passkey signs to approve an access record. The
// kind is hashed too, so an agent's bytes can't pass as a grant.
func AccessChallenge(kind string, record, nonce []byte, issuedAt time.Time) []byte {
	return hashParts([]byte("mandate-access-v1"), []byte(kind), record, nonce, []byte(issuedAt.UTC().Format(time.RFC3339Nano)))
}

func EncodeAgent(a Agent) ([]byte, error) {
	a.CreatedAt = a.CreatedAt.UTC()
	if err := a.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(a)
}

func DecodeAgent(b []byte) (Agent, error) {
	var a Agent
	if err := decodeStrict(b, &a); err != nil {
		return Agent{}, fmt.Errorf("decode agent: %w", err)
	}
	return a, a.validate()
}

func (a Agent) validate() error {
	if a.ID == "" || a.Name == "" {
		return errors.New("agent is missing its id or name")
	}
	if h, err := hex.DecodeString(a.TokenSHA256); err != nil || len(h) != 32 {
		return errors.New("agent token hash is not 32 bytes of hex")
	}
	return nil
}

func EncodeGrant(g Grant) ([]byte, error) {
	g.From, g.Until, g.CreatedAt = g.From.UTC(), g.Until.UTC(), g.CreatedAt.UTC()
	if err := g.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(g)
}

func DecodeGrant(b []byte) (Grant, error) {
	var g Grant
	if err := decodeStrict(b, &g); err != nil {
		return Grant{}, fmt.Errorf("decode grant: %w", err)
	}
	return g, g.validate()
}

func (g Grant) validate() error {
	switch {
	case g.ID == "" || g.Agent == "" || g.Connector == "":
		return errors.New("grant is missing its id, agent, or connector")
	case slices.Contains(g.Chats, ""):
		return errors.New("grant names an empty chat")
	case g.Horizon < 0:
		return errors.New("grant horizon is negative")
	case g.Schedule == nil && g.Until.IsZero():
		return errors.New("grant has neither an end nor a schedule")
	case g.Schedule != nil && (!g.Until.IsZero() || !g.From.IsZero()):
		return errors.New("grant has both a window and a schedule")
	case g.Schedule == nil && (g.From.IsZero() || !g.From.Before(g.Until)):
		return errors.New("grant window ends before it starts")
	case g.Schedule != nil:
		return g.Schedule.validate()
	}
	return nil
}

func (s Schedule) validate() error {
	if _, _, err := s.clock(); err != nil {
		return err
	}
	if s.Length <= 0 || s.Length > 24*time.Hour {
		return errors.New("schedule length must be over 0 and at most 24h")
	}
	for _, d := range s.Days {
		if d < time.Sunday || d > time.Saturday {
			return fmt.Errorf("schedule day %d is not a weekday", d)
		}
	}
	if _, err := s.location(); err != nil {
		return err
	}
	return nil
}

// clock parses Start.
func (s Schedule) clock() (hour, minute int, err error) {
	t, err := time.Parse("15:04", s.Start)
	if err != nil {
		return 0, 0, fmt.Errorf("schedule start %q is not HH:MM", s.Start)
	}
	return t.Hour(), t.Minute(), nil
}

// location refuses "" and "Local", which LoadLocation accepts but which
// mean different zones on different machines.
func (s Schedule) location() (*time.Location, error) {
	if s.Zone == "" || s.Zone == "Local" {
		return nil, errors.New("schedule needs an IANA zone")
	}
	loc, err := time.LoadLocation(s.Zone)
	if err != nil {
		return nil, fmt.Errorf("schedule zone: %w", err)
	}
	return loc, nil
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// VerifyAgent checks that a key trusted now signed s as an agent. Access
// records stay valid until deleted, so there is no age limit; a removed key
// invalidates everything it signed.
func (v *Verifier) VerifyAgent(s Signed) (Agent, error) {
	if err := v.verifySigned(s, KindAgent); err != nil {
		return Agent{}, err
	}
	return DecodeAgent(s.Record)
}

// VerifyGrant is VerifyAgent for grants.
func (v *Verifier) VerifyGrant(s Signed) (Grant, error) {
	if err := v.verifySigned(s, KindGrant); err != nil {
		return Grant{}, err
	}
	return DecodeGrant(s.Record)
}

func (v *Verifier) verifySigned(s Signed, kind string) error {
	if s.Kind != kind {
		return fmt.Errorf("access record is a %q, want %q", s.Kind, kind)
	}
	if len(s.Nonce) < 16 {
		return errors.New("access record nonce is too short")
	}
	_, err := v.passkey(s.Assertion, AccessChallenge(s.Kind, s.Record, s.Nonce, s.IssuedAt))
	return err
}

// Open reports whether g is active at now, and when its current window
// closes or, while closed, when the next one opens (zero if none will).
func (g Grant) Open(now time.Time) (bool, time.Time) {
	if g.Schedule == nil {
		switch {
		case now.Before(g.From):
			return false, g.From
		case now.Before(g.Until):
			return true, g.Until
		}
		return false, time.Time{}
	}
	s := g.Schedule
	loc, err := s.location()
	if err != nil {
		return false, time.Time{}
	}
	hour, minute, err := s.clock()
	if err != nil {
		return false, time.Time{}
	}
	local := now.In(loc)
	// A window open now started today or, running past midnight, yesterday;
	// the next one starts within two weeks, as the clocks may skip a weekly
	// start.
	var end, next time.Time
	for d := -1; d <= 14; d++ {
		day := time.Date(local.Year(), local.Month(), local.Day()+d, 12, 0, 0, 0, loc)
		if len(s.Days) > 0 && !slices.Contains(s.Days, day.Weekday()) {
			continue
		}
		start, ok := wallClock(day, hour, minute, loc)
		if !ok {
			continue
		}
		switch {
		case !now.Before(start) && now.Before(start.Add(s.Length)):
			end = start.Add(s.Length)
		case start.After(now) && next.IsZero():
			next = start
		}
	}
	if !end.IsZero() {
		return true, end
	}
	return false, next
}

// wallClock is the first instant reading hour:minute on day's date in loc,
// or false when the clocks skip that time that day. time.Date would move a
// skipped time and pick either of a repeated one. Each occurrence is at
// one of the offsets in effect around that day, as zones change at most
// once a day.
func wallClock(day time.Time, hour, minute int, loc *time.Location) (time.Time, bool) {
	wall := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, time.UTC)
	var first time.Time
	for _, probe := range []time.Time{wall.Add(-24 * time.Hour), wall, wall.Add(24 * time.Hour)} {
		_, offset := probe.In(loc).Zone()
		t := wall.Add(-time.Duration(offset) * time.Second).In(loc)
		if t.Day() == day.Day() && t.Hour() == hour && t.Minute() == minute && (first.IsZero() || t.Before(first)) {
			first = t
		}
	}
	return first, !first.IsZero()
}

// Cutoff is the oldest moment g lets an agent read at now; zero is no
// limit.
func (g Grant) Cutoff(now time.Time) time.Time {
	if g.Horizon == 0 {
		return time.Time{}
	}
	return now.Add(-g.Horizon)
}
