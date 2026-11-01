package console

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/internal/proposal"
)

func TestVisible(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\nok": "plain\ttext\nok",
		"<b>&</b>":        "&lt;b&gt;&amp;&lt;/b&gt;",
		"pay\u200bpal\u202e\u2066\u00ad\ufeff\U000E0041": `pay<span class="cp">U+200B</span>pal<span class="cp">U+202E</span>` +
			`<span class="cp">U+2066</span><span class="cp">U+00AD</span><span class="cp">U+FEFF</span><span class="cp">U+E0041</span>`,
		"bell\a\r":               `bell<span class="cp">U+0007</span><span class="cp">U+000D</span>`,
		"line\u2028sep":          `line<span class="cp">U+2028</span>sep`,
		"hangul\u3164filler":     `hangul<span class="cp">U+3164</span>filler`,
		"émoji 👍 and ünïcode ok": "émoji 👍 and ünïcode ok",
	} {
		if got := string(visible(in)); got != want {
			t.Errorf("visible(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRequestPage(t *testing.T) {
	h := start(t)
	p := h.submit(t, "<script>alert(1)</script> pay\u200bpal \u202egnp.exe")
	status, body := h.page(t, "/r/"+p.ID)
	if status != http.StatusOK {
		t.Fatalf("request page = %d", status)
	}
	for _, want := range []string{
		"Farid", "untrusted", "she asked",
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		`pay<span class="cp">U+200B</span>pal`, `<span class="cp">U+202E</span>gnp.exe`,
		"invisible characters",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("request page lacks %q", want)
		}
	}
	before, rest, _ := strings.Cut(body, "<textarea")
	_, after, _ := strings.Cut(rest, "</textarea>")
	if strings.ContainsAny(before+after, "\u200b\u202e") {
		t.Error("an invisible character is rendered raw outside the edit box")
	}

	// The challenge preview marks them too.
	_, _, res := h.challenge(t, p.ID, nil)
	if !strings.Contains(res.Preview, `<span class="cp">U+200B</span>`) || strings.Contains(res.Preview, "<script>") {
		t.Fatalf("preview:\n%s", res.Preview)
	}

	// Output is escaped once decided.
	nonce, c, _ := h.challenge(t, p.ID, nil)
	h.approve(t, p.ID, nonce, h.key.Assert(c))
	_, body = h.page(t, "/r/"+p.ID)
	if !strings.Contains(body, "&lt;b&gt;sent&lt;/b&gt;") {
		t.Fatalf("decided page:\n%s", body)
	}
}

func TestInbox(t *testing.T) {
	h := startWith(t, func(cfg *Config, dir string) {
		sock := filepath.Join(dir, "other.sock")
		serveFake(t, sock, "other", emptyKeys(t))
		cfg.Connectors["other"] = sock
	})
	deny := func(p proposal.Proposal) {
		t.Helper()
		if status, _ := h.post(t, "/r/"+p.ID+"/deny", nil); status != http.StatusSeeOther {
			t.Fatalf("deny = %d", status)
		}
	}
	for i := range pageSize + 5 {
		deny(h.submit(t, fmt.Sprintf("routine %d", i)))
	}
	deny(h.submitTo(t, "other", "dinner on Sunday?"))
	older, newer := h.submit(t, "waiting a"), h.submit(t, "waiting b")

	_, body := h.page(t, "/")
	if i, j := strings.Index(body, newer.ID), strings.Index(body, older.ID); i < 0 || j < 0 || i > j {
		t.Fatal("waiting isn't newest first")
	}
	if !strings.Contains(body, `<span class="count">2</span>`) || !strings.Contains(body, "1–25 of 31") ||
		!strings.Contains(body, `href="/?page=2"`) || strings.Contains(body, "← newer") {
		t.Fatalf("first page:\n%s", body)
	}
	if _, body = h.page(t, "/?page=2"); !strings.Contains(body, "26–31 of 31") ||
		!strings.Contains(body, "← newer") || strings.Contains(body, "older →") {
		t.Fatalf("last page:\n%s", body)
	}

	today := time.Now().Format(time.DateOnly)
	tomorrow := time.Now().AddDate(0, 0, 1).Format(time.DateOnly)
	for query, want := range map[string]string{
		"q=SUNDAY":         "1–1 of 1",
		"connector=other":  "1–1 of 1",
		"status=rejected":  "1–25 of 31",
		"status=done":      "No matches.",
		"to=" + today:      "1–25 of 31",
		"from=" + tomorrow: "No matches.",
		// Labels like "Text" aren't searched, only what they label.
		"q=text": "No matches.",
	} {
		if _, body := h.page(t, "/?"+query); !strings.Contains(body, want) {
			t.Errorf("%s: want %q", query, want)
		}
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := start(t)
	p := h.submit(t, "hi")
	for _, path := range []string{"/", "/r/" + p.ID, "/r/nope", "/login", "/static/app.js", "/connections", "/keys"} {
		resp := h.do(t, "GET", path, h.session, nil)
		resp.Body.Close()
		got := resp.Header.Get("Content-Security-Policy")
		for _, want := range []string{"default-src 'self'", "frame-ancestors 'none'", "script-src 'self'", "style-src 'self'"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: CSP %q lacks %q", path, got, want)
			}
		}
		if strings.Contains(got, "unsafe") {
			t.Errorf("%s: CSP %q allows unsafe sources", path, got)
		}
		if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Cache-Control") != "no-store" {
			t.Errorf("%s: headers = %v", path, resp.Header)
		}
	}
	for _, path := range []string{"/", "/?q=x&page=2", "/r/" + p.ID, "/login", "/connections", "/keys"} {
		_, body := h.page(t, path)
		if strings.Count(body, "<script") != strings.Count(body, `<script src="/static/app.js"`) ||
			strings.Contains(body, "style=") || strings.Contains(body, "<style") {
			t.Errorf("%s: page has inline script or style", path)
		}
	}

	// A cross-site form post can't deny on the owner's behalf, even with the
	// cookie.
	req, _ := http.NewRequest(http.MethodPost, h.pages.URL+"/r/"+p.ID+"/deny", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: h.session})
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site deny = %d, want 403", resp.StatusCode)
	}
}

func TestSetupConfirm(t *testing.T) {
	h := start(t)
	if _, body := h.page(t, "/connections"); !strings.Contains(body, `data-answer="yes">They match</button>`) ||
		!strings.Contains(body, `data-answer="no">They don't match</button>`) {
		t.Fatalf("connections page lacks the confirm buttons:\n%s", body)
	}
	setup := func(args map[string]string) (int, setupView) {
		t.Helper()
		status, body := h.post(t, "/connections/fake/setup", setupRequest{Args: args})
		var v setupView
		if status == http.StatusOK {
			if err := json.Unmarshal(body, &v); err != nil {
				t.Fatal(err)
			}
		}
		return status, v
	}
	if status, v := setup(map[string]string{"account": "compare"}); status != http.StatusOK || v.Code != "🐶 🔑" || v.Confirm != "Same emoji?" {
		t.Fatalf("compare = %d %+v", status, v)
	}
	if status, v := setup(map[string]string{"confirm": "yes"}); status != http.StatusOK || v.Instructions != "answered yes" {
		t.Fatalf("answer = %d %+v", status, v)
	}
	for _, args := range []map[string]string{{"confirm": "maybe"}, {"confirm": "no", "account": "compare"}} {
		if status, _ := setup(args); status != http.StatusBadRequest {
			t.Fatalf("setup %v = %d, want 400", args, status)
		}
	}
}

func TestSetupFlow(t *testing.T) {
	h := start(t)
	if _, body := h.page(t, "/connections"); !strings.Contains(body, `name="account"`) || !strings.Contains(body, `name="token" type="password"`) {
		t.Fatalf("connections page lacks the setup inputs:\n%s", body)
	}

	setup := func(args map[string]string) (int, setupView) {
		t.Helper()
		status, body := h.post(t, "/connections/fake/setup", setupRequest{Args: args})
		var v setupView
		if status == http.StatusOK {
			if err := json.Unmarshal(body, &v); err != nil {
				t.Fatal(err)
			}
		}
		return status, v
	}
	// An empty input counts as not given.
	status, first := setup(map[string]string{"account": ""})
	if status != http.StatusOK || !strings.HasPrefix(first.QR, "<svg") || first.Code != "" || first.Done {
		t.Fatalf("setup = %d %+v", status, first)
	}
	if status, _ := setup(map[string]string{"phone": "+15550100"}); status != http.StatusBadRequest {
		t.Fatalf("setup with an undeclared input = %d, want 400", status)
	}
	status, code := setup(map[string]string{"account": "+15550100"})
	if status != http.StatusOK || code.Instructions != "type it on +15550100" {
		t.Fatalf("pairing = %d %+v", status, code)
	}
	if _, done := setup(nil); !done.Done {
		t.Fatalf("third poll = %+v, want done", done)
	}
	if status, _ := setup(nil); status != http.StatusConflict {
		t.Fatalf("setup when linked = %d, want the connector's 409", status)
	}
	if status, _ := h.post(t, "/connections/nope/setup", setupRequest{}); status != http.StatusNotFound {
		t.Fatalf("setup unknown = %d", status)
	}

	health := func() connector.HealthState {
		t.Helper()
		_, raw := h.call(t, "GET", "/connections/fake/health", h.session, nil)
		var hl connector.Health
		if err := json.Unmarshal(raw, &hl); err != nil {
			t.Fatal(err)
		}
		return hl.State
	}
	if status, _ := h.post(t, "/connections/fake/unlink", nil); status != http.StatusOK || health() != connector.NeedsSetup {
		t.Fatalf("unlink = %d, health %s", status, health())
	}
	if status, v := setup(nil); status != http.StatusOK || v.QR == "" {
		t.Fatalf("relink = %d %+v", status, v)
	}
	h.post(t, "/connections/fake/unlink", nil)
	const token = "tok-4711-secret"
	if _, v := setup(map[string]string{"token": token}); !v.Done || health() != connector.Healthy {
		t.Fatalf("link with token = %+v", v)
	}
	if _, body := h.page(t, "/connections"); strings.Contains(body, token) {
		t.Fatal("connections page shows the token")
	}
	if status, _ := h.post(t, "/connections/nope/unlink", nil); status != http.StatusNotFound {
		t.Fatalf("unlink unknown = %d", status)
	}
}

func TestOtherProtocolIsDown(t *testing.T) {
	var called atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /spec", func(w http.ResponseWriter, _ *http.Request) {
		connector.WriteJSON(w, http.StatusOK, connector.Spec{
			Protocol: 2, Name: "next", Ops: []connector.Op{{Name: "send", Kind: connector.Write}},
			Setup: []connector.Param{{Name: "account"}},
		})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		connector.WriteJSON(w, http.StatusOK, connector.Health{State: connector.NeedsSetup})
	})
	mux.HandleFunc("POST /", func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		connector.WriteJSON(w, http.StatusOK, map[string]any{})
	})
	h := startWith(t, func(cfg *Config, dir string) {
		sock := filepath.Join(dir, "next.sock")
		serveSocket(t, sock, mux)
		cfg.Connectors["next"] = sock
	})

	const want = "speaks protocol 2"
	status, raw := h.call(t, "GET", "/connections/next/health", h.session, nil)
	var hl connector.Health
	if err := json.Unmarshal(raw, &hl); err != nil || status != http.StatusOK || hl.State != connector.Broken || !strings.Contains(hl.Detail, want) {
		t.Fatalf("health = %d %s", status, raw)
	}
	_, body := h.page(t, "/connections")
	if !strings.Contains(body, want) || strings.Contains(body, "setup-next-") {
		t.Fatalf("connections page:\n%s", body)
	}
	status, raw = h.post(t, "/connections/next/setup", setupRequest{Args: map[string]string{"account": "x"}})
	if status != http.StatusBadGateway || !strings.Contains(string(raw), want) {
		t.Fatalf("setup = %d %s", status, raw)
	}
	if _, err := h.api.Submit(context.Background(), proposal.Request{Connector: "next", Op: "send"}); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("submit: %v", err)
	}
	if n := called.Load(); n != 0 {
		t.Fatalf("connector speaking another protocol was called %d times", n)
	}
}
