package owner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"rsc.io/qr"

	"github.com/adeci/mandate/internal/config"
	"github.com/adeci/mandate/internal/front"
	"github.com/adeci/mandate/record"
)

// setup.json holds the SHA-256 of the setup password while one waits. Root
// writes it; the owner side deletes it when the first device pairs.
func setupFile(dir string) string { return filepath.Join(dir, "setup.json") }

type setupHash struct {
	SHA256 string `json:"sha256"`
}

// normalize lets the owner type the password in any case, with or without
// its dashes.
func normalize(password string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(password))
}

func hashPassword(password string) string {
	h := sha256.Sum256([]byte(normalize(password)))
	return hex.EncodeToString(h[:])
}

// setupOpen is whether the setup page exists: a password waits and no
// device is paired. The caller holds s.mu.
func (s *Server) setupOpen() (setupHash, bool) {
	if n, _ := s.keys.Head(); n > 0 {
		return setupHash{}, false
	}
	b, err := os.ReadFile(setupFile(s.dir))
	if err != nil {
		return setupHash{}, false
	}
	var h setupHash
	if json.Unmarshal(b, &h) != nil || h.SHA256 == "" {
		return setupHash{}, false
	}
	return h, true
}

var setupTmpl = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Pair with mandate</title>
<style>body{font:16px/1.5 system-ui,sans-serif;max-width:34em;margin:2em auto;padding:0 1em}svg{width:18em;max-width:100%}code{word-break:break-all}</style>
{{if .Link}}<h1>Pair your first device</h1>
<p>Scan this with the mandate app, or open the link on the device.</p>
{{.QR}}
<p><a href="{{.Href}}"><code>{{.Link}}</code></a></p>
<p>The app shows the server key before pairing. It must be:</p>
<p><code>{{.SPKI}}</code></p>
{{else}}<h1>Pair with mandate</h1>
{{if .Wrong}}<p><strong>That password is not right.</strong></p>{{end}}
<form method="post"><p><label>Setup password, from <code>sudo mandate setup</code><br>
<input name="password" autocomplete="off" autocapitalize="characters" required autofocus></label></p>
<p><button>Continue</button></p></form>
{{end}}`))

type setupPage struct {
	Wrong bool
	Link  string
	Href  template.URL
	QR    template.HTML
	SPKI  string
}

func renderSetup(w http.ResponseWriter, status int, p setupPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	w.WriteHeader(status)
	_ = setupTmpl.Execute(w, p)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	_, ok := s.setupOpen()
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	renderSetup(w, http.StatusOK, setupPage{})
}

func (s *Server) setupPassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	password := r.PostFormValue("password")
	spki := r.Header.Get("Mandate-Spki-Sha256")
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.setupOpen()
	if !ok {
		http.NotFound(w, r)
		return
	}
	if subtle.ConstantTimeCompare([]byte(hashPassword(password)), []byte(h.SHA256)) != 1 {
		renderSetup(w, http.StatusForbidden, setupPage{Wrong: true})
		return
	}
	if r.Host == "" || len(spki) != 64 {
		http.Error(w, "this page must be reached through mandate's HTTPS port", http.StatusBadRequest)
		return
	}
	if s.secret == nil {
		s.secret = make([]byte, 32)
		rand.Read(s.secret)
	}
	link := "mandate://pair?url=" + url.QueryEscape("https://"+r.Host) +
		"&spki=" + spki + "&secret=" + base64.RawURLEncoding.EncodeToString(s.secret)
	svg, err := qrSVG(link)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	renderSetup(w, http.StatusOK, setupPage{Link: link, Href: template.URL(link), QR: template.HTML(svg), SPKI: spki})
}

// qrSVG draws text as a QR code, black on white with a 4-module quiet zone
// so that cameras read it in either theme.
func qrSVG(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", fmt.Errorf("QR code: %w", err)
	}
	const quiet = 4
	n := code.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" role="img" aria-label="QR code" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := range code.Size {
		for x := range code.Size {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}

// pair makes the first device's key seq 0 of the key log, spending the
// secret and the setup password.
func (s *Server) pair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Secret      record.Bytes    `json:"secret"`
		TokenSHA256 string          `json:"token_sha256"`
		Entry       record.Envelope `json:"entry"`
	}
	if !decode(w, r, &body) {
		return
	}
	if !isSHA256(body.TokenSHA256) {
		fail(w, http.StatusBadRequest, "token_sha256 is a SHA-256 in lowercase hex")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, _ := s.keys.Head(); n > 0 {
		fail(w, http.StatusConflict, "a device is paired already; mandate reset starts over")
		return
	}
	if _, ok := s.setupOpen(); !ok || s.secret == nil || subtle.ConstantTimeCompare(body.Secret, s.secret) != 1 {
		fail(w, http.StatusForbidden, "the pairing secret is wrong or spent")
		return
	}
	if err := s.appendKey(body.Entry); err != nil {
		fail(w, http.StatusForbidden, err.Error())
		return
	}
	key := s.keys.Trusted()[0]
	s.st.Devices[key.ID] = body.TokenSHA256
	if err := s.commit("keys"); err != nil {
		s.keys, s.st.Keys = new(record.Log), nil
		delete(s.st.Devices, key.ID)
		failErr(w, err)
		return
	}
	s.secret = nil
	if err := os.Remove(setupFile(s.dir)); err != nil {
		s.log.Error("spend setup password", "err", err)
	}
	reply(w, http.StatusOK, map[string]string{"kind": "key", "id": key.ID})
}

func isSHA256(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func tokenSHA256(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Setup makes a setup password unless one is waiting, and prints the setup
// page's address, the password and the server key's fingerprint.
func Setup(ctx context.Context, cfg *config.Config, out io.Writer) error {
	dir := config.StateDir("owner")
	st, err := loadState(dir)
	if err != nil {
		return err
	}
	if len(st.Keys) > 0 {
		return errors.New("a device is paired already; mandate reset starts over")
	}
	password, err := setupPassword(dir)
	if err != nil {
		return err
	}
	printSetup(ctx, cfg, out, password)
	return nil
}

// setupPassword is the waiting setup password, or a new one.
func setupPassword(dir string) (string, error) {
	file := config.SetupPasswordFile()
	if b, err := os.ReadFile(file); err == nil {
		if _, err := os.Stat(setupFile(dir)); err == nil {
			return strings.TrimSpace(string(b)), nil
		}
	}
	// 16 base32 characters: 80 bits, beyond guessing at any rate.
	p := rand.Text()[:16]
	password := p[:4] + "-" + p[4:8] + "-" + p[8:12] + "-" + p[12:]
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return "", err
	}
	if err := writeFile(file, []byte(password+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write setup password: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.Marshal(setupHash{SHA256: hashPassword(password)})
	if err != nil {
		return "", err
	}
	if err := writeOwned(setupFile(dir), b, 0o600); err != nil {
		return "", fmt.Errorf("write setup password hash: %w", err)
	}
	return password, nil
}

// writeOwned writes a file as root for the process owning its directory.
func writeOwned(path string, b []byte, mode fs.FileMode) error {
	if err := writeFile(path, b, mode); err != nil {
		return err
	}
	fi, err := os.Stat(filepath.Dir(path))
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(path, int(st.Uid), int(st.Gid))
}

func printSetup(ctx context.Context, cfg *config.Config, out io.Writer, password string) {
	host := cfg.TLS.Domain
	if host == "" {
		host, _ = os.Hostname()
	}
	_, port, _ := net.SplitHostPort(cfg.Listen)
	fmt.Fprintf(out, "Open https://%s/setup\nSetup password: %s\n", net.JoinHostPort(host, port), password)
	// Front may have just been started.
	var spki string
	var err error
	for range 10 {
		if spki, err = front.SPKI(ctx, cfg); err == nil {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		fmt.Fprintf(out, "Server key: unknown (%v); the setup page shows it\n", err)
		return
	}
	fmt.Fprintf(out, "Server key SHA-256: %s\n", spki)
}

// Reset stops mandate, forgets the paired devices with the key logs,
// identities and grants, and makes a new setup password. Connectors keep
// their spent approvals, so nothing signed before runs again.
func Reset(ctx context.Context, cfg *config.Config, out io.Writer) error {
	restart, err := stopServices(ctx, cfg)
	if err != nil {
		return err
	}
	password, err := wipe()
	if restart != nil {
		if rerr := restart(); rerr != nil {
			err = errors.Join(err, fmt.Errorf("start mandate.target: %w", rerr))
		}
	}
	if err != nil {
		return err
	}
	printSetup(ctx, cfg, out, password)
	return nil
}

// stopServices stops mandate.target and returns how to start it again. With
// no systemd or no target it only makes sure nothing of mandate runs.
func stopServices(ctx context.Context, cfg *config.Config) (func() error, error) {
	var restart func() error
	if _, err := exec.LookPath("systemctl"); err == nil &&
		exec.CommandContext(ctx, "systemctl", "stop", "mandate.target").Run() == nil {
		restart = exec.CommandContext(ctx, "systemctl", "start", "mandate.target").Run
	}
	socks := []string{config.Socket("owner"), config.Socket("gateway")}
	for name := range cfg.Connectors {
		socks = append(socks, config.ConnectorSocket(name))
	}
	for _, sock := range socks {
		d := net.Dialer{Timeout: time.Second}
		if c, err := d.DialContext(ctx, "unix", sock); err == nil {
			c.Close()
			err := fmt.Errorf("%s answers: stop mandate's services first", sock)
			if restart != nil {
				err = errors.Join(err, restart())
			}
			return nil, err
		}
	}
	return restart, nil
}

// wipe empties every key log and the owner side's state but never-share
// and activity, and makes a new setup password.
func wipe() (string, error) {
	dir := config.StateDir("owner")
	old, err := loadState(dir)
	if err != nil {
		return "", err
	}
	st := newState()
	st.NeverShare = old.NeverShare
	b, err := json.Marshal(&st)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := writeOwned(stateFile(dir), b, 0o600); err != nil {
		return "", fmt.Errorf("reset owner state: %w", err)
	}
	logs, err := filepath.Glob(filepath.Join(config.ConnectorDir("*"), "private", "keylog.json"))
	if err != nil {
		return "", err
	}
	for _, f := range logs {
		if err := os.Remove(f); err != nil {
			return "", fmt.Errorf("reset connector key log: %w", err)
		}
	}
	// All the gateway keeps derives from the live set the owner side pushes.
	gw := config.StateDir("gateway")
	entries, err := os.ReadDir(gw)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(gw, e.Name())); err != nil {
			return "", fmt.Errorf("reset gateway state: %w", err)
		}
	}
	for _, f := range []string{setupFile(dir), config.SetupPasswordFile()} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
	}
	return setupPassword(dir)
}
