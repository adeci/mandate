package main

import (
	"context"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []waiter
}

type waiter struct {
	at time.Time
	ch chan time.Time
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

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, waiter{c.now.Add(d), ch})
	return ch
}

// pending reports whether a timer is set for now+d.
func (c *fakeClock) pending(d time.Duration) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, w := range c.waiters {
		if w.at.Equal(c.now.Add(d)) {
			return true
		}
	}
	return false
}

func (c *fakeClock) Advance(d time.Duration) {
	if d <= 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if w.at.After(c.now) {
			kept = append(kept, w)
			continue
		}
		w.ch <- c.now
	}
	c.waiters = kept
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

func TestGapBounds(t *testing.T) {
	p := newPacer(&state{}, rand.New(rand.NewPCG(1, 2)))
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
		p := newPacer(&state{}, fixedRand(r))
		p.record(noon)
		now := noon.Add(idleAfter + time.Second)
		slot, reason := p.slot(now, kindAPI, "r", time.Time{})
		if d := slot.Sub(now); d < openMin || d > openMax || reason != "app open" {
			t.Errorf("rng %v: idle wait %v (%s), want 1-3 s app open", r, d, reason)
		}
	}
	p := newPacer(&state{}, fixedRand(0.5))
	if slot, _ := p.slot(noon, kindAPI, "r", time.Time{}); slot.Sub(noon) != 2*time.Second {
		t.Errorf("first request waits %v, want 2s", slot.Sub(noon))
	}
	p.record(noon)
	if slot, _ := p.slot(noon, kindFollow, "js", time.Time{}); slot.Sub(noon) != 500*time.Millisecond {
		t.Errorf("follow-up waits %v", slot.Sub(noon))
	}
	// A follow-up belongs to an action already under way, so a session break
	// that began with the request before it does not hold it.
	p.st.BreakUntil = noon.Add(10 * time.Minute)
	if slot, reason := p.slot(noon, kindFollow, "r", noon.Add(3*time.Second)); reason != "follow-up" || !slot.Equal(noon.Add(3*time.Second)) {
		t.Errorf("follow-up during a break: %v (%s)", slot.Sub(noon), reason)
	}
}

func TestTypingDelay(t *testing.T) {
	for _, r := range []float64{0, 0.5, 0.999} {
		p := newPacer(&state{}, fixedRand(r))
		short, long := p.typingDelay("ok"), p.typingDelay(strings.Repeat("é", 300))
		if short < typingMin || short > time.Second || long != typingMax {
			t.Errorf("rng %v: typing %v for 2 characters, %v for 300", r, short, long)
		}
		if mid := p.typingDelay(strings.Repeat("a", 40)); mid <= short || mid >= long {
			t.Errorf("rng %v: typing 40 characters takes %v", r, mid)
		}
	}
}

func TestSessionBreak(t *testing.T) {
	t.Run("after a session's requests", func(t *testing.T) {
		p := newPacer(&state{}, fixedRand(0.5))
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
	t.Run("after a session's length", func(t *testing.T) {
		p := newPacer(&state{}, fixedRand(0.5))
		minutes := int(sessionLength / time.Minute)
		for i := range minutes + 1 {
			p.record(noon.Add(time.Duration(i) * time.Minute))
			if i < minutes && !p.st.BreakUntil.IsZero() {
				t.Fatalf("break after %d min", i)
			}
		}
		if p.st.BreakUntil.IsZero() {
			t.Fatalf("no break after %v of activity", sessionLength)
		}
		if p.st.SessionCount != 0 {
			t.Errorf("session not reset: %d", p.st.SessionCount)
		}
	})
	t.Run("idle starts a new session", func(t *testing.T) {
		p := newPacer(&state{}, fixedRand(0.5))
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
	p := newPacer(st, fixedRand(0.5))
	start := noon
	const step = time.Minute // dailyBudget of these fit in budgetWindow
	for i := range dailyBudget {
		st.Requests = append(st.Requests, start.Add(time.Duration(i)*step))
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
	next, reason := p.slot(slot.Add(time.Second), kindAPI, "r", time.Time{})
	if reason != "daily budget" || next.Before(start.Add(step+budgetWindow)) {
		t.Errorf("second slot %v (%s)", next, reason)
	}
}

func TestCooldownAndBuckets(t *testing.T) {
	st := &state{CooldownUntil: noon.Add(time.Hour)}
	p := newPacer(st, fixedRand(0.5))
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

// An approved send skips reading pauses and breaks but not the gap or the budget.
func TestSendPacing(t *testing.T) {
	st := &state{}
	p := newPacer(st, fixedRand(0.1)) // rolls a reading pause
	p.record(noon)
	st.BreakUntil = noon.Add(10 * time.Minute)
	if slot, reason := p.slot(noon, kindAPI, "r", time.Time{}); reason != "session break" || slot.Before(st.BreakUntil) {
		t.Errorf("read slot %v (%s), want after the break", slot, reason)
	}
	slot, reason := p.slot(noon, kindSend, "r", time.Time{})
	if want := gapMin + (gapMax-gapMin)/10; reason != "gap" || slot.Sub(noon) != want {
		t.Errorf("send slot %v (%s), want the plain %v gap", slot.Sub(noon), reason, want)
	}
	for range dailyBudget {
		st.Requests = append(st.Requests, noon)
	}
	if _, reason := p.slot(noon, kindSend, "r", time.Time{}); reason != "daily budget" {
		t.Errorf("send with the budget spent: %s", reason)
	}
}
