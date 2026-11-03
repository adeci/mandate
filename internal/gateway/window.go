package gateway

import (
	"slices"
	"time"

	"github.com/adeci/mandate/record"
)

// writeGrace is how long after a grant's window ends the agent may still
// propose writes within it.
const writeGrace = 24 * time.Hour

// open reports whether g is open at now, and when its current window closes
// or, while closed, when the next one opens (zero if none will).
func open(g record.Grant, now time.Time) (bool, time.Time) {
	s := g.Schedule
	if s == nil {
		switch {
		case now.Before(g.From.Time):
			return false, g.From.Time
		case now.Before(g.Until.Time):
			return true, g.Until.Time
		}
		return false, time.Time{}
	}
	// record verified the start and zone.
	loc, err := time.LoadLocation(s.Zone)
	if err != nil {
		return false, time.Time{}
	}
	start, _ := time.Parse("15:04", s.Start)
	length := time.Duration(s.Length) * time.Second
	local := now.In(loc)
	// A window open now started today or, running past midnight, yesterday;
	// the next one starts within two weeks, as the clocks may skip a weekly
	// start.
	var next time.Time
	for d := -1; d <= 14; d++ {
		day := time.Date(local.Year(), local.Month(), local.Day()+d, 12, 0, 0, 0, loc)
		if len(s.Days) > 0 && !slices.Contains(s.Days, int(day.Weekday())) {
			continue
		}
		at, ok := wallClock(day, start.Hour(), start.Minute(), loc)
		switch {
		case !ok:
		case !now.Before(at) && now.Before(at.Add(length)):
			return true, at.Add(length)
		case at.After(now) && next.IsZero():
			next = at
		}
	}
	return false, next
}

// wallClock is the first instant reading hour:minute on day's date in loc,
// or false when the clocks skip that time that day. time.Date would move a
// skipped time and pick either of a repeated one. Each occurrence is at one
// of the offsets in effect around that day, as zones change at most once a
// day.
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

// lastEnd is when the latest window of g open within writeGrace before now
// closes, which is after now while it is still open. Windows before the
// grant was signed don't count, so a new or resumed routine has no grace
// until its first window has run.
func lastEnd(g record.Grant, now time.Time) (time.Time, bool) {
	var end time.Time
	t := now.Add(-writeGrace)
	if g.CreatedAt.After(t) {
		t = g.CreatedAt.Time
	}
	// A grant has at most one window a day, so a few steps cover the span.
	for range 8 {
		isOpen, at := open(g, t)
		switch {
		case isOpen:
			end, t = at, at
			if !at.Before(now) {
				return end, true
			}
		case at.IsZero() || at.After(now):
			return end, !end.IsZero()
		default:
			t = at
		}
	}
	return end, !end.IsZero()
}

// access is an identity's access to a connector now: open until, or the
// next time a grant opens.
type access struct {
	Open  bool      `json:"open"`
	Until time.Time `json:"until,omitzero"`
	Next  time.Time `json:"next,omitzero"`
}

func accessNow(grants []record.Grant, now time.Time) access {
	var a access
	for _, g := range grants {
		isOpen, at := open(g, now)
		switch {
		case isOpen:
			a.Open = true
			if at.After(a.Until) {
				a.Until = at.UTC()
			}
		case !at.IsZero() && (a.Next.IsZero() || at.Before(a.Next)):
			a.Next = at.UTC()
		}
	}
	if a.Open {
		a.Next = time.Time{}
	}
	return a
}

// sentAt is how the store writes times, so they compare as text.
const sentAt = "2006-01-02T15:04:05.000Z"

// readScope is the rules of the grants open at now.
func readScope(grants []record.Grant, now time.Time) []rule {
	var rules []rule
	for _, g := range grants {
		if isOpen, _ := open(g, now); isOpen {
			rules = append(rules, ruleAt(g, now, false))
		}
	}
	return rules
}

// writeScope is the rules of the grants open at now or whose window ended
// within writeGrace. An ended grant shows what it showed when it ended.
func writeScope(grants []record.Grant, now time.Time) []rule {
	var rules []rule
	for _, g := range grants {
		end, ok := lastEnd(g, now)
		switch {
		case !ok:
		case end.After(now):
			rules = append(rules, ruleAt(g, now, false))
		default:
			rules = append(rules, ruleAt(g, end, true))
		}
	}
	return rules
}

func ruleAt(g record.Grant, at time.Time, ended bool) rule {
	r := rule{Chats: g.Chats}
	if g.Horizon > 0 {
		r.Cutoff = at.Add(-time.Duration(g.Horizon) * time.Second).UTC().Format(sentAt)
	}
	if ended {
		r.Until = at.UTC().Format(sentAt)
	}
	return r
}
