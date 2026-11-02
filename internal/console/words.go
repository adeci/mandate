package console

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/internal/proposal"
)

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// windowText says when g is open, in the server's local time for windows.
func windowText(g approval.Grant) string {
	if g.Schedule == nil {
		return "until " + g.Until.Local().Format(stamp)
	}
	sc := g.Schedule
	return fmt.Sprintf("%s at %s for %s (%s)", daysText(sc.Days), sc.Start, span(sc.Length), sc.Zone)
}

// routineText says when a requested routine runs; the zone is the
// approver's.
func routineText(sc proposal.RequestSchedule) string {
	return fmt.Sprintf("%s at %s for %s (your time zone)", daysText(sc.Days), sc.Start, span(sc.Length))
}

func daysText(days []time.Weekday) string {
	set := slices.Sorted(slices.Values(days))
	set = slices.Compact(set)
	switch {
	case len(set) == 0 || len(set) == 7:
		return "every day"
	case slices.Equal(set, []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}):
		return "weekdays"
	case slices.Equal(set, []time.Weekday{time.Sunday, time.Saturday}):
		return "weekends"
	}
	out := make([]string, len(set))
	for i, d := range set {
		out[i] = d.String()[:3]
	}
	return strings.Join(out, ", ")
}

func horizonText(h time.Duration) string {
	if h == 0 {
		return "all history"
	}
	return "the last " + span(h)
}

// span says d in the largest whole unit.
func span(d time.Duration) string {
	unit := func(n int64, name string) string {
		if n == 1 {
			return "1 " + name
		}
		return strconv.FormatInt(n, 10) + " " + name + "s"
	}
	switch {
	case d%(24*time.Hour) == 0:
		return unit(int64(d/(24*time.Hour)), "day")
	case d%time.Hour == 0:
		return unit(int64(d/time.Hour), "hour")
	case d%time.Minute == 0:
		return unit(int64(d/time.Minute), "minute")
	}
	return d.String()
}

// short is a span for buttons: 30m, 2h, 7d.
func short(d time.Duration) string {
	switch {
	case d == 0:
		return "all"
	case d%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	case d%time.Hour == 0:
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
}

// shortID is the end of an ID, enough to tell two chats of one name apart.
func shortID(id string) string {
	if r := []rune(id); len(r) > 12 {
		return "…" + string(r[len(r)-10:])
	}
	return id
}

// option is a choice in a select.
type option struct {
	Value    string `json:"value"`
	Label    string `json:"label"`
	Selected bool   `json:"selected,omitempty"`
}

var (
	forChoices     = []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 8 * time.Hour, 12 * time.Hour}
	grantChoices   = append(slices.Clone(forChoices), 24*time.Hour, 7*24*time.Hour, 30*24*time.Hour)
	horizonChoices = []time.Duration{24 * time.Hour, 7 * 24 * time.Hour, 30 * 24 * time.Hour, 0}
)

// spanOptions offers choices up to most (any when 0) and chosen itself,
// selecting chosen.
func spanOptions(choices []time.Duration, most, chosen time.Duration, label func(time.Duration) string) []option {
	var out []option
	add := func(d time.Duration) {
		out = append(out, option{Value: d.String(), Label: label(d), Selected: d == chosen})
	}
	seen := false
	for _, d := range choices {
		if most > 0 && (d == 0 || d > most) {
			continue
		}
		seen = seen || d == chosen
		add(d)
	}
	if !seen {
		add(chosen)
	}
	return out
}
