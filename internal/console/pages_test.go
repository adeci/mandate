package console

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestVisible(t *testing.T) {
	for in, want := range map[string]string{
		"plain\ttext\nok":        "plain\ttext\nok",
		"<b>&</b>":               "&lt;b&gt;&amp;&lt;/b&gt;",
		"pay\u200bpal":           `pay<span class="cp">U+200B</span>pal`,
		"abc\u202edcba":          `abc<span class="cp">U+202E</span>dcba`,
		"\u2066x\u2069":          `<span class="cp">U+2066</span>x<span class="cp">U+2069</span>`,
		"a\u00adb\ufeffc":        `a<span class="cp">U+00AD</span>b<span class="cp">U+FEFF</span>c`,
		"bell\a\r":               `bell<span class="cp">U+0007</span><span class="cp">U+000D</span>`,
		"line\u2028sep":          `line<span class="cp">U+2028</span>sep`,
		"hangul\u3164filler":     `hangul<span class="cp">U+3164</span>filler`,
		"tag\U000E0041":          `tag<span class="cp">U+E0041</span>`,
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
		"Farid", "untrusted", "she asked", `data-param="text"`,
		"&lt;script&gt;alert(1)&lt;/script&gt;",
		`pay<span class="cp">U+200B</span>pal`, `<span class="cp">U+202E</span>gnp.exe`,
		"invisible characters", "data-review",
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
	if !strings.Contains(body, "&lt;b&gt;sent&lt;/b&gt;") || !strings.Contains(body, "approved by phone") {
		t.Fatalf("decided page:\n%s", body)
	}
}

func TestListPages(t *testing.T) {
	h := start(t)
	first := h.submit(t, "first")
	second := h.submit(t, "second")
	h.post(t, "/r/"+first.ID+"/deny", nil)
	third := h.submit(t, "third")

	_, body := h.page(t, "/")
	if i, j := strings.Index(body, third.ID), strings.Index(body, second.ID); i < 0 || j < 0 || i > j {
		t.Fatal("pending list isn't newest first")
	}
	if strings.Contains(body, first.ID) || !strings.Contains(body, `<span class="count">2</span>`) {
		t.Fatalf("pending list:\n%s", body)
	}
	_, body = h.page(t, "/history")
	if !strings.Contains(body, first.ID) || !strings.Contains(body, "rejected") || strings.Contains(body, second.ID) {
		t.Fatalf("history:\n%s", body)
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := start(t)
	p := h.submit(t, "hi")
	for _, path := range []string{"/", "/r/" + p.ID, "/r/nope", "/login", "/enroll/nope", "/static/app.js", "/connections", "/passkeys"} {
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
	for _, path := range []string{"/", "/r/" + p.ID, "/login", "/connections", "/passkeys", "/history"} {
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

func TestSetupFlow(t *testing.T) {
	h := start(t)
	_, body := h.page(t, "/connections")
	if !strings.Contains(body, `data-connection="fake"`) || !strings.Contains(body, "not linked") || !strings.Contains(body, "data-link") {
		t.Fatalf("connections page:\n%s", body)
	}

	setup := func(req setupRequest) (int, setupView) {
		t.Helper()
		status, body := h.post(t, "/connections/fake/setup", req)
		var v setupView
		if status == http.StatusOK {
			if err := json.Unmarshal(body, &v); err != nil {
				t.Fatal(err)
			}
		}
		return status, v
	}
	status, first := setup(setupRequest{})
	if status != http.StatusOK || !strings.HasPrefix(first.QR, "<svg") || !strings.Contains(first.QR, `<path fill="#000" d="M`) ||
		first.Instructions != "scan it" || first.Done {
		t.Fatalf("setup = %d %+v", status, first)
	}
	// Polls get the rotated code.
	status, code := setup(setupRequest{Phone: "+15550100"})
	if status != http.StatusOK || code.Code != "K7QF-2M9X" || code.QR != "" || code.Instructions != "type it on +15550100" {
		t.Fatalf("pairing = %d %+v", status, code)
	}
	if _, done := setup(setupRequest{}); !done.Done {
		t.Fatalf("third poll = %+v, want done", done)
	}
	if status, _ := setup(setupRequest{}); status != http.StatusConflict {
		t.Fatalf("setup when linked = %d, want the connector's 409", status)
	}
	status, raw := h.call(t, "GET", "/connections/fake/health", h.session, nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `"state":"ok"`) {
		t.Fatalf("health = %d %s", status, raw)
	}
	if status, _ := h.post(t, "/connections/nope/setup", setupRequest{}); status != http.StatusNotFound {
		t.Fatalf("setup unknown = %d", status)
	}
	if status, _ := h.post(t, "/connections/fake/setup", map[string]string{"code": "x"}); status != http.StatusBadRequest {
		t.Fatalf("setup with unknown arg = %d, want 400", status)
	}
}

func TestQRSVG(t *testing.T) {
	svg, err := qrSVG("2@" + strings.Repeat("A", 200))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(svg, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 `) || !strings.HasSuffix(svg, `"/></svg>`) ||
		strings.ContainsAny(svg[strings.Index(svg, `d="`)+3:len(svg)-9], `<>"&`) {
		t.Fatalf("svg = %s", svg)
	}
}
