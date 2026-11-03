package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// gateway fakes the agent API: each route answers its replies in turn
// ("202 {...}") and the gateway records what it got.
type gateway struct {
	t       *testing.T
	mu      sync.Mutex
	replies map[string][]string // by "METHOD path"
	got     []string            // "METHOD path?query body"
}

func newGateway(t *testing.T, replies map[string][]string) (*cli, *gateway) {
	t.Helper()
	g := &gateway{t: t, replies: replies}
	srv := httptest.NewServer(g)
	t.Cleanup(srv.Close)
	t.Cleanup(func() {
		for k, left := range g.replies {
			if len(left) > 0 {
				t.Errorf("%s: %d replies left", k, len(left))
			}
		}
	})
	env := map[string]string{"MANDATE_URL": srv.URL + "/", "MANDATE_TOKEN": "secret", "HOME": t.TempDir()}
	return &cli{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, getenv: func(k string) string { return env[k] }}, g
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if a := r.Header.Get("Authorization"); a != "Bearer secret" {
		g.t.Errorf("%s %s: authorization %q", r.Method, r.URL, a)
	}
	key := r.Method + " " + r.URL.Path
	g.mu.Lock()
	defer g.mu.Unlock()
	g.got = append(g.got, strings.TrimSpace(r.Method+" "+r.URL.RequestURI()+" "+string(body)))
	left := g.replies[key]
	if len(left) == 0 {
		g.t.Errorf("unexpected %s", key)
		http.Error(w, `{"error":"unexpected"}`, http.StatusInternalServerError)
		return
	}
	g.replies[key] = left[1:]
	status, reply, _ := strings.Cut(left[0], " ")
	code, _ := strconv.Atoi(status)
	w.WriteHeader(code)
	w.Write([]byte(reply))
}

func output(c *cli) (string, string) {
	return c.out.(*bytes.Buffer).String(), c.errOut.(*bytes.Buffer).String()
}

func TestRead(t *testing.T) {
	t.Parallel()
	// A multi-line body, and an OSC title change and a C1 CSI from a
	// third party.
	const rows = `{"columns":["seq","sender","body"],"rows":[[7,"~Mom\u001b]0;pwned\u0007","see you\nat 5\t\\o/"],[8,"me","a\u009b2Jb"]],"more":true,"next_before":7}`
	c, g := newGateway(t, map[string][]string{"GET /agent/v1/signal/messages": {"200 " + rows, "200 " + rows}})
	if err := c.run(t.Context(), []string{"signal", "messages", "Fam+1=", "--limit", "2"}); err != nil {
		t.Fatal(err)
	}
	out, errOut := output(c)
	want := "seq\tsender\tbody\n7\t~Mom\\x1b]0;pwned\\x07\tsee you\\nat 5\\t\\\\o/\n8\tme\ta\\u009b2Jb\n"
	if out != want || errOut != "next: --before 7\n" {
		t.Errorf("TSV:\n%q\nwant\n%q\nstderr %q", out, want, errOut)
	}
	if g.got[0] != "GET /agent/v1/signal/messages?chat=Fam%2B1%3D&limit=2" {
		t.Errorf("request %q", g.got[0])
	}
	c.out = &bytes.Buffer{}
	if err := c.run(t.Context(), []string{"signal", "messages", "Fam+1=", "--json"}); err != nil {
		t.Fatal(err)
	}
	if out, _ := output(c); out != rows+"\n" {
		t.Errorf("json %q", out)
	}
}

func TestRefusal(t *testing.T) {
	t.Parallel()
	c, _ := newGateway(t, map[string][]string{"GET /agent/v1/signal/chats": {
		`403 {"error":"no read access to signal now; ask the owner with: mandate request signal --reason WHY","request":"mandate request signal --reason WHY"}`,
	}})
	err := c.run(t.Context(), []string{"signal", "chats"})
	if ExitCode(err) != 1 || !strings.Contains(err.Error(), "mandate request signal --reason WHY") {
		t.Errorf("403: %v", err)
	}
}

func TestProposal(t *testing.T) {
	t.Parallel()
	c, g := newGateway(t, map[string][]string{
		"POST /agent/v1/signal/send": {`202 {"id":"P1","status":"pending"}`},
		"GET /agent/v1/proposals/P1": {
			`200 {"id":"P1","status":"pending"}`,
			`200 {"id":"P1","status":"done","result":{"ref":"m9"}}`,
		},
	})
	if err := c.run(t.Context(), []string{"signal", "send", "Fam+1=", "on", "my", "way", "--note", "the owner asked", "--wait", "1m"}); err != nil {
		t.Fatalf("send: %v (exit %d)", err, ExitCode(err))
	}
	if out, _ := output(c); out != "P1 pending\nP1 done ref=m9\n" {
		t.Errorf("output %q", out)
	}
	if want := `POST /agent/v1/signal/send {"args":{"chat":"Fam+1=","text":"on my way"},"note":"the owner asked"}`; g.got[0] != want {
		t.Errorf("proposal %q, want %q", g.got[0], want)
	}

	for status, exit := range map[string]int{"pending": exitTryLater, "done": 0, "rejected": 1, "unknown": 1} {
		c, _ := newGateway(t, map[string][]string{
			"GET /agent/v1/proposals/P1": {`200 {"id":"P1","status":"` + status + `"}`},
		})
		if err := c.run(t.Context(), []string{"status", "P1", "--wait", "0"}); ExitCode(err) != exit {
			t.Errorf("%s: exit %d (%v), want %d", status, ExitCode(err), err, exit)
		}
	}

	c, _ = newGateway(t, map[string][]string{"POST /agent/v1/signal/send": {`202 {"id":"P1","status":"pending"}`}})
	if err := c.run(t.Context(), []string{"signal", "send", "Fam+1=", "hi", "--wait", "0"}); ExitCode(err) != exitTryLater {
		t.Errorf("send --wait 0: exit %d (%v)", ExitCode(err), err)
	}
}

func TestRequest(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args []string
		body string
	}{
		"once": {
			[]string{"--chat", "Family", "--for", "1h", "--since", "2d"},
			`{"connector":"signal","chats":["Family"],"for":3600,"horizon":172800,"reason":"summary"}`,
		},
		"routine": {
			[]string{"--chat", "Family", "--every", "weekdays", "--at", "07:00", "--for", "20m", "--since", "all"},
			`{"connector":"signal","chats":["Family"],"horizon":0,"schedule":{"days":[1,2,3,4,5],"start":"07:00","length":1200},"reason":"summary"}`,
		},
		"extend": {[]string{"--extend"}, `{"connector":"signal","extend":true,"reason":"summary"}`},
	} {
		c, g := newGateway(t, map[string][]string{
			"POST /agent/v1/access/requests":   {`202 {"id":"R1"}`},
			"GET /agent/v1/access/requests/R1": {`200 {"status":"granted","until":"2026-11-03T08:00:00Z"}`},
		})
		args := append([]string{"request", "signal", "--reason", "summary", "--wait", "0"}, tc.args...)
		if err := c.run(t.Context(), args); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if want := "POST /agent/v1/access/requests " + tc.body; g.got[0] != want {
			t.Errorf("%s:\n%s\nwant\n%s", name, g.got[0], want)
		}
	}
}
