package discord

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

func TestBrowserClientBuilds(t *testing.T) {
	if _, _, err := newBrowserClient(); err != nil {
		t.Fatal(err)
	}
}

func TestChromeProfileSpeaksHTTP2(t *testing.T) {
	type seen struct {
		proto  string
		header http.Header
	}
	got := make(chan seen, 1)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{r.Proto, r.Header.Clone()}
		w.Write([]byte("{}"))
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithClientProfile(profiles.Chrome_152),
		tls_client.WithRandomTLSExtensionOrder(),
		tls_client.WithInsecureSkipVerify(),
	)
	if err != nil {
		t.Fatal(err)
	}
	st := &state{LaunchID: newUUID(), LaunchSignature: newLaunchSignature(), HeartbeatSessionID: newUUID()}
	req, err := fhttp.NewRequest(fhttp.MethodGet, srv.URL+"/api/v9/users/@me", nil)
	if err != nil {
		t.Fatal(err)
	}
	applyHeaders(req, apiHeaders(st, "tok", "UTC", "https://discord.com/channels/@me"))
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	s := <-got
	if s.proto != "HTTP/2.0" {
		t.Errorf("protocol %s, want HTTP/2.0", s.proto)
	}
	if s.header.Get("User-Agent") != userAgent || s.header.Get("Sec-Ch-Ua") != secChUA || s.header.Get("Authorization") != "tok" ||
		s.header.Get("Accept-Encoding") != "gzip, deflate, br, zstd" {
		t.Errorf("headers %v", s.header)
	}
}

func TestReadToken(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name, content string
		ok            bool
	}{
		{"plain", "abc.def.ghi\n", true},
		{"empty", "  \n", false},
		{"bot token", "Bot abc.def.ghi", false},
		{"two tokens", "abc\ndef", false},
	}
	for _, tt := range tests {
		p := filepath.Join(dir, tt.name)
		if err := os.WriteFile(p, []byte(tt.content), 0o600); err != nil {
			t.Fatal(err)
		}
		tok, err := readToken(p)
		if (err == nil) != tt.ok {
			t.Errorf("%s: err %v", tt.name, err)
		}
		if err != nil && strings.Contains(err.Error(), "abc") {
			t.Errorf("%s: error leaks the token: %v", tt.name, err)
		}
		if tt.ok && tok != "abc.def.ghi" {
			t.Errorf("%s: token %q", tt.name, tok)
		}
	}
}

func TestCommandUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"run"}, {"serve"}, {"serve", "--data", "x"}} {
		if err := Command(t.Context(), args); err == nil {
			t.Errorf("Command(%q) succeeded", args)
		}
	}
}
