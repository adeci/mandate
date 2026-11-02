package approval_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/approval/approvaltest"
)

func agentRecord(t *testing.T) []byte {
	t.Helper()
	b, err := approval.EncodeAgent(approval.Agent{
		ID: "a1", Name: "Grok Bot", TokenSHA256: strings.Repeat("ab", 32), CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestVerifyAccessRecords(t *testing.T) {
	t.Parallel()
	key := approvaltest.New(t, rpID, origin)
	other := approvaltest.New(t, rpID, origin)
	v := verifier(t, key)
	rec := agentRecord(t)

	signed := key.SignAccess(approval.KindAgent, rec, now.Add(-30*24*time.Hour))
	a, err := v.VerifyAgent(signed)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Grok Bot" {
		t.Fatalf("agent = %+v", a)
	}

	// The same bytes signed in another domain or as another kind don't pass.
	write := key.Approve(rec, now)
	asWrite := approval.Signed{Kind: approval.KindAgent, Record: rec, Nonce: write.Nonce, IssuedAt: write.IssuedAt, Assertion: write.Assertion}
	asGrant := key.SignAccess(approval.KindGrant, rec, now)
	asGrant.Kind = approval.KindAgent
	for name, tc := range map[string]struct {
		s    approval.Signed
		want error
	}{
		"signed as a write approval": {asWrite, approval.ErrSignature},
		"signed as a grant":          {asGrant, approval.ErrSignature},
		"unenrolled passkey":         {other.SignAccess(approval.KindAgent, rec, now), approval.ErrUnknown},
	} {
		if _, err := v.VerifyAgent(tc.s); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if _, err := v.VerifyGrant(signed); err == nil {
		t.Error("agent verified as a grant")
	}
	fresh := key.SignAccess(approval.KindAgent, rec, now)
	_, err = v.Verify(approval.Approval{Record: rec, Nonce: fresh.Nonce, IssuedAt: fresh.IssuedAt, Assertion: fresh.Assertion})
	if !errors.Is(err, approval.ErrSignature) {
		t.Errorf("access assertion as a write approval: %v", err)
	}
}

func TestRemovedKeyInvalidatesAccess(t *testing.T) {
	t.Parallel()
	a := approvaltest.New(t, rpID, origin)
	b := approvaltest.New(t, rpID, origin)
	k, _ := trusting(t, a, b)
	v := &approval.Verifier{Keys: k}
	signed := b.SignAccess(approval.KindAgent, agentRecord(t), now)
	if _, err := v.VerifyAgent(signed); err != nil {
		t.Fatal(err)
	}
	mustApply(t, k, a.Sign(change(t, k, approval.OpRemove, b.Key("more"))))
	if _, err := v.VerifyAgent(signed); !errors.Is(err, approval.ErrUnknown) {
		t.Fatalf("after removing the signer: %v", err)
	}
}

func TestDecodeGrantRejects(t *testing.T) {
	t.Parallel()
	sched := &approval.Schedule{Start: "09:00", Length: time.Hour, Zone: "Europe/Berlin"}
	base := approval.Grant{ID: "g1", Agent: "a1", Connector: "signal", From: now, Until: now.Add(time.Hour), CreatedAt: now}
	for name, edit := range map[string]func(*approval.Grant){
		"window and schedule": func(g *approval.Grant) { g.Schedule = sched },
		"no end":              func(g *approval.Grant) { g.Until = time.Time{} },
		"ends before start":   func(g *approval.Grant) { g.From = g.Until },
		"negative horizon":    func(g *approval.Grant) { g.Horizon = -time.Hour },
		"no agent":            func(g *approval.Grant) { g.Agent = "" },
		"empty chat":          func(g *approval.Grant) { g.Chats = []string{""} },
		"machine-local zone": func(g *approval.Grant) {
			g.From, g.Until, g.Schedule = time.Time{}, time.Time{}, &approval.Schedule{Start: "09:00", Length: time.Hour, Zone: "Local"}
		},
		"unknown zone": func(g *approval.Grant) {
			g.From, g.Until, g.Schedule = time.Time{}, time.Time{}, &approval.Schedule{Start: "09:00", Length: time.Hour, Zone: "Mars/Olympus"}
		},
		"over a day": func(g *approval.Grant) {
			g.From, g.Until, g.Schedule = time.Time{}, time.Time{}, &approval.Schedule{Start: "09:00", Length: 25 * time.Hour, Zone: "UTC"}
		},
		"bad start": func(g *approval.Grant) {
			g.From, g.Until, g.Schedule = time.Time{}, time.Time{}, &approval.Schedule{Start: "9am", Length: time.Hour, Zone: "UTC"}
		},
	} {
		g := base
		edit(&g)
		if _, err := approval.EncodeGrant(g); err == nil {
			t.Errorf("%s: encoded", name)
		}
	}
	if _, err := approval.DecodeGrant([]byte(`{"id":"g1","agent":"a1","connector":"signal","horizon":0,"until":"2026-01-01T00:00:00Z","from":"2025-01-01T00:00:00Z","created_at":"2025-01-01T00:00:00Z","extra":1}`)); err == nil {
		t.Error("unknown field decoded")
	}
}

func TestScheduleWindows(t *testing.T) {
	t.Parallel()
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	at := func(month time.Month, day, hour, minute int) time.Time {
		return time.Date(2026, month, day, hour, minute, 0, 0, berlin)
	}
	weeknights := approval.Grant{Schedule: &approval.Schedule{
		Days:  []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday},
		Start: "22:00", Length: 4 * time.Hour, Zone: "Europe/Berlin",
	}}
	mornings := approval.Grant{Schedule: &approval.Schedule{Start: "09:00", Length: 2 * time.Hour, Zone: "Europe/Berlin"}}
	for _, tc := range []struct {
		name  string
		g     approval.Grant
		now   time.Time
		open  bool
		until time.Time
	}{
		{"Friday night", weeknights, at(time.May, 15, 23, 0), true, at(time.May, 16, 2, 0)},
		{"past midnight into Saturday", weeknights, at(time.May, 16, 1, 30), true, at(time.May, 16, 2, 0)},
		{"Saturday night is closed", weeknights, at(time.May, 16, 23, 0), false, at(time.May, 18, 22, 0)},
		{"just closed", weeknights, at(time.May, 16, 2, 0), false, at(time.May, 18, 22, 0)},
		{"morning on the DST day", mornings, at(time.March, 29, 9, 30), true, at(time.March, 29, 11, 0)},
		// 07:30 UTC is 08:30 the day before the clocks change and 09:30 on
		// the day: the window follows local time.
		{"07:30 UTC before DST", mornings, time.Date(2026, time.March, 28, 7, 30, 0, 0, time.UTC), false, at(time.March, 28, 9, 0)},
		{"07:30 UTC on the DST day", mornings, time.Date(2026, time.March, 29, 7, 30, 0, 0, time.UTC), true, at(time.March, 29, 11, 0)},
	} {
		open, until := tc.g.Open(tc.now)
		if open != tc.open || !until.Equal(tc.until) {
			t.Errorf("%s: Open(%s) = %v, %s; want %v, %s", tc.name, tc.now, open, until, tc.open, tc.until)
		}
	}

	// New York skips 02:00-03:00 on 2026-03-08 and repeats 01:00-02:00 on
	// 2026-11-01.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	utc := func(month time.Month, day, hour, minute int) time.Time {
		return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
	}
	sundays := approval.Grant{Schedule: &approval.Schedule{Days: []time.Weekday{time.Sunday}, Start: "02:30", Length: time.Hour, Zone: "America/New_York"}}
	nights := approval.Grant{Schedule: &approval.Schedule{Start: "01:30", Length: time.Hour, Zone: "America/New_York"}}
	for _, tc := range []struct {
		name  string
		g     approval.Grant
		now   time.Time
		open  bool
		until time.Time
	}{
		{"the evening before a skipped start", sundays, utc(time.March, 8, 2, 0), false, time.Date(2026, time.March, 15, 2, 30, 0, 0, ny)},
		{"a start the clocks skip", sundays, utc(time.March, 8, 6, 45), false, time.Date(2026, time.March, 15, 2, 30, 0, 0, ny)},
		{"after the skipped start", sundays, utc(time.March, 8, 7, 15), false, time.Date(2026, time.March, 15, 2, 30, 0, 0, ny)},
		{"the first 01:30 of a repeated hour", nights, utc(time.November, 1, 5, 45), true, utc(time.November, 1, 6, 30)},
		{"the second 01:30 opens nothing", nights, utc(time.November, 1, 6, 45), false, time.Date(2026, time.November, 2, 1, 30, 0, 0, ny)},
	} {
		open, until := tc.g.Open(tc.now)
		if open != tc.open || !until.Equal(tc.until) {
			t.Errorf("%s: Open(%s) = %v, %s; want %v, %s", tc.name, tc.now, open, until, tc.open, tc.until)
		}
	}

	oneOff := approval.Grant{From: now, Until: now.Add(time.Hour)}
	if open, next := oneOff.Open(now.Add(-time.Minute)); open || !next.Equal(now) {
		t.Errorf("before a one-off: %v %s", open, next)
	}
	if open, next := oneOff.Open(now.Add(time.Hour)); open || !next.IsZero() {
		t.Errorf("after a one-off: %v %s", open, next)
	}
}

func TestCutoff(t *testing.T) {
	t.Parallel()
	if c := (approval.Grant{Horizon: 24 * time.Hour}).Cutoff(now); !c.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("24h horizon: cutoff %s", c)
	}
	if c := (approval.Grant{}).Cutoff(now); !c.IsZero() {
		t.Errorf("no horizon: cutoff %s", c)
	}
}
