package client

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	c, err := fromEnv(func(k string) string { return env[k] }, &out, table)
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
		"action": {
			cmd:  (*client).action,
			args: []string{"p1"}, method: "GET", path: "/v1/actions/p1",
		},
		"http with data": {
			cmd:    (*client).passthrough,
			args:   []string{"post", "github", "/repos/a/b/issues?x=1", "--data", `{"title":"t"}`},
			method: "POST", path: "/v1/http/github/repos/a/b/issues", query: "x=1",
			body: `{"title":"t"}`, ctype: "application/json",
		},
		"http get has no body": {
			cmd:  (*client).passthrough,
			args: []string{"GET", "github", "user"}, method: "GET", path: "/v1/http/github/user",
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

func TestProposePrintsIDAndStatus(t *testing.T) {
	t.Parallel()
	srv, _ := gateway(t, http.StatusAccepted,
		`{"id":"p1","connector":"whatsapp","op":"send","status":"pending","display":[{"label":"To","value":"Farid"}]}`)
	c, out := testClient(t, srv.URL, true)
	if err := c.propose(t.Context(), []string{"whatsapp", "send", "to=Farid"}); err != nil {
		t.Fatalf("propose: %v (exit %d)", err, exitcode.For(err))
	}
	if s := out.String(); !strings.HasPrefix(s, "p1  pending  whatsapp send\n  To: Farid\n") || !strings.Contains(s, "mandate action p1") {
		t.Errorf("output:\n%s", s)
	}
}

func TestActionExitCodes(t *testing.T) {
	t.Parallel()
	for status, want := range map[string]int{
		"pending":   exitcode.TryLater,
		"executing": exitcode.TryLater,
		"done":      0,
		"unknown":   0,
		"rejected":  0,
	} {
		srv, _ := gateway(t, http.StatusOK, `{"id":"p1","status":"`+status+`","result":{"status":"done","ref":"m9"}}`)
		c, out := testClient(t, srv.URL, true)
		err := c.action(t.Context(), []string{"p1"})
		if exitcode.For(err) != want {
			t.Errorf("%s: exit %d (%v), want %d", status, exitcode.For(err), err, want)
		}
		if want != 0 && !strings.Contains(err.Error(), "proposal p1 is "+status) {
			t.Errorf("%s: unclear message %q", status, err)
		}
		if !strings.Contains(out.String(), "p1  "+status) || !strings.Contains(out.String(), "ref=m9") {
			t.Errorf("%s: output %q", status, out)
		}
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	srv, _ := gateway(t, http.StatusTooManyRequests, `{"error":"daily budget used","retry_after_seconds":3600}`)
	c, _ := testClient(t, srv.URL, false)
	err := c.read(t.Context(), []string{"discord", "fetch", "channel_id=456"})
	if exitcode.For(err) != exitcode.TryLater || !strings.Contains(err.Error(), "retry in 1h0m0s") {
		t.Errorf("429: %v (exit %d)", err, exitcode.For(err))
	}

	srv, _ = gateway(t, http.StatusNotFound, `{"error":"discord container 9 is unknown or hidden"}`)
	c, _ = testClient(t, srv.URL, false)
	err = c.read(t.Context(), []string{"discord", "fetch", "channel_id=9"})
	if exitcode.For(err) != 1 || !strings.Contains(err.Error(), "unknown or hidden (HTTP 404)") {
		t.Errorf("404: %v", err)
	}

	srv, rec := gateway(t, http.StatusOK, `{}`)
	c, _ = testClient(t, srv.URL, false)
	for name, err := range map[string]error{
		"read without op":     c.read(t.Context(), []string{"discord"}),
		"arg without =":       c.read(t.Context(), []string{"discord", "fetch", "456"}),
		"duplicate arg":       c.propose(t.Context(), []string{"whatsapp", "send", "to=a", "to=b"}),
		"action without id":   c.action(t.Context(), nil),
		"http missing path":   c.passthrough(t.Context(), []string{"GET", "github"}),
		"http invalid --data": c.passthrough(t.Context(), []string{"POST", "github", "x", "--data", "{"}),
	} {
		if exitcode.For(err) != 1 {
			t.Errorf("%s: %v", name, err)
		}
	}
	if rec.method != "" {
		t.Errorf("usage errors sent a request: %s %s", rec.method, rec.path)
	}
}

func TestSchemaListsConnectorOps(t *testing.T) {
	t.Parallel()
	srv, _ := gateway(t, http.StatusOK, `{"views":[],"connectors":[{"name":"whatsapp","ops":[
		{"name":"send","kind":"write","summary":"Send a message","params":[
			{"name":"to","doc":"chat or contact","required":true,"container":true},
			{"name":"text","doc":"message text","required":true,"editable":true}]},
		{"name":"history","kind":"read","summary":"Fetch older messages","params":[
			{"name":"chat","doc":"chat ID","required":true,"container":true},
			{"name":"count","doc":"how many"}]}]}]}`)
	c, out := testClient(t, srv.URL, true)
	if err := c.schema(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"mandate propose whatsapp send to=... text=...\n    Send a message\n",
		"    to: chat or contact (container)\n",
		"    text: message text (owner may edit)\n",
		"mandate read whatsapp history chat=... [count=...]\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("schema output lacks %q:\n%s", want, out)
		}
	}
}

func TestFromEnv(t *testing.T) {
	t.Parallel()
	for name, env := range map[string]map[string]string{
		"no url":   {"MANDATE_TOKEN": "x"},
		"no token": {"MANDATE_URL": "http://x"},
	} {
		if _, err := fromEnv(func(k string) string { return env[k] }, nil, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	_, err := fromEnv(func(k string) string {
		return map[string]string{"MANDATE_URL": "http://x", "MANDATE_TOKEN_FILE": "/nonexistent"}[k]
	}, nil, false)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing token file: %v", err)
	}
}
