package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/adeci/mandate/internal/exitcode"
)

type recorded struct {
	path, auth string
	body       map[string]any
}

func gateway(t *testing.T, status int, reply string) (*httptest.Server, *recorded) {
	t.Helper()
	rec := &recorded{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.path, rec.auth = r.URL.Path, r.Header.Get("Authorization")
		if r.Method == http.MethodPost {
			if err := json.NewDecoder(r.Body).Decode(&rec.body); err != nil {
				t.Errorf("decode body: %v", err)
			}
		}
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

func TestDiscordRequests(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args []string
		path string
		body map[string]any
	}{
		"search with trailing flags": {
			[]string{"search", "123", "mounting brackets", "--channel", "456", "--has", "image"},
			"/v1/discord/search",
			map[string]any{"guild_id": "123", "content": "mounting brackets", "channel_id": "456", "has": "image"},
		},
		"fetch defaults to 50": {
			[]string{"fetch", "456", "--before", "789"},
			"/v1/discord/fetch",
			map[string]any{"channel_id": "456", "before": "789", "limit": float64(50)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv, rec := gateway(t, http.StatusOK, rowsReply)
			c, _ := testClient(t, srv.URL, false)
			if err := c.discord(t.Context(), tc.args[0], tc.args[1:], "usage"); err != nil {
				t.Fatal(err)
			}
			got, _ := json.Marshal(rec.body)
			want, _ := json.Marshal(tc.body)
			if rec.path != tc.path || string(got) != string(want) {
				t.Errorf("sent %s %s, want %s %s", rec.path, got, tc.path, want)
			}
		})
	}
}

func TestErrors(t *testing.T) {
	t.Parallel()
	srv, _ := gateway(t, http.StatusTooManyRequests, `{"error":"daily budget used","retry_after_seconds":3600}`)
	c, _ := testClient(t, srv.URL, false)
	err := c.discord(t.Context(), "fetch", []string{"456"}, "usage")
	if exitcode.For(err) != exitcode.TryLater || !strings.Contains(err.Error(), "retry in 1h0m0s") {
		t.Errorf("429: %v (exit %d)", err, exitcode.For(err))
	}

	srv, _ = gateway(t, http.StatusNotFound, `{"error":"discord container 9 is unknown or hidden"}`)
	c, _ = testClient(t, srv.URL, false)
	err = c.discord(t.Context(), "fetch", []string{"9"}, "usage")
	if exitcode.For(err) != 1 || !strings.Contains(err.Error(), "unknown or hidden (HTTP 404)") {
		t.Errorf("404: %v", err)
	}

	if err := c.discord(t.Context(), "fetch", nil, "usage"); err == nil || err.Error() != "usage" {
		t.Errorf("missing channel: %v", err)
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
