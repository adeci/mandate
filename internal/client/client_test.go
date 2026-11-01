package client

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adeci/mandate/internal/exitcode"
)

type recorded struct {
	method, path, query, auth, ctype string
	body                             string
}

func gateway(t *testing.T, status int, reply string) (*httptest.Server, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		rec.method, rec.path, rec.query = r.Method, r.URL.EscapedPath(), r.URL.RawQuery
		rec.auth, rec.ctype, rec.body = r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(body)
		w.WriteHeader(status)
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func testClient(t *testing.T, url string, tty bool) (*client, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, errOut bytes.Buffer
	env := map[string]string{"MANDATE_URL": url + "/", "MANDATE_TOKEN": "secret"}
	c, err := fromEnv(func(k string) string { return env[k] }, &out, &errOut, tty)
	if err != nil {
		t.Fatal(err)
	}
	return c, &out, &errOut
}

const rowsReply = `{"columns":["seq","sender","body"],"rows":[[7,"Farid","see you\nat 5\t\\o/"],[8,"Mom",null]],"more":true,"next_before":7}` + "\n"

func TestOutputModes(t *testing.T) {
	t.Parallel()
	srv, rec := gateway(t, http.StatusOK, rowsReply)
	for name, tc := range map[string]struct {
		tty  bool
		args []string
		want string
	}{
		"piped is TSV":   {false, nil, "seq\tsender\tbody\n7\tFarid\tsee you\\nat 5\\t\\\\o/\n8\tMom\t\n"},
		"terminal table": {true, nil, "seq  sender  body\n7    Farid   see you\\nat 5\\t\\\\o/\n8    Mom     \n"},
		"json":           {false, []string{"--json"}, rowsReply},
	} {
		c, out, errOut := testClient(t, srv.URL, tc.tty)
		if err := c.connector(t.Context(), "whatsapp", append([]string{"messages", "123@g.us"}, tc.args...)); err != nil {
			t.Fatal(err)
		}
		if out.String() != tc.want {
			t.Errorf("%s:\n%q\nwant\n%q", name, out, tc.want)
		}
		if tc.args == nil && errOut.String() != "next: --before 7\n" {
			t.Errorf("%s: footer %q", name, errOut)
		}
	}
	if rec.auth != "Bearer secret" {
		t.Errorf("auth %q", rec.auth)
	}
}

func TestRequests(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args                      []string
		method, path, query, body string
	}{
		"chats": {
			args:   []string{"chats", "--search", "Farid Haddad", "--limit", "5"},
			method: "GET", path: "/v1/whatsapp/chats", query: "limit=5&search=Farid+Haddad",
		},
		"messages": {
			args:   []string{"messages", "123@g.us", "--before", "41"},
			method: "GET", path: "/v1/whatsapp/messages", query: "before=41&chat=123%40g.us",
		},
		"search joins words": {
			args:   []string{"search", "drill", "bits", "--since", "2026-10-01"},
			method: "GET", path: "/v1/whatsapp/search", query: "since=2026-10-01&text=drill+bits",
		},
		"send with trailing note": {
			args:   []string{"send", "123@g.us", "running late", "--note", "Alex asked"},
			method: "POST", path: "/v1/whatsapp/send",
			body: `{"args":{"chat":"123@g.us","text":"running late"},"note":"Alex asked"}`,
		},
		"send text after --": {
			args:   []string{"send", "123@g.us", "--", "-5 outside"},
			method: "POST", path: "/v1/whatsapp/send", body: `{"args":{"chat":"123@g.us","text":"-5 outside"}}`,
		},
		"send to a base64 chat ID": {
			args:   []string{"send", "Zm9vYmFy+/a2V5=", "hi"},
			method: "POST", path: "/v1/whatsapp/send", body: `{"args":{"chat":"Zm9vYmFy+/a2V5=","text":"hi"}}`,
		},
		"send with KEY=VALUE": {
			args:   []string{"send", "to=a@example.com", "subject=Lunch", "body=at 1?", "--note", "Alex asked"},
			method: "POST", path: "/v1/whatsapp/send",
			body: `{"args":{"body":"at 1?","subject":"Lunch","to":"a@example.com"},"note":"Alex asked"}`,
		},
		"other op": {
			args:   []string{"fetch", "channel_id=9", "content=a=b c"},
			method: "POST", path: "/v1/whatsapp/fetch", body: `{"args":{"channel_id":"9","content":"a=b c"}}`,
		},
		"get keeps path and query": {
			args:   []string{"get", "/github/repos/a/b%2Fc/issues?state=open"},
			method: "GET", path: "/v1/forward/whatsapp/github/repos/a/b%2Fc/issues", query: "state=open",
		},
		"get --head": {
			args: []string{"get", "github/user", "--head"}, method: "HEAD", path: "/v1/forward/whatsapp/github/user",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, rec := gateway(t, http.StatusOK, `{"columns":[],"rows":[]}`)
			c, _, _ := testClient(t, srv.URL, false)
			if err := c.connector(t.Context(), "whatsapp", tc.args); err != nil {
				t.Fatal(err)
			}
			got := [4]string{rec.method, rec.path, rec.query, rec.body}
			want := [4]string{tc.method, tc.path, tc.query, tc.body}
			if got != want {
				t.Errorf("sent %q\nwant %q", got, want)
			}
		})
	}
}

func TestSendPrintsProposal(t *testing.T) {
	t.Parallel()
	const reply = `{"id":"p1","connector":"whatsapp","op":"send","status":"pending","display":[{"label":"To","value":"Farid"}],"url":"https://console/p/p1"}`
	srv, _ := gateway(t, http.StatusAccepted, reply)
	c, out, errOut := testClient(t, srv.URL, false)
	if err := c.connector(t.Context(), "whatsapp", []string{"send", "1@s.whatsapp.net", "hi"}); err != nil {
		t.Fatalf("send: %v (exit %d)", err, exitcode.For(err))
	}
	if !strings.Contains(out.String(), "p1") || !strings.Contains(out.String(), "https://console/p/p1") ||
		!strings.Contains(errOut.String(), "mandate status p1") {
		t.Errorf("output:\n%s%s", out, errOut)
	}
}

func TestOutputEscapesControlCharacters(t *testing.T) {
	t.Parallel()
	// An OSC title change, a C1 CSI and DEL from a third party.
	srv, _ := gateway(t, http.StatusOK, `{"columns":["sender","body"],"rows":[["~Mom\u001b]0;pwned\u0007","a\u009b2Jb\u007f"]]}`)
	c, out, _ := testClient(t, srv.URL, false)
	if err := c.connector(t.Context(), "whatsapp", []string{"messages", "123@g.us"}); err != nil {
		t.Fatal(err)
	}
	if want := "sender\tbody\n~Mom\\x1b]0;pwned\\x07\ta\\u009b2Jb\\x7f\n"; out.String() != want {
		t.Errorf("TSV %q, want %q", out, want)
	}

	srv, _ = gateway(t, http.StatusAccepted, `{"id":"p1","status":"pending","display":[{"label":"To","value":"~Mom\u001b[2K (+1 555)"},{"label":"Text","value":"hi\nthere"}]}`)
	c, out, _ = testClient(t, srv.URL, false)
	if err := c.connector(t.Context(), "whatsapp", []string{"send", "1@s.whatsapp.net", "hi"}); err != nil {
		t.Fatal(err)
	}
	if want := "  To: ~Mom\\x1b[2K (+1 555)\n  Text: hi\n    there\n"; !strings.Contains(out.String(), want) {
		t.Errorf("proposal %q, want it to contain %q", out, want)
	}
}

// actions fakes the gateway's proposal endpoints: POST answers pending, and
// GET answers pending until decided after `after` GETs. A pending long-poll
// times out after 1 s whatever its wait.
type actions struct {
	mu      sync.Mutex
	decided string
	after   int
	gets    int
	waits   []string
}

func (a *actions) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(`{"id":"p1","status":"pending","url":"https://console/p/p1"}`))
			return
		}
		a.mu.Lock()
		a.gets++
		a.waits = append(a.waits, r.URL.Query().Get("wait"))
		status := "pending"
		if a.decided != "" && a.gets > a.after {
			status = a.decided
		}
		a.mu.Unlock()
		if status == "pending" {
			if n, _ := strconv.Atoi(r.URL.Query().Get("wait")); n > 0 {
				time.Sleep(time.Second)
			}
		}
		w.Write([]byte(`{"id":"p1","status":"` + status + `","url":"https://console/p/p1","result":{"status":"done","ref":"m9","detail":"boom"}}` + "\n"))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestStatusExitCodes(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]struct {
		exit int
		msg  string
	}{
		"pending":   {exitcode.TryLater, "mandate status p1"},
		"executing": {exitcode.TryLater, ""},
		"done":      {0, ""},
		"rejected":  {1, ""},
		"expired":   {1, ""},
		"failed":    {1, "boom"},
		"unknown":   {1, "boom"},
	} {
		a := &actions{decided: status}
		c, out, _ := testClient(t, a.serve(t), false)
		err := c.status(t.Context(), []string{"p1", "--json"})
		if exitcode.For(err) != want.exit || (err != nil && !strings.Contains(err.Error(), want.msg)) {
			t.Errorf("%s: exit %d (%v), want %d %q", status, exitcode.For(err), err, want.exit, want.msg)
		}
		if !strings.Contains(out.String(), `"status":"`+status+`"`) {
			t.Errorf("%s: output %q", status, out)
		}
	}
}

func TestWaitBlocksUntilDecided(t *testing.T) {
	t.Parallel()
	a := &actions{decided: "done", after: 1}
	c, out, _ := testClient(t, a.serve(t), false)
	// The first long-poll runs out at the fake's 1 s; the next sees the decision.
	if err := c.connector(t.Context(), "whatsapp", []string{"send", "1@s.whatsapp.net", "hi", "--wait", "1h", "--json"}); err != nil {
		t.Fatalf("send --wait: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 2 ||
		!strings.Contains(lines[0], `"status":"pending"`) || !strings.Contains(lines[1], `"status":"done"`) {
		t.Errorf("output: %s", out)
	}
	a.mu.Lock()
	if a.waits[0] != "900" {
		t.Errorf("first long-poll wait=%s, want the 900 s cap", a.waits[0])
	}
	a.mu.Unlock()

	a = &actions{decided: "rejected", after: 0}
	c, _, _ = testClient(t, a.serve(t), false)
	if err := c.status(t.Context(), []string{"p1", "--wait", "15m"}); exitcode.For(err) != 1 {
		t.Errorf("rejected while waiting: exit %d (%v)", exitcode.For(err), err)
	}
}

func TestWaitTimesOut(t *testing.T) {
	t.Parallel()
	a := &actions{}
	c, out, _ := testClient(t, a.serve(t), false)
	start := time.Now()
	err := c.connector(t.Context(), "whatsapp", []string{"send", "1@s.whatsapp.net", "hi", "--wait", "1500ms"})
	if exitcode.For(err) != exitcode.TryLater {
		t.Errorf("timeout: exit %d (%v)", exitcode.For(err), err)
	}
	if d := time.Since(start); d < 1500*time.Millisecond || d > 5*time.Second {
		t.Errorf("--wait 1500ms returned after %v", d)
	}
	if n := strings.Count(out.String(), "p1 pending"); n != 2 {
		t.Errorf("output: %s, want the proposal then its final state", out)
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	srv, _ := gateway(t, http.StatusTooManyRequests, `{"error":"daily budget used","retry_after_seconds":3600}`)
	c, _, _ := testClient(t, srv.URL, false)
	err := c.connector(t.Context(), "discord", []string{"fetch", "channel_id=456"})
	if exitcode.For(err) != exitcode.TryLater || !strings.Contains(err.Error(), "1h0m0s") {
		t.Errorf("429: %v (exit %d)", err, exitcode.For(err))
	}

	srv, _ = gateway(t, http.StatusNotFound, `{"error":"unknown chat \"Farid\": run mandate whatsapp chats --search Farid"}`)
	c, _, _ = testClient(t, srv.URL, false)
	err = c.connector(t.Context(), "whatsapp", []string{"send", "Farid", "hi"})
	if exitcode.For(err) != 1 || !strings.Contains(err.Error(), "chats --search Farid") {
		t.Errorf("404: %v", err)
	}

	srv, rec := gateway(t, http.StatusOK, `[{"name":"whatsapp","source":true,"ops":[]}]`)
	c, _, _ = testClient(t, srv.URL, false)
	for name, args := range map[string][]string{
		"arg without =":       {"fetch", "456"},
		"duplicate arg":       {"fetch", "a=1", "a=2"},
		"messages without id": {"messages"},
		"send without text":   {"send", "123@g.us"},
		"negative wait":       {"send", "123@g.us", "hi", "--wait", "-1s"},
		"unknown flag":        {"chats", "--limt", "5"},
	} {
		if err := c.connector(t.Context(), "whatsapp", args); exitcode.For(err) != exitcode.Usage {
			t.Errorf("%s: %v", name, err)
		}
	}
	if rec.method != "" {
		t.Errorf("usage errors sent a request: %s %s", rec.method, rec.path)
	}
	if err := c.connector(t.Context(), "whatsap", []string{"--help"}); exitcode.For(err) != exitcode.Usage || !strings.Contains(err.Error(), "whatsapp") {
		t.Errorf("help for unknown connector: %v", err)
	}
}
