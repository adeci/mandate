package discord

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.Advance(d)
	return nil
}

func (c *fakeClock) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type fixedRand float64

func (r fixedRand) Float64() float64 { return float64(r) }

var newYork = mustLoc("America/New_York")

func mustLoc(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

var noon = time.Date(2026, 10, 29, 12, 0, 0, 0, newYork)

func allDay() activeHours { return activeHours{start: 0, end: 24, loc: newYork} }

func TestGapBounds(t *testing.T) {
	p := newPacer(&state{}, rand.New(rand.NewPCG(1, 2)), allDay())
	var plain, paused int
	at := noon
	for range 2000 {
		p.record(at)
		p.st.BreakUntil = time.Time{} // sessions are tested separately
		slot, _ := p.slot(at.Add(time.Second), kindAPI, "r", time.Time{})
		gap := slot.Sub(at)
		switch {
		case gap >= gapMin && gap <= gapMax:
			plain++
		case gap >= gapMin+pauseMin && gap <= gapMax+pauseMax:
			paused++
		default:
			t.Fatalf("gap %v outside [5s,12s] and [20s,57s]", gap)
		}
		at = slot
		p.st.Requests = nil // keep the budget out of this test
	}
	// 15% pauses: expect roughly 300 of 2000.
	if paused < 200 || paused > 400 || plain+paused != 2000 {
		t.Errorf("plain %d, paused %d", plain, paused)
	}
}

func TestAppOpenAfterIdle(t *testing.T) {
	for _, r := range []float64{0, 0.5, 0.999} {
		p := newPacer(&state{}, fixedRand(r), allDay())
		p.record(noon)
		now := noon.Add(idleAfter + time.Second)
		slot, reason := p.slot(now, kindAPI, "r", time.Time{})
		if d := slot.Sub(now); d < openMin || d > openMax || reason != "app open" {
			t.Errorf("rng %v: idle wait %v (%s), want 1-3 s app open", r, d, reason)
		}
	}
	p := newPacer(&state{}, fixedRand(0.5), allDay())
	if slot, _ := p.slot(noon, kindAPI, "r", time.Time{}); slot.Sub(noon) != 2*time.Second {
		t.Errorf("first request waits %v, want 2s", slot.Sub(noon))
	}
	p.record(noon)
	if slot, _ := p.slot(noon, kindSubresource, "js", time.Time{}); slot.Sub(noon) != 500*time.Millisecond {
		t.Errorf("subresource waits %v", slot.Sub(noon))
	}
}

func TestSessionBreak(t *testing.T) {
	t.Run("after 20 requests", func(t *testing.T) {
		p := newPacer(&state{}, fixedRand(0.5), allDay())
		at := noon
		for i := range sessionRequests {
			if !p.st.BreakUntil.IsZero() {
				t.Fatalf("break after %d requests", i)
			}
			p.record(at)
			at = at.Add(6 * time.Second)
		}
		last := p.st.LastRequest
		if d := p.st.BreakUntil.Sub(last); d < breakMin || d > breakMax {
			t.Fatalf("break %v, want 10-25 min", d)
		}
		slot, reason := p.slot(last.Add(time.Minute), kindAPI, "r", time.Time{})
		if reason != "session break" || slot.Before(p.st.BreakUntil) {
			t.Errorf("slot %v (%s) before break end %v", slot, reason, p.st.BreakUntil)
		}
	})
	t.Run("after 15 minutes", func(t *testing.T) {
		p := newPacer(&state{}, fixedRand(0.5), allDay())
		for i := range 16 {
			p.record(noon.Add(time.Duration(i) * time.Minute))
			if i < 15 && !p.st.BreakUntil.IsZero() {
				t.Fatalf("break after %d min", i)
			}
		}
		if p.st.BreakUntil.IsZero() {
			t.Fatal("no break after 15 minutes of activity")
		}
		if p.st.SessionCount != 0 {
			t.Errorf("session not reset: %d", p.st.SessionCount)
		}
	})
	t.Run("idle starts a new session", func(t *testing.T) {
		p := newPacer(&state{}, fixedRand(0.5), allDay())
		for i := range 10 {
			p.record(noon.Add(time.Duration(i) * time.Minute))
		}
		p.record(noon.Add(time.Hour))
		if p.st.SessionCount != 1 || !p.st.SessionStart.Equal(noon.Add(time.Hour)) {
			t.Errorf("session %d since %v", p.st.SessionCount, p.st.SessionStart)
		}
	})
}

func TestDailyBudget(t *testing.T) {
	st := &state{}
	p := newPacer(st, fixedRand(0.5), allDay())
	start := noon
	for i := range dailyBudget {
		st.Requests = append(st.Requests, start.Add(time.Duration(i)*5*time.Minute))
	}
	st.LastRequest = st.Requests[len(st.Requests)-1]
	now := st.LastRequest.Add(time.Hour)
	slot, reason := p.slot(now, kindAPI, "r", time.Time{})
	wantFree := start.Add(budgetWindow + time.Millisecond)
	if reason != "daily budget" || slot.Before(wantFree) || slot.After(wantFree.Add(openMax)) {
		t.Errorf("slot %v (%s), want just after %v", slot, reason, wantFree)
	}
	// Once the oldest request leaves the window, one more is allowed; the next
	// waits for the second oldest.
	p.record(slot)
	if n := len(st.Requests); n != dailyBudget {
		t.Fatalf("window holds %d requests, want %d", n, dailyBudget)
	}
	next, reason := p.slot(slot.Add(time.Minute), kindAPI, "r", time.Time{})
	if reason != "daily budget" || next.Before(start.Add(5*time.Minute+budgetWindow)) {
		t.Errorf("second slot %v (%s)", next, reason)
	}
}

func TestActiveHours(t *testing.T) {
	h, err := parseActiveHours("08-24", newYork)
	if err != nil {
		t.Fatal(err)
	}
	day := func(hh, mm int) time.Time { return time.Date(2026, 10, 29, hh, mm, 0, 0, newYork) }
	tests := []struct {
		at     time.Time
		active bool
		next   time.Time
	}{
		{day(7, 59), false, day(8, 0)},
		{day(8, 0), true, time.Time{}},
		{day(23, 59), true, time.Time{}},
		{day(2, 0), false, day(8, 0)},
		{day(0, 0), false, day(8, 0)},
	}
	for _, tt := range tests {
		if got := h.active(tt.at); got != tt.active {
			t.Errorf("active(%v) = %v", tt.at, got)
		}
		if !tt.active {
			if got := h.next(tt.at); !got.Equal(tt.next) {
				t.Errorf("next(%v) = %v, want %v", tt.at, got, tt.next)
			}
		}
	}
	wrap, err := parseActiveHours("22-06", newYork)
	if err != nil {
		t.Fatal(err)
	}
	if !wrap.active(day(23, 0)) || !wrap.active(day(3, 0)) || wrap.active(day(12, 0)) {
		t.Error("wrapping window wrong")
	}
	if got := wrap.next(day(12, 0)); !got.Equal(day(22, 0)) {
		t.Errorf("wrap next = %v", got)
	}
	for _, bad := range []string{"", "8", "08-08", "24-08", "08-25", "a-b", "-1-05"} {
		if _, err := parseActiveHours(bad, newYork); err == nil {
			t.Errorf("parseActiveHours(%q) accepted", bad)
		}
	}

	// A request at 02:00 waits for 08:00 plus the app-open delay.
	p := newPacer(&state{}, fixedRand(0.5), h)
	slot, reason := p.slot(day(2, 0), kindAPI, "r", time.Time{})
	if reason != "outside active hours" || !slot.Equal(day(8, 0).Add(2*time.Second)) {
		t.Errorf("slot %v (%s)", slot, reason)
	}
}

func TestCooldownAndBuckets(t *testing.T) {
	st := &state{CooldownUntil: noon.Add(time.Hour)}
	p := newPacer(st, fixedRand(0.5), allDay())
	if slot, reason := p.slot(noon, kindAPI, "r", time.Time{}); reason != "cooldown" || !slot.Equal(noon.Add(time.Hour+2*time.Second)) {
		t.Errorf("cooldown slot %v (%s)", slot, reason)
	}
	st.CooldownUntil = time.Time{}
	p.record(noon)
	p.updateBucket(noon, "/channels/{channel_id}/messages|1", "abcd", "0", "30.5")
	slot, reason := p.slot(noon.Add(time.Second), kindAPI, "/channels/{channel_id}/messages|1", time.Time{})
	if reason != "rate limit bucket" || !slot.Equal(noon.Add(30500*time.Millisecond)) {
		t.Errorf("bucket slot %v (%s)", slot, reason)
	}
	// Another channel is a different bucket instance.
	if _, reason := p.slot(noon.Add(time.Second), kindAPI, "/channels/{channel_id}/messages|2", time.Time{}); reason != "gap" {
		t.Errorf("other channel blocked: %s", reason)
	}
	p.updateBucket(noon, "/channels/{channel_id}/messages|1", "abcd", "3", "1")
	if _, reason := p.slot(noon.Add(time.Second), kindAPI, "/channels/{channel_id}/messages|1", time.Time{}); reason != "gap" {
		t.Errorf("bucket still blocked: %s", reason)
	}
}
