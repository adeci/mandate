package client

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/internal/exitcode"
	"github.com/adeci/mandate/internal/proposal"
)

const (
	accessUsage  = "mandate access"
	requestUsage = "mandate request CONNECTOR [--chat TEXT]... [--for 30m] [--since 7d] --reason WHY [--wait DUR]"
	routineUsage = "mandate request CONNECTOR [--chat TEXT]... --every daily|weekdays|weekends|mon,wed,... --at HH:MM [--for 30m] [--since 7d] --reason WHY [--wait DUR]"
	extendUsage  = "mandate request CONNECTOR --extend [--for 30m] --reason WHY [--wait DUR]"
	// requestUsages is every form of mandate request, one per line.
	requestUsages = requestUsage + "\n  " + routineUsage + "\n  " + extendUsage
)

// timeFormat is how access times print; like the rest of the output, in UTC.
const timeFormat = "2006-01-02 15:04"

func Access(ctx context.Context, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	return c.access(ctx, args)
}

func Request(ctx context.Context, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	return c.request(ctx, args)
}

// window is a connector's read access now, as the gateway reports it.
type window struct {
	Open  bool      `json:"open"`
	Until time.Time `json:"until"`
	Next  time.Time `json:"next"`
}

func (w window) String() string {
	switch {
	case w.Open:
		return "open until " + w.Until.UTC().Format(timeFormat)
	case !w.Next.IsZero():
		return "closed, opens " + w.Next.UTC().Format(timeFormat)
	}
	return "closed"
}

type grantInfo struct {
	ID        string `json:"id"`
	Connector string `json:"connector"`
	Chats     []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"chats"`
	From     time.Time          `json:"from"`
	Until    time.Time          `json:"until"`
	Schedule *approval.Schedule `json:"schedule"`
	Horizon  time.Duration      `json:"horizon"`
	Note     string             `json:"note"`
	Now      window             `json:"now"`
}

// access prints the agent's grants and whether each is open now.
func (c *client) access(ctx context.Context, args []string) error {
	if _, err := c.parse(flag.NewFlagSet("access", flag.ContinueOnError), accessUsage, args, 0, 0); err != nil {
		return err
	}
	_, _, raw, err := c.do(ctx, requestTimeout, http.MethodGet, "/v1/access", nil)
	if err != nil {
		return err
	}
	if c.json {
		_, err := c.out.Write(append(bytes.TrimRight(raw, "\n"), '\n'))
		return err
	}
	var grants []grantInfo
	if err := json.Unmarshal(raw, &grants); err != nil {
		return err
	}
	t := table{Columns: []string{"connector", "now", "chats", "when", "since", "id", "note"}, Rows: [][]any{}}
	for _, g := range grants {
		chats := "all"
		if len(g.Chats) > 0 {
			names := make([]string, len(g.Chats))
			for i, ch := range g.Chats {
				names[i] = ch.ID
				if ch.Name != "" {
					names[i] = ch.Name + " (" + ch.ID + ")"
				}
			}
			chats = strings.Join(names, ", ")
		}
		since := "all history"
		if g.Horizon > 0 {
			since = "last " + human(g.Horizon)
		}
		t.Rows = append(t.Rows, []any{g.Connector, g.Now.String(), chats, when(g), since, g.ID, g.Note})
	}
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if err := c.table(b); err != nil {
		return err
	}
	if len(grants) == 0 {
		fmt.Fprintln(c.errOut, "no grants: reads are closed. If a task needs one, ask with "+requestUsage)
	}
	return nil
}

// when says in words when a grant is open.
func when(g grantInfo) string {
	s := g.Schedule
	if s == nil {
		return g.From.UTC().Format(timeFormat) + " to " + g.Until.UTC().Format(timeFormat)
	}
	return fmt.Sprintf("every %s at %s for %s (%s)", days(s.Days), s.Start, human(s.Length), s.Zone)
}

var (
	weekdays = []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}
	weekends = []time.Weekday{time.Sunday, time.Saturday}
)

// days names a schedule's days as --every takes them.
func days(ds []time.Weekday) string {
	switch {
	case len(ds) == 0 || len(ds) == 7:
		return "day"
	case slices.Equal(ds, weekdays):
		return "weekday"
	case slices.Equal(ds, weekends):
		return "weekend"
	}
	names := make([]string, len(ds))
	for i, d := range ds {
		names[i] = d.String()[:3]
	}
	return strings.Join(names, ",")
}

// parseEvery reads --every: daily, weekdays, weekends, or days such as
// mon,wed (or monday,wednesday).
func parseEvery(v string) ([]time.Weekday, error) {
	switch strings.ToLower(v) {
	case "daily":
		return nil, nil
	case "weekdays":
		return weekdays, nil
	case "weekends":
		return weekends, nil
	}
	var out []time.Weekday
	for part := range strings.SplitSeq(strings.ToLower(v), ",") {
		part = strings.TrimSpace(part)
		i := slices.IndexFunc(allDays, func(d time.Weekday) bool {
			name := strings.ToLower(d.String())
			return len(part) >= 3 && strings.HasPrefix(name, part)
		})
		if i < 0 {
			return nil, fmt.Errorf("--every: unknown day %q; use daily, weekdays, weekends or days like mon,wed", part)
		}
		out = append(out, allDays[i])
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

var allDays = []time.Weekday{time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday}

// human writes whole days as 7d and drops zero units, as in 1h30m.
func human(d time.Duration) string {
	if d > 0 && d%(24*time.Hour) == 0 {
		return strconv.FormatInt(int64(d/(24*time.Hour)), 10) + "d"
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// parseDays is time.ParseDuration plus whole days, as in 7d.
func parseDays(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.Atoi(n)
		if err != nil || days < 0 {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

type accessRequest struct {
	Connector string                    `json:"connector"`
	Chats     []string                  `json:"chats,omitempty"`
	For       time.Duration             `json:"for,omitempty"`
	Horizon   *time.Duration            `json:"horizon,omitempty"`
	Schedule  *proposal.RequestSchedule `json:"schedule,omitempty"`
	Extend    bool                      `json:"extend,omitempty"`
	Reason    string                    `json:"reason"`
}

// request asks the owner for read access and, like status, waits for the
// decision.
func (c *client) request(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	req := accessRequest{}
	fs.Func("chat", "", func(v string) error {
		req.Chats = append(req.Chats, v)
		return nil
	})
	fs.Func("for", "", func(v string) error {
		d, err := parseDays(v)
		if err == nil && d <= 0 {
			err = errors.New("--for must be positive")
		}
		req.For = d
		return err
	})
	fs.Func("since", "", func(v string) error {
		var d time.Duration
		var err error
		if v != "all" {
			d, err = parseDays(v)
		}
		if err == nil && d < 0 {
			err = errors.New("--since must not be negative")
		}
		req.Horizon = &d
		return err
	})
	var every []time.Weekday
	everySet := false
	fs.Func("every", "", func(v string) error {
		d, err := parseEvery(v)
		every, everySet = d, true
		return err
	})
	at := fs.String("at", "", "")
	fs.BoolVar(&req.Extend, "extend", false, "")
	fs.StringVar(&req.Reason, "reason", "", "")
	wait := fs.Duration("wait", 0, "")
	pos, err := c.parse(fs, requestUsages, args, 1, 1)
	if err != nil {
		return err
	}
	switch {
	case *wait < 0:
		return usage(requestUsages, errNegativeWait)
	case strings.TrimSpace(req.Reason) == "":
		return usage(requestUsages, errors.New("--reason is required: say what the task is and why it needs this"))
	case req.Extend && (len(req.Chats) > 0 || req.Horizon != nil || everySet || *at != ""):
		return usage(extendUsage, errors.New("--extend keeps the chats and horizon of the window it extends"))
	case everySet != (*at != ""):
		return usage(routineUsage, errors.New("--every and --at go together"))
	}
	if everySet {
		req.Schedule = &proposal.RequestSchedule{Days: every, Start: *at, Length: cmp.Or(req.For, 30*time.Minute)}
		req.For = 0
	}
	req.Connector = pos[0]
	_, _, raw, err := c.do(ctx, requestTimeout, http.MethodPost, "/v1/access/requests", req)
	if err != nil {
		return err
	}
	var ref proposal.RequestRef
	if err := json.Unmarshal(raw, &ref); err != nil {
		return err
	}
	if c.json {
		if _, err := c.out.Write(append(bytes.TrimRight(raw, "\n"), '\n')); err != nil {
			return err
		}
	} else {
		what := "read access to " + req.Connector
		switch {
		case req.Schedule != nil:
			what = fmt.Sprintf("a routine on %s, every %s at %s for %s", req.Connector, days(req.Schedule.Days), req.Schedule.Start, human(req.Schedule.Length))
		case req.Extend:
			what = "more time on " + req.Connector
		}
		fmt.Fprintf(c.out, "%s pending: %s\napprove: %s\n", ref.ID, what, ref.URL)
	}
	if *wait > 0 {
		fmt.Fprintf(c.errOut, "waiting up to %s for the owner to decide\n", *wait)
	}
	return c.awaitAccess(ctx, ref.ID, req.Connector, *wait)
}

// awaitAccess long-polls the request until the owner decides or wait runs
// out, and maps the outcome to the exit code.
func (c *client) awaitAccess(ctx context.Context, id, connector string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		path := "/v1/access/requests/" + url.PathEscape(id)
		poll := min(time.Until(deadline), maxPoll)
		var secs int
		if poll > 0 {
			secs = int(math.Ceil(poll.Seconds()))
			path += "?wait=" + strconv.Itoa(secs)
		}
		_, _, raw, err := c.do(ctx, time.Duration(secs)*time.Second+requestTimeout, http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		var st proposal.AccessStatus
		if err := json.Unmarshal(raw, &st); err != nil {
			return err
		}
		if st.Status == proposal.AccessPending && time.Now().Before(deadline) {
			continue
		}
		switch st.Status {
		case proposal.AccessGranted:
			switch {
			case c.json:
			case st.Until.IsZero():
				fmt.Fprintf(c.out, "%s granted: a routine on %s; mandate access shows when it runs\n", id, connector)
			default:
				fmt.Fprintf(c.out, "%s granted: read %s until %s\n", id, connector, st.Until.UTC().Format(timeFormat))
			}
			return nil
		case proposal.AccessPending:
			msg := fmt.Sprintf("request %s is still pending", id)
			if wait > 0 {
				msg += " after " + wait.String()
			}
			msg += "; send the owner the approve link, and check with mandate access later. Don't ask again meanwhile"
			return &exitError{msg: msg, code: exitcode.ErrTryLater}
		case proposal.AccessDenied:
			return fmt.Errorf("request %s was denied by the owner; don't ask again unless they say so", id)
		case proposal.AccessExpired:
			return fmt.Errorf("request %s expired before the owner decided; ask again only if the task still needs it", id)
		}
		return fmt.Errorf("request %s has status %q; tell the owner", id, st.Status)
	}
}
