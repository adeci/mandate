package console

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	return scheduleWords(sc) + " (your time zone)"
}

// scheduleWords says a routine's runs: "weekdays 07:00 for 20 min".
func scheduleWords(sc proposal.RequestSchedule) string {
	return daysText(sc.Days) + " " + sc.Start + " for " + span(sc.Length)
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

// horizonText says how far back a grant reads: "last day", "last 7 days",
// "all history".
func horizonText(h time.Duration) string {
	switch h {
	case 0:
		return "all history"
	case 24 * time.Hour:
		return "last day"
	}
	return "last " + span(h)
}

// span says d in the largest whole unit: "20 min", "1 hour", "7 days".
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
		return strconv.FormatInt(int64(d/time.Minute), 10) + " min"
	}
	return d.String()
}

// shortID is enough of a chat's ID to tell two chats of one name apart: a
// DM's phone number, or the last 6 letters and digits, past any base64
// padding.
func shortID(id string) string {
	local, domain, _ := strings.Cut(id, "@")
	if phone := strings.TrimPrefix(local, "+"); isDigits(phone) && len(phone) >= 8 && len(phone) <= 15 &&
		(domain == "" && phone != local || domain == "s.whatsapp.net" || domain == "c.us") {
		return formatPhone(phone)
	}
	base := id
	if domain != "" {
		base = local
	}
	r := []rune(strings.TrimRightFunc(base, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }))
	if len(r) > 6 {
		return "…" + string(r[len(r)-6:])
	}
	return id
}

// statusWord says a proposal status in the owner's words.
func statusWord(status any) string {
	switch s := fmt.Sprint(status); s {
	case "rejected":
		return "denied"
	case "pending":
		return "waiting"
	default:
		return s
	}
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// formatPhone groups an international number: the country code, three
// digits, the middle, and the last four, e.g. +49 151 2345 6789.
func formatPhone(digits string) string {
	cc := 3
	switch {
	case digits[0] == '1' || digits[0] == '7':
		cc = 1
	case slices.Contains(twoDigitCodes, digits[:2]):
		cc = 2
	}
	rest := digits[cc:]
	if len(rest) <= 7 {
		return "+" + digits[:cc] + " " + rest
	}
	return "+" + digits[:cc] + " " + rest[:3] + " " + rest[3:len(rest)-4] + " " + rest[len(rest)-4:]
}

// twoDigitCodes are the two-digit country calling codes; 1 and 7 have one
// digit and the rest three.
var twoDigitCodes = strings.Fields("20 27 30 31 32 33 34 36 39 40 41 43 44 45 46 47 48 49 51 52 53 54 55 56 57 58 60 61 62 63 64 65 66 81 82 84 86 90 91 92 93 94 95 98")

// connectorTitles name the built-in connectors in sentences.
var connectorTitles = map[string]string{
	"whatsapp": "WhatsApp", "signal": "Signal", "discord": "Discord", "fastmail": "Fastmail",
	"gmessages": "Google Messages", "matrix": "Matrix", "http": "HTTP",
}

func connectorTitle(name string) string { return cmpOr(connectorTitles[name], name) }

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
