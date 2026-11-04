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

// joinServer plays the owner side of an exchange the owner opened with
// code "123456": three wrong codes end it, and the right one is approved at
// once.
type joinServer struct {
	spki func(sum []byte) []byte // what front reports as the TLS key

	mu       sync.Mutex
	attempt  *exchange.Server
	wrong    int
	approved bool
	payload  exchange.Payload
}

var joinID = []byte("0123456789abcdef")

func (j *joinServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("POST /agent/v1/join", func(w http.ResponseWriter, _ *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		if j.wrong == 3 {
			http.Error(w, `{"error":"nothing is waiting for a login"}`, http.StatusNotFound)
			return
		}
		a, err := exchange.Start(exchange.Agent, joinID, "123456")
		if err != nil {
			t.Error(err)
		}
		j.attempt = a
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]string{"id": hex.EncodeToString(joinID), "msg": hex.EncodeToString(a.Msg())})
	})
	mux.HandleFunc("GET /agent/v1/join/{id}", func(w http.ResponseWriter, _ *http.Request) {
		j.mu.Lock()
		defer j.mu.Unlock()
		status := "ended"
		if j.approved {
			status = "approved"
		}
		json.NewEncoder(w).Encode(map[string]string{"status": status})
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
			j.wrong++
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"error": "wrong code", "attempts_left": 3 - j.wrong})
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

func TestLogin(t *testing.T) {
	t.Parallel()
	same := func(b []byte) []byte { return b }
	for name, tc := range map[string]struct {
		typed  string // stdin
		spki   func([]byte) []byte
		rooted bool
		says   string // on stderr
		err    string
	}{
		"pinned":           {typed: "123456\n", spki: same, says: "Code confirmed"},
		"rooted":           {typed: "123 456\n", spki: same, rooted: true, says: "Code confirmed"},
		"wrong then right": {typed: "111111\n12345\n123456", spki: same, says: "Wrong code; attempts left: 2.\nCode from the mandate app: The code is 6 digits."},
		"wrong three times": {
			typed: "111111\n222222\n333333\n123456\n", spki: same, says: "attempts left: 1.", err: "wrong code three times",
		},
		"no code":       {typed: "", spki: same, err: "no code given"},
		"other TLS key": {typed: "123456\n", spki: func([]byte) []byte { return make([]byte, 32) }, err: "intercepting"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			j := &joinServer{spki: tc.spki}
			srv := j.start(t)
			roots := x509.NewCertPool()
			if tc.rooted {
				roots.AddCert(srv.Certificate())
			}
			home := t.TempDir()
			env := map[string]string{"HOME": home}
			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			c := &cli{in: strings.NewReader(tc.typed), out: out, errOut: errOut, getenv: func(k string) string { return env[k] }, roots: roots}
			err := c.run(t.Context(), []string{"login", srv.URL, "--name", "laptop"})
			if !strings.Contains(errOut.String(), tc.says) {
				t.Errorf("stderr %q, want %q", errOut, tc.says)
			}
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
				t.Fatalf("login: %v\n%s", err, errOut)
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
			if err := c.run(t.Context(), []string{"connectors"}); err != nil || after.String() != "name\thealth\taccess\tcommands\n" {
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
