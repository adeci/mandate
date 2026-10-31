package client

import (
	"bytes"
	"context"
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

func testClient(t *testing.T, url string, table bool) (*client, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	env := map[string]string{"MANDATE_URL": url + "/", "MANDATE_TOKEN": "secret"}
	c, err := fromEnv(func(k string) string { return env[k] }, &out, io.Discard, table)
	if err != nil {
		t.Fatal(err)
	}
	return c, &out
}

const rowsReply = `{"columns":["seq","sender_name","body"],"rows":[[7,"Farid","see you\nat 5"],[8,"Mom",null]],"truncated":false,"total_results":40}`

func TestRowsTableAndJSON(t *testing.T) {
	t.Parallel()
	srv, rec := gateway(t, http.StatusOK, rowsReply)

	c, out := testClient(t, srv.URL, true)
	if err := c.rows(t.Context(), "/v1/query", map[string]any{"sql": "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	if rec.auth != "Bearer secret" || rec.path != "/v1/query" {
		t.Errorf("request: path %q auth %q", rec.path, rec.auth)
	}
	want := "seq  sender_name  body\n7    Farid        see you at 5\n8    Mom          \n(2 of 40 results)\n"
	if out.String() != want {
		t.Errorf("table output:\n%q\nwant\n%q", out.String(), want)
	}

	c, out = testClient(t, srv.URL, false)
	if err := c.rows(t.Context(), "/v1/query", map[string]any{"sql": "SELECT 1"}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != rowsReply {
		t.Errorf("piped output is not the raw JSON: %s", out)
	}
}

func TestRequests(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cmd                       func(*client, context.Context, []string) error
		args                      []string
		method, path, query, body string
		ctype                     string
	}{
		"read": {
			cmd:    (*client).read,
			args:   []string{"discord", "search", "guild_id=123", "content=a=b c"},
			method: "POST", path: "/v1/read/discord/search",
			body: `{"args":{"content":"a=b c","guild_id":"123"}}`, ctype: "application/json",
		},
		"propose with trailing note": {
			cmd:    (*client).propose,
			args:   []string{"whatsapp", "send", "to=Farid", "text=running late", "--note", "Alex asked"},
			method: "POST", path: "/v1/actions",
			body:  `{"connector":"whatsapp","op":"send","args":{"text":"running late","to":"Farid"},"note":"Alex asked"}`,
			ctype: "application/json",
		},
		"status": {
			cmd:  (*client).status,
			args: []string{"p1"}, method: "GET", path: "/v1/actions/p1",
		},
		"get keeps path and query": {
			cmd:  (*client).forward,
			args: []string{"http", "/github/repos/a/b%2Fc/issues?state=open"}, method: "GET", path: "/v1/forward/http/github/repos/a/b%2Fc/issues", query: "state=open",
		},
		"get --head": {
			cmd:  (*client).forward,
			args: []string{"http", "github/user", "--head"}, method: "HEAD", path: "/v1/forward/http/github/user",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, rec := gateway(t, http.StatusOK, `{"id":"p1","status":"done"}`)
			c, _ := testClient(t, srv.URL, false)
			if err := tc.cmd(c, t.Context(), tc.args); err != nil {
				t.Fatal(err)
			}
			got := [5]string{rec.method, rec.path, rec.query, rec.body, rec.ctype}
			want := [5]string{tc.method, tc.path, tc.query, tc.body, tc.ctype}
			if got != want {
				t.Errorf("sent %q\nwant %q", got, want)
			}
		})
	}
}

func TestProposePrintsURL(t *testing.T) {
	t.Parallel()
	const reply = `{"id":"p1","connector":"whatsapp","op":"send","status":"pending","display":[{"label":"To","value":"Farid"}],"url":"https://console/p/p1"}`
	srv, _ := gateway(t, http.StatusAccepted, reply)
	c, out := testClient(t, srv.URL, true)
	if err := c.propose(t.Context(), []string{"whatsapp", "send", "to=Farid"}); err != nil {
		t.Fatalf("propose: %v (exit %d)", err, exitcode.For(err))
	}
	if !strings.Contains(out.String(), "https://console/p/p1") {
		t.Errorf("output:\n%s", out)
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
		w.Write([]byte(`{"id":"p1","status":"` + status + `","url":"https://console/p/p1","result":{"status":"done","ref":"m9","detail":"boom"}}`))
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
		"pending":   {exitcode.TryLater, "https://console/p/p1"},
		"executing": {exitcode.TryLater, ""},
		"done":      {0, ""},
		"rejected":  {1, ""},
		"expired":   {1, ""},
		"failed":    {1, "boom"},
		"unknown":   {1, "boom"},
	} {
		a := &actions{decided: status}
		c, out := testClient(t, a.serve(t), false)
		err := c.status(t.Context(), []string{"p1"})
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
	c, out := testClient(t, a.serve(t), false)
	// The first long-poll runs out at the fake's 1 s; the next sees the decision.
	if err := c.propose(t.Context(), []string{"whatsapp", "send", "to=Farid", "--wait", "1h"}); err != nil {
		t.Fatalf("propose --wait: %v", err)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 2 ||
		!strings.Contains(lines[0], `"url":"https://console/p/p1"`) || !strings.Contains(lines[1], `"status":"done"`) {
		t.Errorf("output: %s", out)
	}
	a.mu.Lock()
	if a.waits[0] != "900" {
		t.Errorf("first long-poll wait=%s, want the 900 s cap", a.waits[0])
	}
	a.mu.Unlock()

	a = &actions{decided: "rejected", after: 0}
	c, _ = testClient(t, a.serve(t), false)
	if err := c.status(t.Context(), []string{"p1", "--wait", "15m"}); exitcode.For(err) != 1 {
		t.Errorf("rejected while waiting: exit %d (%v)", exitcode.For(err), err)
	}
}

func TestWaitTimesOut(t *testing.T) {
	t.Parallel()
	a := &actions{}
	c, out := testClient(t, a.serve(t), false)
	start := time.Now()
	err := c.propose(t.Context(), []string{"whatsapp", "send", "to=Farid", "--wait", "1500ms"})
	if exitcode.For(err) != exitcode.TryLater {
		t.Errorf("timeout: exit %d (%v)", exitcode.For(err), err)
	}
	if d := time.Since(start); d < 1500*time.Millisecond || d > 5*time.Second {
		t.Errorf("--wait 1500ms returned after %v", d)
	}
	if n := strings.Count(out.String(), `"status":"pending"`); n != 2 {
		t.Errorf("output: %s, want the proposal then its final state", out)
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	srv, _ := gateway(t, http.StatusTooManyRequests, `{"error":"daily budget used","retry_after_seconds":3600}`)
	c, _ := testClient(t, srv.URL, false)
	err := c.read(t.Context(), []string{"discord", "fetch", "channel_id=456"})
	if exitcode.For(err) != exitcode.TryLater || !strings.Contains(err.Error(), "1h0m0s") {
		t.Errorf("429: %v (exit %d)", err, exitcode.For(err))
	}

	srv, _ = gateway(t, http.StatusNotFound, `{"error":"discord container 9 is unknown or hidden"}`)
	c, _ = testClient(t, srv.URL, false)
	err = c.read(t.Context(), []string{"discord", "fetch", "channel_id=9"})
	if exitcode.For(err) != 1 || !strings.Contains(err.Error(), "unknown or hidden") {
		t.Errorf("404: %v", err)
	}

	srv, rec := gateway(t, http.StatusOK, `{}`)
	c, _ = testClient(t, srv.URL, false)
	for name, err := range map[string]error{
		"arg without =": c.read(t.Context(), []string{"discord", "fetch", "456"}),
		"duplicate arg": c.propose(t.Context(), []string{"whatsapp", "send", "to=a", "to=b"}),
	} {
		if exitcode.For(err) != exitcode.Usage {
			t.Errorf("%s: %v", name, err)
		}
	}
	if rec.method != "" {
		t.Errorf("usage errors sent a request: %s %s", rec.method, rec.path)
	}
}

func TestGuideFromSchema(t *testing.T) {
	t.Parallel()
	srv, rec := gateway(t, http.StatusOK, `{
		"views":[{"name":"whatsapp_messages","description":"Messages. seq grows.","columns":["seq","container_name","sender_name","body"]},
			{"name":"whatsapp_containers","description":"Chats.","columns":["id","name"]}],
		"connectors":[
			{"name":"whatsapp","health":{"state":"setup","detail":"not linked"},"ops":[
				{"name":"send","kind":"write","summary":"Send a message","params":[
					{"name":"to","doc":"chat or contact","required":true,"container":true},
					{"name":"text","doc":"message text","required":true,"editable":true}]}]},
			{"name":"http","health":{"state":"ok"},"doc":"Reads allowed: GET /repos/adeci/*","forward":true,"ops":[
				{"name":"request","kind":"write","summary":"Call an API","params":[{"name":"service","doc":"one of: github","required":true}]}]}],
		"limits":{"max_rows":5000}}`)
	c, out := testClient(t, srv.URL, false)
	if err := c.guide(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if rec.path != "/v1/schema" {
		t.Errorf("guide fetched %s", rec.path)
	}
	for _, want := range []string{
		"whatsapp_messages",
		"Messages. seq grows.",
		"FROM whatsapp_containers",
		"not linked",
		"mandate propose whatsapp send to=... text=...",
		"owner may edit",
		"Reads allowed: GET /repos/adeci/*",
		"mandate get http PATH",
	} {
		if !strings.Contains(strings.ToLower(out.String()), strings.ToLower(want)) {
			t.Errorf("guide lacks %q", want)
		}
	}
	if t.Failed() {
		t.Log(out)
	}
}
