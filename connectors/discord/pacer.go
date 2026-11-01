package main

import (
	"context"
	"math/rand/v2"
	"strconv"
	"strings"
	"time"
)

// Every upstream request, including the daily page load, goes through the pacer.
const (
	gapMin      = 5 * time.Second // between requests
	gapMax      = 12 * time.Second
	pauseChance = 0.15 // chance of an extra reading pause on top of the gap
	pauseMin    = 15 * time.Second
	pauseMax    = 45 * time.Second

	idleAfter = 10 * time.Minute // a longer silence means the app was closed
	openMin   = 1 * time.Second  // "app open" delay of the first request after idle
	openMax   = 3 * time.Second

	// Subresources of the app page load right after the page, as in a browser.
	subresourceMin = 200 * time.Millisecond
	subresourceMax = 800 * time.Millisecond

	sessionRequests = 40 // a session ends after this many requests ...
	sessionLength   = 20 * time.Minute
	breakMin        = 5 * time.Minute // ... and is followed by a break
	breakMax        = 15 * time.Minute

	dailyBudget  = 1000 // upstream requests per rolling budgetWindow
	budgetWindow = 24 * time.Hour

	maxSlotWait  = 90 * time.Second // longest a gateway call waits for a slot
	linkSlotWait = 15 * time.Second // well inside the console's setup timeout

	rateLimitWindow    = time.Hour
	rateLimitTrips     = 3                 // 429s within rateLimitWindow that trigger a cooldown
	rateLimitMaxObey   = 300 * time.Second // longer retry_after triggers a cooldown
	cooldownLength     = 6 * time.Hour
	notReadyRetryAfter = 5 * time.Second // 202 without retry_after
)

type requestKind int

const (
	kindAPI         requestKind = iota // REST call or the app page itself
	kindSubresource                    // a script loaded by the app page
	// kindSend is a message the owner approved. It skips reading pauses and
	// session breaks; the gap, daily budget, cooldown and buckets still apply.
	kindSend
)

type clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type rng interface{ Float64() float64 }

type globalRand struct{}

func (globalRand) Float64() float64 { return rand.Float64() }

// Delays are rolled once after each request, so a refused call is told an
// honest, stable wait. Used only by the single upstream worker goroutine.
type pacer struct {
	st  *state
	rng rng

	gap, pause, open, subresource time.Duration // pre-rolled delays for the next request

	routeBuckets map[string]string    // route key -> X-RateLimit-Bucket
	exhausted    map[string]time.Time // bucket -> reset time while Remaining is 0
}

func newPacer(st *state, r rng) *pacer {
	p := &pacer{
		st:           st,
		rng:          r,
		routeBuckets: make(map[string]string),
		exhausted:    make(map[string]time.Time),
	}
	p.roll()
	return p
}

func (p *pacer) uniform(lo, hi time.Duration) time.Duration {
	return lo + time.Duration(p.rng.Float64()*float64(hi-lo))
}

func (p *pacer) roll() {
	p.gap, p.pause = p.uniform(gapMin, gapMax), 0
	if p.rng.Float64() < pauseChance {
		p.pause = p.uniform(pauseMin, pauseMax)
	}
	p.open = p.uniform(openMin, openMax)
	p.subresource = p.uniform(subresourceMin, subresourceMax)
}

func (p *pacer) slot(now time.Time, kind requestKind, route string, notBefore time.Time) (time.Time, string) {
	last := p.st.LastRequest
	var t time.Time
	reason := "gap"
	switch {
	case kind == kindSubresource && !last.IsZero():
		t = last.Add(p.subresource)
		reason = "page load"
	case last.IsZero() || now.Sub(last) > idleAfter:
		t = now.Add(p.open)
		reason = "app open"
	case kind == kindSend:
		t = last.Add(p.gap)
	default:
		t = last.Add(p.gap + p.pause)
	}
	if t.Before(now) {
		t = now
	}
	if notBefore.After(t) {
		t, reason = notBefore, "retry after"
	}
	long := false
	for range 16 {
		until, why, isLong := p.blocker(t, kind, route)
		if !until.After(t) {
			break
		}
		t, reason = until, why
		long = long || isLong
	}
	if long {
		// Coming back after a long block is opening the app again.
		t = t.Add(p.open)
	}
	return t, reason
}

// blocker returns when a hard limit stops blocking a request at t (t itself
// if none applies), its name, and whether it is a long block.
func (p *pacer) blocker(t time.Time, kind requestKind, route string) (time.Time, string, bool) {
	if p.st.CooldownUntil.After(t) {
		return p.st.CooldownUntil, "cooldown", true
	}
	if p.st.BreakUntil.After(t) && kind != kindSend {
		return p.st.BreakUntil, "session break", true
	}
	if w := since(p.st.Requests, t.Add(-budgetWindow)); len(w) >= dailyBudget {
		// Wait until enough old requests leave the window.
		return w[len(w)-dailyBudget].Add(budgetWindow + time.Millisecond), "daily budget", true
	}
	if until, ok := p.exhausted[p.bucket(route)]; ok && until.After(t) {
		return until, "rate limit bucket", false
	}
	return t, "", false
}

func (p *pacer) bucket(route string) string {
	if b, ok := p.routeBuckets[route]; ok {
		return b
	}
	return route
}

// record accounts for a request sent at: budget, session, and break.
func (p *pacer) record(at time.Time) {
	st := p.st
	st.prune(at)
	st.Requests = append(st.Requests, at)
	if st.SessionStart.IsZero() || at.Sub(st.LastRequest) > idleAfter {
		st.SessionStart, st.SessionCount = at, 0
	}
	st.SessionCount++
	st.LastRequest = at
	if st.SessionCount >= sessionRequests || at.Sub(st.SessionStart) >= sessionLength {
		st.BreakUntil = at.Add(p.uniform(breakMin, breakMax))
		st.SessionStart, st.SessionCount = time.Time{}, 0
	}
	p.roll()
}

// updateBucket applies X-RateLimit-* headers. A shared bucket is still
// tracked per major resource, as Discord does.
func (p *pacer) updateBucket(now time.Time, route, bucket, remaining, resetAfter string) {
	if remaining == "" {
		return
	}
	key := route
	if bucket != "" {
		key = bucket + "|" + route[strings.LastIndexByte(route, '|')+1:]
		p.routeBuckets[route] = key
	}
	rem, err := strconv.Atoi(remaining)
	if err != nil {
		return
	}
	if rem > 0 {
		delete(p.exhausted, key)
		return
	}
	secs, err := strconv.ParseFloat(resetAfter, 64)
	if err != nil || secs <= 0 {
		return
	}
	p.exhausted[key] = now.Add(time.Duration(secs * float64(time.Second)))
}

// rest returns when the long blocks in force at now end, and the first one's
// name; zero if none applies.
func (p *pacer) rest(now time.Time) (time.Time, string) {
	var until time.Time
	var reason string
	t := now
	for range 16 {
		next, why, long := p.blocker(t, kindAPI, "")
		if !long || !next.After(t) {
			break
		}
		if reason == "" {
			reason = why
		}
		t, until = next, next
	}
	return until, reason
}
