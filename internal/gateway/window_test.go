package gateway

import (
	"testing"
	"time"
	_ "time/tzdata"

	"github.com/adeci/mandate/record"
)

// Routine windows open in the owner's zone, across midnight and both clock
// changes of Europe/Berlin in 2026, and grant a day's grace after they close.
func TestRoutineWindows(t *testing.T) {
	utc := func(s string) time.Time {
		at, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	routine := func(start string, length int, days ...int) record.Grant {
		return record.Grant{
			Schedule:  &record.Schedule{Days: days, Start: start, Length: length, Zone: "Europe/Berlin"},
			CreatedAt: record.Time{Time: utc("2026-01-01T00:00:00Z")},
		}
	}
	signedLate := routine("07:00", 1200)
	signedLate.CreatedAt = record.Time{Time: utc("2026-11-03T06:25:00Z")}
	for _, tc := range []struct {
		name string
		g    record.Grant
		now  string
		open bool
		// at is when the window closes, or the next opens.
		at string
		// end is lastEnd's answer, "" for none.
		end string
	}{
		{"inside", routine("07:00", 1200), "2026-11-03T06:10:00Z", true, "2026-11-03T06:20:00Z", "2026-11-03T06:20:00Z"},
		{"after, within the grace", routine("07:00", 1200), "2026-11-03T06:30:00Z", false, "2026-11-04T06:00:00Z", "2026-11-03T06:20:00Z"},
		{"opened yesterday, past midnight", routine("23:30", 3600), "2026-11-03T23:15:00Z", true, "2026-11-03T23:30:00Z", "2026-11-03T23:30:00Z"},
		{"start skipped by the clocks", routine("02:30", 1200), "2026-03-29T00:00:00Z", false, "2026-03-30T00:30:00Z", "2026-03-28T01:50:00Z"},
		{"start repeated, first time", routine("02:30", 1200), "2026-10-25T00:40:00Z", true, "2026-10-25T00:50:00Z", "2026-10-25T00:50:00Z"},
		{"start repeated, second time", routine("02:30", 1200), "2026-10-25T01:40:00Z", false, "2026-10-26T01:30:00Z", "2026-10-25T00:50:00Z"},
		{"Mondays, on a Wednesday", routine("07:00", 1200, 1), "2026-11-04T06:10:00Z", false, "2026-11-09T06:00:00Z", ""},
		{"signed after the day's window", signedLate, "2026-11-03T06:30:00Z", false, "2026-11-04T06:00:00Z", ""},
	} {
		now := utc(tc.now)
		isOpen, at := open(tc.g, now)
		if isOpen != tc.open || !at.Equal(utc(tc.at)) {
			t.Errorf("%s: open %v until %v, want %v %s", tc.name, isOpen, at.UTC(), tc.open, tc.at)
		}
		end, ok := lastEnd(tc.g, now)
		if ok != (tc.end != "") || ok && !end.Equal(utc(tc.end)) {
			t.Errorf("%s: last end %v %v, want %q", tc.name, end.UTC(), ok, tc.end)
		}
	}
}
