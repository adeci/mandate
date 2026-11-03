package cli

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/adeci/mandate/internal/exchange"
)

// joinServer plays the owner side of the code exchange: once the CLI shows
// its code, the owner types typed(code), and a wrong code ends the exchange.
type joinServer struct {
	typed func(code string) string
	spki  func(sum []byte) []byte // what front reports as the TLS key
	codes chan string

	mu       sync.Mutex
	id       []byte
	attempt  *exchange.Server
	approved bool
	ended    bool
	payload  exchange.Payload
}

func (j *joinServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("POST /agent/v1/join", func(w http.ResponseWriter, _ *http.Request) {
		j.id = []byte("0123456789abcdef")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": hex.EncodeToString(j.id)})
	})
	mux.HandleFunc("GET /agent/v1/join/{id}", func(w http.ResponseWriter, _ *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		switch {
		case j.approved:
			json.NewEncoder(w).Encode(map[string]string{"status": "approved"})
		case j.ended:
			json.NewEncoder(w).Encode(map[string]string{"status": "ended"})
		case j.attempt == nil:
			a, err := exchange.Start(exchange.Agent, j.id, j.typed(<-j.codes))
			if err != nil {
				t.Error(err)
			}
			j.attempt = a
			fallthrough
		default:
			json.NewEncoder(w).Encode(map[string]string{"msg": hex.EncodeToString(j.attempt.Msg())})
		}
	})
	mux.HandleFunc("POST /agent/v1/join/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Msg, Confirm, Sealed string }
		json.NewDecoder(r.Body).Decode(&body)
		msg, _ := hex.DecodeString(body.Msg)
		confirm, _ := hex.DecodeString(body.Confirm)
		sealed, _ := hex.DecodeString(body.Sealed)
		sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
		j.mu.Lock()
		defer j.mu.Unlock()
		p, server, err := j.attempt.Finish(msg, confirm, sealed, j.spki(sum[:]))
		if err != nil {
			j.ended = true
			http.Error(w, `{"error":"wrong code"}`, http.StatusForbidden)
			return
		}
		j.approved, j.payload = true, p
		json.NewEncoder(w).Encode(map[string]string{"confirm": hex.EncodeToString(server)})
	})
	mux.HandleFunc("GET /agent/v1/connectors", func(w http.ResponseWriter, r *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !j.approved || exchange.TokenSHA256(token) != j.payload.TokenSHA256 {
			http.Error(w, `{"error":"unknown or revoked token"}`, http.StatusUnauthorized)
			return
		}
		w.Write([]byte("[]"))
	})
	srv = httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// codeOut passes the code the CLI prints on to the server.
type codeOut struct {
	bytes.Buffer
	codes chan string
}

func (o *codeOut) Write(p []byte) (int, error) {
	if code, ok := strings.CutPrefix(string(p), "code: "); ok {
		o.codes <- strings.TrimSpace(code)
	}
	return o.Buffer.Write(p)
}

func TestLogin(t *testing.T) {
	t.Parallel()
	same := func(code string) string { return code }
	for name, tc := range map[string]struct {
		typed  func(string) string
		spki   func([]byte) []byte
		rooted bool
		err    string
	}{
		"pinned": {typed: same, spki: func(b []byte) []byte { return b }},
		"rooted": {typed: same, spki: func(b []byte) []byte { return b }, rooted: true},
		"wrong code": {
			typed: func(code string) string { return strings.Map(func(r rune) rune { return '0' + (r-'0'+1)%10 }, code) },
			spki:  func(b []byte) []byte { return b }, err: "ended",
		},
		"other TLS key": {typed: same, spki: func([]byte) []byte { return make([]byte, 32) }, err: "intercepting"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			j := &joinServer{typed: tc.typed, spki: tc.spki, codes: make(chan string, 1)}
			srv := j.start(t)
			roots := x509.NewCertPool()
			if tc.rooted {
				roots.AddCert(srv.Certificate())
			}
			home := t.TempDir()
			env := map[string]string{"HOME": home}
			out := &codeOut{codes: j.codes}
			c := &cli{out: out, errOut: &bytes.Buffer{}, getenv: func(k string) string { return env[k] }, roots: roots}
			err := c.run(t.Context(), []string{"login", srv.URL, "--name", "laptop"})
			path := filepath.Join(home, ".config", "mandate", "credentials.json")
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("login: %v, want %q", err, tc.err)
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("credentials written after a failed login: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("login: %v\n%s", err, out)
			}
			fi, err := os.Stat(path)
			if err != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("credentials: %v %v", fi, err)
			}
			cr, err := readCredentials(path)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(srv.Certificate().RawSubjectPublicKeyInfo)
			want := hex.EncodeToString(sum[:])
			if tc.rooted {
				want = ""
			}
			if cr.URL != srv.URL || cr.SPKISHA256 != want || exchange.TokenSHA256(cr.Token) != j.payload.TokenSHA256 {
				t.Errorf("credentials %+v, want pin %q", cr, want)
			}

			// The token works afterwards, over the pin or the roots.
			after := &bytes.Buffer{}
			c = &cli{out: after, errOut: &bytes.Buffer{}, getenv: func(k string) string { return env[k] }, roots: roots}
			if err := c.run(t.Context(), []string{"connectors", "--json"}); err != nil || after.String() != "[]\n" {
				t.Errorf("connectors after login: %v %q", err, after)
			}
		})
	}
}

func TestPinRefusesOtherKey(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("[]")) }))
	t.Cleanup(srv.Close)
	home := t.TempDir()
	path := filepath.Join(home, ".config", "mandate", "credentials.json")
	if err := writeCredentials(path, credentials{URL: srv.URL, Token: "t", SPKISHA256: strings.Repeat("00", 32)}); err != nil {
		t.Fatal(err)
	}
	c := &cli{out: &bytes.Buffer{}, errOut: &bytes.Buffer{}, getenv: func(k string) string { return map[string]string{"HOME": home}[k] }}
	if err := c.run(t.Context(), []string{"connectors"}); err == nil || !strings.Contains(err.Error(), "confirmed at login") {
		t.Errorf("connectors over another key: %v", err)
	}
}
