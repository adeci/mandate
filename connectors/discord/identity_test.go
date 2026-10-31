package main

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"os"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
)

func TestSecCHUA(t *testing.T) {
	// Values Chrome actually sent for these versions.
	tests := []struct {
		major int
		want  string
	}{
		{120, `"Not_A Brand";v="8", "Chromium";v="120", "Google Chrome";v="120"`},
		{124, `"Chromium";v="124", "Google Chrome";v="124", "Not-A.Brand";v="99"`},
		{131, `"Google Chrome";v="131", "Chromium";v="131", "Not_A Brand";v="24"`},
		{133, `"Not(A:Brand";v="99", "Google Chrome";v="133", "Chromium";v="133"`},
		{140, `"Chromium";v="140", "Not=A?Brand";v="24", "Google Chrome";v="140"`},
		{152, `"Chromium";v="152", "Not?A_Brand";v="24", "Google Chrome";v="152"`},
	}
	for _, tt := range tests {
		if got := secCHUA(tt.major); got != tt.want {
			t.Errorf("secCHUA(%d) = %s, want %s", tt.major, got, tt.want)
		}
	}
	if !strings.Contains(userAgent, "X11; Linux x86_64") || !strings.Contains(userAgent, "Chrome/152.0.0.0 ") {
		t.Errorf("user agent %q", userAgent)
	}
}

func TestSuperProperties(t *testing.T) {
	st := &state{
		LaunchID: "9a65c85b-401d-4cf0-9c17-9676a68a482c", LaunchSignature: "477bea01-90cb-422d-9a38-aaa66ed3e25e",
		HeartbeatSessionID: "2fc3cabd-3ca0-4716-b399-496780c302d1", BuildNumber: 655747,
	}
	raw, err := base64.StdEncoding.DecodeString(encodeSuperProperties(st))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"os":"Linux","browser":"Chrome","device":"","device_model_name":"","system_locale":"en-US",` +
		`"has_client_mods":false,"browser_user_agent":"` + userAgent + `","browser_version":"152.0.0.0",` +
		`"os_version":"","referrer":"","referring_domain":"","referrer_current":"","referring_domain_current":"",` +
		`"release_channel":"stable","client_build_number":655747,"client_event_source":null,` +
		`"client_launch_id":"9a65c85b-401d-4cf0-9c17-9676a68a482c","launch_signature":"477bea01-90cb-422d-9a38-aaa66ed3e25e",` +
		`"client_heartbeat_session_id":"2fc3cabd-3ca0-4716-b399-496780c302d1","client_app_state":"focused"}`
	if string(raw) != want {
		t.Errorf("super properties\n got %s\nwant %s", raw, want)
	}
	var fresh state
	raw, _ = base64.StdEncoding.DecodeString(encodeSuperProperties(&fresh))
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m["client_build_number"] != float64(defaultBuildNumber) {
		t.Errorf("default build number not used: %s", raw)
	}
}

var wantAPIOrder = []string{
	"sec-ch-ua-platform", "authorization", "x-debug-options", "sec-ch-ua", "sec-ch-ua-mobile",
	"x-discord-timezone", "x-super-properties", "x-discord-locale", "user-agent", "accept",
	"sec-fetch-site", "sec-fetch-mode", "sec-fetch-dest", "referer", "accept-encoding",
	"accept-language", "cookie", "priority",
}

func TestAPIHeaders(t *testing.T) {
	st := &state{LaunchID: newUUID(), LaunchSignature: newLaunchSignature(), HeartbeatSessionID: newUUID()}
	req, err := fhttp.NewRequest(fhttp.MethodGet, "https://discord.com/api/v9/users/@me", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyHeaders(req, apiHeaders(st, "tok", "America/New_York", "https://discord.com/channels/@me"))
	if got := req.Header[fhttp.HeaderOrderKey]; strings.Join(got, ",") != strings.Join(wantAPIOrder, ",") {
		t.Errorf("order\n got %v\nwant %v", got, wantAPIOrder)
	}
	want := map[string]string{
		"Authorization":      "tok",
		"Accept":             "*/*",
		"Accept-Language":    "en-US,en;q=0.9",
		"Referer":            "https://discord.com/channels/@me",
		"Sec-Ch-Ua":          `"Chromium";v="152", "Not?A_Brand";v="24", "Google Chrome";v="152"`,
		"Sec-Ch-Ua-Mobile":   "?0",
		"Sec-Ch-Ua-Platform": `"Linux"`,
		"Sec-Fetch-Dest":     "empty",
		"Sec-Fetch-Mode":     "cors",
		"Sec-Fetch-Site":     "same-origin",
		"User-Agent":         userAgent,
		"X-Debug-Options":    "bugReporterEnabled",
		"X-Discord-Locale":   "en-US",
		"X-Discord-Timezone": "America/New_York",
		"X-Super-Properties": encodeSuperProperties(st),
	}
	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"Origin", "Content-Type", "X-Fingerprint"} {
		if req.Header.Get(k) != "" {
			t.Errorf("unexpected %s", k)
		}
	}
}

// HTTP/1.1 here; HTTP/2 uses the same order key.
func TestHeaderWireOrder(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	lines := make(chan []string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			lines <- nil
			return
		}
		defer c.Close()
		r := bufio.NewReader(c)
		var got []string
		for {
			l, err := r.ReadString('\n')
			l = strings.TrimRight(l, "\r\n")
			if err != nil || l == "" {
				break
			}
			got = append(got, l)
		}
		c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}"))
		lines <- got
	}()
	st := &state{LaunchID: newUUID(), LaunchSignature: newLaunchSignature(), HeartbeatSessionID: newUUID()}
	req, err := fhttp.NewRequest(fhttp.MethodGet, "http://"+ln.Addr().String()+"/api/v9/users/@me", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyHeaders(req, apiHeaders(st, "tok", "UTC", "https://discord.com/channels/@me"))
	resp, err := (&fhttp.Client{}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	got := <-lines
	if len(got) == 0 || got[0] != "GET /api/v9/users/@me HTTP/1.1" {
		t.Fatalf("request line %v", got)
	}
	var names []string
	for _, l := range got[1:] {
		name, _, _ := strings.Cut(l, ":")
		if n := strings.ToLower(name); n != "host" {
			names = append(names, n)
		}
	}
	var want []string
	for _, n := range wantAPIOrder {
		if n != "cookie" {
			want = append(want, n)
		}
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Errorf("wire order\n got %v\nwant %v", names, want)
	}
}

func TestPageHeaders(t *testing.T) {
	req, _ := fhttp.NewRequest(fhttp.MethodGet, "https://discord.com/channels/@me", nil)
	applyHeaders(req, documentHeaders())
	for k, v := range map[string]string{
		"Sec-Fetch-Dest": "document", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Site": "none",
		"Sec-Fetch-User": "?1", "Upgrade-Insecure-Requests": "1", "User-Agent": userAgent,
	} {
		if got := req.Header.Get(k); got != v {
			t.Errorf("document %s = %q, want %q", k, got, v)
		}
	}
	for _, k := range []string{"Authorization", "X-Super-Properties", "Referer"} {
		if req.Header.Get(k) != "" {
			t.Errorf("document request carries %s", k)
		}
	}
	req, _ = fhttp.NewRequest(fhttp.MethodGet, "https://discord.com/assets/web.0123456789abcdef.js", nil)
	applyHeaders(req, scriptHeaders("https://discord.com/channels/@me"))
	for k, v := range map[string]string{
		"Sec-Fetch-Dest": "script", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Site": "same-origin",
		"Accept": "*/*", "Referer": "https://discord.com/channels/@me",
	} {
		if got := req.Header.Get(k); got != v {
			t.Errorf("script %s = %q, want %q", k, got, v)
		}
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("script request carries the token")
	}
}

func TestLaunchSignature(t *testing.T) {
	for range 50 {
		s := newLaunchSignature()
		b, err := hex.DecodeString(strings.ReplaceAll(s, "-", ""))
		if err != nil || len(b) != 16 || len(s) != 36 {
			t.Fatalf("bad uuid %q", s)
		}
		if b[6]>>4 != 4 || b[8]>>6 != 2 {
			t.Errorf("%s is not a v4 UUID", s)
		}
		for _, bit := range launchSignatureBits {
			if b[(127-bit)/8]&(1<<(bit%8)) != 0 {
				t.Errorf("%s has client-mod bit %d set", s, bit)
			}
		}
	}
	// The documented example signature has every client-mod bit clear.
	b, _ := hex.DecodeString("477bea0190cb422d9a38aaa66ed3e25e")
	for _, bit := range launchSignatureBits {
		if b[(127-bit)/8]&(1<<(bit%8)) != 0 {
			t.Errorf("example has bit %d set: bit positions are wrong", bit)
		}
	}
}

func TestBuildNumberExtraction(t *testing.T) {
	html, err := os.ReadFile("testdata/app.html")
	if err != nil {
		t.Fatal(err)
	}
	js, err := os.ReadFile("testdata/web.js")
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := webJSPath(html); !ok || p != "/assets/web.b2b84438f3c83e84.js" {
		t.Errorf("webJSPath = %q, %v", p, ok)
	}
	if n, ok := extractBuildNumber(buildNumberHTMLRe, html); !ok || n != 655747 {
		t.Errorf("HTML build number = %d, %v", n, ok)
	}
	if n, ok := extractBuildNumber(buildNumberJSRe, js); !ok || n != 655747 {
		t.Errorf("JS build number = %d, %v", n, ok)
	}
	tests := []struct {
		name string
		in   string
		want int
	}{
		{"quoted camel case", `x={buildNumber:"123456"}`, 123456},
		{"snake case with space", `build_number: '7654321'`, 7654321},
		{"unquoted", `buildNumber:234567,`, 234567},
		{"ternary before the real one", `crashed_build_number:t?1:2,build_number:"345678"`, 345678},
		{"too short", `buildNumber:"12345"`, 0},
		{"absent", `nothing here`, 0},
	}
	for _, tt := range tests {
		n, ok := extractBuildNumber(buildNumberJSRe, []byte(tt.in))
		if ok != (tt.want != 0) || n != tt.want {
			t.Errorf("%s: got %d, %v; want %d", tt.name, n, ok, tt.want)
		}
	}
	for _, bad := range []string{
		`<script src="https://evil.example/assets/web.0123456789abcdef.js"></script>`,
		`<script src="/assets/web.../../x.js"></script>`,
		`<link href="/assets/web.0123456789abcdef.js" rel="preload">`,
	} {
		if p, ok := webJSPath([]byte(bad)); ok {
			t.Errorf("webJSPath(%q) = %q", bad, p)
		}
	}
	if !bytes.Contains(html, []byte(`<script defer src="/assets/web.`)) {
		t.Error("fixture lost its script tag")
	}
}
