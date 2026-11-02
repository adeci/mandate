package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/internal/exitcode"
)

func TestRequestSendsAndMapsOutcome(t *testing.T) {
	t.Parallel()
	for status, exit := range map[string]int{"granted": 0, "pending": exitcode.TryLater, "denied": 1, "expired": 1} {
		var got accessRequest
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/access/requests", func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			w.Write([]byte(`{"id":"r1","url":"https://console.example/access#r1"}`))
		})
		mux.HandleFunc("GET /v1/access/requests/r1", func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"status":"` + status + `","agent_id":"a1","until":"2026-11-02T15:00:00Z"}`))
		})
		srv := httptest.NewServer(mux)
		c, out, _ := testClient(t, srv.URL, false)
		err := c.request(t.Context(), []string{"signal", "--chat", "Family", "--chat", "Work", "--for", "1h", "--since", "2d", "--reason", "summarize plans"})
		srv.Close()
		if exitcode.For(err) != exit || !strings.Contains(out.String(), "https://console.example/access#r1") {
			t.Errorf("%s: exit %d (%v), out %q", status, exitcode.For(err), err, out)
		}
		if got.Connector != "signal" || len(got.Chats) != 2 || got.For != time.Hour || got.Horizon == nil || *got.Horizon != 48*time.Hour || got.Reason != "summarize plans" {
			t.Errorf("sent %+v", got)
		}
	}

	c, _, _ := testClient(t, "http://127.0.0.1:1", false)
	if err := c.request(t.Context(), []string{"signal"}); exitcode.For(err) != exitcode.Usage || !strings.Contains(err.Error(), "--reason") {
		t.Errorf("without --reason: %v", err)
	}
}

func TestNoAccessPrintsRequestCommand(t *testing.T) {
	t.Parallel()
	srv, _ := gateway(t, http.StatusForbidden,
		`{"error":"no read access to signal now; if the task needs it, ask the owner with: mandate request signal --reason WHY","request":"mandate request signal --reason WHY"}`)
	c, _, _ := testClient(t, srv.URL, false)
	err := c.connector(t.Context(), "signal", []string{"chats"})
	if exitcode.For(err) != 1 || !strings.Contains(err.Error(), "mandate request signal --reason WHY") {
		t.Errorf("403: %v", err)
	}
}

func TestAccessPrintsGrants(t *testing.T) {
	t.Parallel()
	srv, _ := gateway(t, http.StatusOK, `[{"id":"g1","connector":"signal","chats":[{"id":"c1","name":"Family"}],
"from":"2026-11-02T14:00:00Z","until":"2026-11-02T15:00:00Z","horizon":604800000000000,"now":{"open":true,"until":"2026-11-02T15:00:00Z"}},
{"id":"g2","connector":"mail","schedule":{"days":[1,5],"start":"07:00","length":5400000000000,"zone":"Europe/Berlin"},"horizon":0,"now":{"open":false,"next":"2026-11-06T06:00:00Z"}}]`)
	c, out, _ := testClient(t, srv.URL, false)
	if err := c.access(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	want := "connector\tnow\tchats\twhen\tsince\tid\tnote\n" +
		"signal\topen until 2026-11-02 15:00\tFamily (c1)\t2026-11-02 14:00 to 2026-11-02 15:00\tlast 7d\tg1\t\n" +
		"mail\tclosed, opens 2026-11-06 06:00\tall\tevery Mon,Fri at 07:00 for 1h30m (Europe/Berlin)\tall history\tg2\t\n"
	if out.String() != want {
		t.Errorf("access:\n%q\nwant\n%q", out, want)
	}
}

func TestRequestRoutineAndExtend(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args []string
		want func(accessRequest) bool
	}{
		"routine": {[]string{"signal", "--chat", "Family", "--every", "weekdays", "--at", "07:00", "--for", "20m"}, func(r accessRequest) bool {
			s := r.Schedule
			return s != nil && len(s.Days) == 5 && s.Days[0] == time.Monday && s.Start == "07:00" && s.Length == 20*time.Minute && r.For == 0
		}},
		"routine on chosen days": {[]string{"signal", "--every", "wed,mon", "--at", "18:30"}, func(r accessRequest) bool {
			s := r.Schedule
			return s != nil && slices.Equal(s.Days, []time.Weekday{time.Monday, time.Wednesday}) && s.Length == 30*time.Minute
		}},
		"extend": {[]string{"signal", "--extend", "--for", "1h"}, func(r accessRequest) bool {
			return r.Extend && r.For == time.Hour && r.Schedule == nil && len(r.Chats) == 0
		}},
	} {
		var got accessRequest
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/access/requests", func(w http.ResponseWriter, r *http.Request) {
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Error(err)
			}
			w.Write([]byte(`{"id":"r1","url":"https://console.example/#r1"}`))
		})
		mux.HandleFunc("GET /v1/access/requests/r1", func(w http.ResponseWriter, _ *http.Request) {
			w.Write([]byte(`{"status":"granted","agent_id":"a1"}`))
		})
		srv := httptest.NewServer(mux)
		c, _, _ := testClient(t, srv.URL, false)
		err := c.request(t.Context(), append(tc.args, "--reason", "daily summary"))
		srv.Close()
		if err != nil || !tc.want(got) {
			t.Errorf("%s: %v, sent %+v (schedule %+v)", name, err, got, got.Schedule)
		}
	}

	c, _, _ := testClient(t, "http://127.0.0.1:1", false)
	for _, args := range [][]string{
		{"signal", "--every", "daily", "--reason", "x"},
		{"signal", "--at", "07:00", "--reason", "x"},
		{"signal", "--every", "someday", "--at", "07:00", "--reason", "x"},
		{"signal", "--extend", "--chat", "Family", "--reason", "x"},
	} {
		if err := c.request(t.Context(), args); exitcode.For(err) != exitcode.Usage {
			t.Errorf("%q: %v, want usage", args, err)
		}
	}
}
