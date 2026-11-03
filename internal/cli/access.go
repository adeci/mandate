package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	accessUsage  = "mandate access [--json]"
	requestUsage = "mandate request CONNECTOR [--chat TEXT]... [--for 30m] [--since 7d] --reason WHY [--wait 15m]"
	routineUsage = "mandate request CONNECTOR [--chat TEXT]... --every daily|weekdays|weekends|mon,wed,... --at HH:MM [--for 30m] [--since 7d] --reason WHY [--wait 15m]"
	extendUsage  = "mandate request CONNECTOR --extend [--for 30m] --reason WHY [--wait 15m]"
	// requestUsages is every form of mandate request, one per line.
	requestUsages = requestUsage + "\n  " + routineUsage + "\n  " + extendUsage
)

// defaultWait is how long request, status and a proposal wait for the
// owner unless --wait says otherwise.
const defaultWait = 15 * time.Minute

// timeFormat is how access times print: in UTC, like the rest of the output,
// and marked so, since agents pass these times on to the owner.
const timeFormat = "2006-01-02 15:04 UTC"

// window is read access now, as the gateway reports it.
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

type schedule struct {
	Days   []int  `json:"days"`
	Start  string `json:"start"`
	Length int    `json:"length"`
	Zone   string `json:"zone,omitempty"`
}

type grantInfo struct {
	ID        string `json:"id"`
	Connector string `json:"connector"`
	Chats     []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"chats"`
	From     time.Time `json:"from"`
	Until    time.Time `json:"until"`
	Schedule *schedule `json:"schedule"`
	Horizon  int       `json:"horizon"`
	Now      window    `json:"now"`
}

// access prints the identity's grants and whether each is open now.
func (c *cli) access(ctx context.Context, args []string) error {
	if _, err := c.parse(flag.NewFlagSet("access", flag.ContinueOnError), accessUsage, args, 0, 0); err != nil {
		return err
	}
	var grants []grantInfo
	raw, _, err := c.do(ctx, requestTimeout, http.MethodGet, "/agent/v1/access", nil, &grants)
	if err != nil {
		return err
	}
	if c.json {
		return c.printJSON(raw)
	}
	t := table{Columns: []string{"connector", "now", "chats", "when", "since", "id"}}
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
			since = "last " + human(seconds(g.Horizon))
		}
		t.Rows = append(t.Rows, []any{g.Connector, g.Now.String(), chats, when(g), since, g.ID})
	}
	if err := c.printTable(t); err != nil {
		return err
	}
	if len(grants) == 0 {
		fmt.Fprintln(c.errOut, "no grants: reads are closed. If a task needs one, ask with "+requestUsage)
	}
	return nil
}

func seconds(n int) time.Duration { return time.Duration(n) * time.Second }

// when says in words when a grant is open.
func when(g grantInfo) string {
	s := g.Schedule
	if s == nil {
		return g.From.UTC().Format(timeFormat) + " to " + g.Until.UTC().Format(timeFormat)
	}
	return fmt.Sprintf("every %s at %s for %s (%s)", days(s.Days), s.Start, human(seconds(s.Length)), s.Zone)
}

var (
	weekdays = []int{1, 2, 3, 4, 5}
	weekends = []int{0, 6}
)

// days names a schedule's days as --every takes them.
func days(ds []int) string {
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
		names[i] = time.Weekday(d).String()[:3]
	}
	return strings.Join(names, ",")
}

// parseEvery reads --every: daily, weekdays, weekends, or days such as
// mon,wed (or monday,wednesday).
func parseEvery(v string) ([]int, error) {
	switch strings.ToLower(v) {
	case "daily":
		return []int{}, nil
	case "weekdays":
		return weekdays, nil
	case "weekends":
		return weekends, nil
	}
	var out []int
	for part := range strings.SplitSeq(strings.ToLower(v), ",") {
		part = strings.TrimSpace(part)
		d := slices.IndexFunc([]int{0, 1, 2, 3, 4, 5, 6}, func(d int) bool {
			return len(part) >= 3 && strings.HasPrefix(strings.ToLower(time.Weekday(d).String()), part)
		})
		if d < 0 {
			return nil, fmt.Errorf("--every: unknown day %q; use daily, weekdays, weekends or days like mon,wed", part)
		}
		out = append(out, d)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

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
	Connector string    `json:"connector"`
	Chats     []string  `json:"chats,omitempty"`
	For       *int      `json:"for,omitempty"`
	Horizon   *int      `json:"horizon,omitempty"`
	Schedule  *schedule `json:"schedule,omitempty"`
	Extend    bool      `json:"extend,omitempty"`
	Reason    string    `json:"reason"`
}

// request asks the owner for read access and waits for the decision.
func (c *cli) request(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("request", flag.ContinueOnError)
	req := accessRequest{}
	fs.Func("chat", "", func(v string) error {
		req.Chats = append(req.Chats, v)
		return nil
	})
	var length time.Duration
	fs.Func("for", "", func(v string) error {
		d, err := parseDays(v)
		if err == nil && d < time.Second {
			err = errors.New("--for must be a second or more")
		}
		length = d
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
		n := int(d / time.Second)
		req.Horizon = &n
		return err
	})
	var every []int
	fs.Func("every", "", func(v string) error {
		var err error
		every, err = parseEvery(v)
		return err
	})
	at := fs.String("at", "", "")
	fs.BoolVar(&req.Extend, "extend", false, "")
	fs.StringVar(&req.Reason, "reason", "", "")
	wait := fs.Duration("wait", defaultWait, "")
	pos, err := c.parse(fs, requestUsages, args, 1, 1)
	if err != nil {
		return err
	}
	routine := every != nil
	switch {
	case *wait < 0:
		return usage(requestUsages, errNegativeWait)
	case strings.TrimSpace(req.Reason) == "":
		return usage(requestUsages, errors.New("--reason is required: say what the task is and why it needs this"))
	case req.Extend && (len(req.Chats) > 0 || req.Horizon != nil || routine || *at != ""):
		return usage(extendUsage, errors.New("--extend keeps the chats and horizon of the window it extends"))
	case routine != (*at != ""):
		return usage(routineUsage, errors.New("--every and --at go together"))
	}
	req.Connector = pos[0]
	switch {
	case routine:
		n := 1800
		if length > 0 {
			n = int(length / time.Second)
		}
		req.Schedule = &schedule{Days: every, Start: *at, Length: n}
	case length > 0:
		n := int(length / time.Second)
		req.For = &n
	}
	var ref struct {
		ID string `json:"id"`
	}
	raw, _, err := c.do(ctx, requestTimeout, http.MethodPost, "/agent/v1/access/requests", req, &ref)
	if err != nil {
		return err
	}
	if c.json {
		if err := c.printJSON(raw); err != nil {
			return err
		}
	} else {
		what := "read access to " + req.Connector
		switch {
		case req.Schedule != nil:
			what = fmt.Sprintf("a routine on %s, every %s at %s for %s", req.Connector, days(req.Schedule.Days), req.Schedule.Start, human(seconds(req.Schedule.Length)))
		case req.Extend:
			what = "more time on " + req.Connector
		}
		fmt.Fprintf(c.out, "%s pending: %s\n", ref.ID, what)
	}
	if *wait > 0 {
		fmt.Fprintf(c.errOut, "waiting up to %s for the owner to decide in the mandate app\n", *wait)
	}
	return c.awaitAccess(ctx, ref.ID, req.Connector, *wait)
}

// awaitAccess long-polls the request until the owner decides or wait runs
// out, and maps the outcome to the exit code.
func (c *cli) awaitAccess(ctx context.Context, id, connector string, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for {
		q, timeout := waitQuery(deadline)
		var st struct {
			Status string    `json:"status"`
			Until  time.Time `json:"until"`
		}
		if _, _, err := c.do(ctx, timeout, http.MethodGet, "/agent/v1/access/requests/"+url.PathEscape(id)+q, nil, &st); err != nil {
			return err
		}
		switch st.Status {
		case "pending":
			if time.Now().Before(deadline) {
				continue
			}
			msg := fmt.Sprintf("request %s is still pending", id)
			if wait > 0 {
				msg += " after " + human(wait)
			}
			msg += "; make sure the owner knows it waits in the mandate app, and check with mandate access later. Don't ask again meanwhile"
			return &exitError{msg: msg, code: errTryLater}
		case "granted":
			switch {
			case c.json:
			case st.Until.IsZero():
				fmt.Fprintf(c.out, "%s granted: a routine on %s; mandate access shows when it runs\n", id, connector)
			default:
				fmt.Fprintf(c.out, "%s granted: read %s until %s\n", id, connector, st.Until.UTC().Format(timeFormat))
			}
			return nil
		case "denied":
			return fmt.Errorf("request %s was denied by the owner; don't ask again unless they say so", id)
		case "expired":
			return fmt.Errorf("request %s expired before the owner decided; ask again only if the task still needs it", id)
		}
		return fmt.Errorf("request %s has status %q; tell the owner", id, st.Status)
	}
}
