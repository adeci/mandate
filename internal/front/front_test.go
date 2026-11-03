package front

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adeci/mandate/internal/config"
)

// start runs front in own mode with gateway and owner behind their sockets
// and returns its address.
func start(t *testing.T, gateway, owner http.Handler) string {
	t.Helper()
	t.Setenv("MANDATE_ROOT", t.TempDir())
	for _, b := range []struct {
		sock string
		h    http.Handler
	}{{config.Socket("gateway"), gateway}, {config.Socket("owner"), owner}} {
		if err := os.MkdirAll(filepath.Dir(b.sock), 0o700); err != nil {
			t.Fatal(err)
		}
		ln, err := net.Listen("unix", b.sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: b.h}
		go srv.Serve(ln)
		t.Cleanup(func() { srv.Close() })
	}

	tc, err := tlsConfig(&config.Config{TLS: config.TLS{Mode: "own"}}, config.StateDir("front"))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- serve(ctx, ln, tc) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return ln.Addr().String()
}

func client(h2 bool) *http.Client {
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
		ForceAttemptHTTP2: h2,
	}}
}

// raw sends request as written, so no client cleans the path first.
func raw(t *testing.T, addr, request string) *http.Response {
	t.Helper()
	c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := c.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRouting(t *testing.T) {
	backend := func(name string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, "%s %s", name, r.URL.Path)
		})
	}
	addr := start(t, backend("gateway"), backend("owner"))

	for _, tt := range []struct {
		method, target string
		want           string // the backend's answer; "" if none may answer
	}{
		{"GET", "/agent/v1/connectors", "gateway /agent/v1/connectors"},
		{"POST", "/owner/v1/pair", "owner /owner/v1/pair"},
		{"GET", "/setup", "owner /setup"},
		{"GET", "/", ""},
		{"GET", "/setup/x", ""},
		{"GET", "/internal/live", ""},
		{"GET", "/agent/../internal/live", ""},
		{"GET", "/owner/v1/%2e%2e/%2e%2e/internal/live", ""},
		{"GET", "//internal/live", ""},
		{"CONNECT", "/agent/../internal/live", ""},
	} {
		resp := raw(t, addr, tt.method+" "+tt.target+" HTTP/1.1\r\nHost: box\r\nContent-Length: 0\r\n\r\n")
		body := make([]byte, 512)
		n, _ := resp.Body.Read(body)
		got := string(body[:n])
		resp.Body.Close()
		if tt.want != "" {
			if resp.StatusCode != http.StatusOK || got != tt.want {
				t.Errorf("%s %s = %d %q, want %q", tt.method, tt.target, resp.StatusCode, got, tt.want)
			}
			continue
		}
		if resp.StatusCode == http.StatusOK || strings.HasPrefix(got, "gateway") || strings.HasPrefix(got, "owner") {
			t.Errorf("%s %s reached a backend: %d %q", tt.method, tt.target, resp.StatusCode, got)
		}
	}
}

func TestHeaders(t *testing.T) {
	echo := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(r.Header)
	})
	addr := start(t, echo, echo)

	for _, h2 := range []bool{false, true} {
		req, err := http.NewRequest(http.MethodGet, "https://"+addr+"/agent/v1/connectors", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		req.Header.Set("Mandate-Spki-Sha256", strings.Repeat("0", 64))
		resp, err := client(h2).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var got http.Header
		err = json.NewDecoder(resp.Body).Decode(&got)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if h2 != (resp.ProtoMajor == 2) {
			t.Fatalf("want h2 %v, got %s", h2, resp.Proto)
		}

		served := spkiSHA256(resp.TLS.PeerCertificates[0])
		if v := got.Values("X-Forwarded-For"); len(v) != 1 || v[0] != "127.0.0.1" {
			t.Errorf("%s: X-Forwarded-For = %q, want [127.0.0.1]", resp.Proto, v)
		}
		if v := got.Values("Mandate-Spki-Sha256"); len(v) != 1 || v[0] != served {
			t.Errorf("%s: Mandate-Spki-Sha256 = %q, want [%s]", resp.Proto, v, served)
		}
	}
}

// The key front makes on its first start is the one setup reports and the
// one the next start serves.
func TestOwnKey(t *testing.T) {
	addr := start(t, http.NotFoundHandler(), http.NotFoundHandler())
	reported, err := SPKI(context.Background(), &config.Config{Listen: addr})
	if err != nil {
		t.Fatal(err)
	}
	next, err := ownCert(config.StateDir("front"))
	if err != nil {
		t.Fatal(err)
	}
	if got := spkiSHA256(next.Leaf); got != reported {
		t.Errorf("next start serves %s, setup reported %s", got, reported)
	}
}

func TestCertReloaded(t *testing.T) {
	file := filepath.Join(t.TempDir(), "cert.pem")
	if _, err := makeOwnCert(file); err != nil {
		t.Fatal(err)
	}
	tc, err := tlsConfig(&config.Config{TLS: config.TLS{Mode: "cert", Cert: file, Key: file}}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before, err := tc.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := makeOwnCert(file); err != nil {
		t.Fatal(err)
	}
	// A renewal comes later than the coarse file clock in a test.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(file, later, later); err != nil {
		t.Fatal(err)
	}
	after, err := tc.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if spkiSHA256(before.Leaf) == spkiSHA256(after.Leaf) {
		t.Error("the new certificate was not picked up")
	}
}

func TestEventStreamUnbuffered(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	owner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: now\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	addr := start(t, http.NotFoundHandler(), owner)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+addr+"/owner/v1/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client(false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil {
		t.Fatalf("no event before the backend finished: %v", err)
	}
	if line != "event: now\n" {
		t.Errorf("first line %q", line)
	}
}
