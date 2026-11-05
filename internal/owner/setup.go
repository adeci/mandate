package owner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
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

// pairingJS shows the Copy link button, which needs JavaScript (Open in
// mandate works without), and moves on to / once a device has paired: /
// redirects to /setup until then. Without JavaScript the page reloads
// instead, and a spent setup link redirects to /.
const pairingJS = `var c = document.getElementById("copy");
c.hidden = false;
c.onclick = function () {
  navigator.clipboard.writeText(document.getElementById("open").getAttribute("href")).then(function () {
    c.textContent = "Copied";
    setTimeout(function () { c.textContent = "Copy link"; }, 1500);
  });
};
setInterval(function () {
  fetch("/", { redirect: "manual", cache: "no-store" }).then(function (r) {
    if (r.ok) location.replace("/");
  }, function () {});
}, 1000);`

// setupCSP allows only the page's own style and pairingJS, and its polls.
var setupCSP = func() string {
	sum := sha256.Sum256([]byte(pairingJS))
	return "default-src 'none'; style-src 'unsafe-inline'; script-src 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) +
		"'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'"
}()

// setupTmpl is the setup page in the app's look: one monospace, black and
// white with one grey, 2px rules, light or dark as the system is.
var setupTmpl = template.Must(template.New("setup").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="color-scheme" content="light dark">
<title>{{if .Home}}mandate{{else}}Initial setup{{end}}</title>
{{if .Href}}<noscript><meta http-equiv="refresh" content="5"></noscript>
{{end}}
<style>
:root{--fg:#000;--bg:#fff;--grey:#666;--lh:1.5rem;color-scheme:light dark}
@media (prefers-color-scheme:dark){:root{--fg:#fff;--bg:#000;--grey:#aaa}}
*{box-sizing:border-box}
html{font:16px/var(--lh) "JetBrains Mono",ui-monospace,"SF Mono",Menlo,Consolas,"Liberation Mono",monospace;color:var(--fg);background:var(--bg)}
body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;padding:var(--lh) 2ch}
main{width:100%;max-width:46ch}
h1{text-align:center;font-size:1rem;font-weight:800;text-transform:uppercase;letter-spacing:.12em;margin:0 0 var(--lh);padding-bottom:calc(var(--lh)/2);border-bottom:2px solid var(--fg)}
p{margin:0 0 var(--lh)}
label{display:block;color:var(--grey);text-transform:uppercase;letter-spacing:.12em;font-size:.875rem}
input,button,.button{font:inherit;border:2px solid var(--fg);border-radius:0;padding:calc(var(--lh)/4) 1ch}
input{display:block;width:100%;color:var(--fg);background:var(--bg);margin-top:calc(var(--lh)/4)}
button,.button{color:var(--bg);background:var(--fg);font-weight:700;text-transform:uppercase;letter-spacing:.12em;cursor:pointer}
.button{display:inline-block;text-decoration:none}
button:hover,button:focus-visible,.button:hover,.button:focus-visible{color:var(--fg);background:var(--bg)}
.center{text-align:center}
:focus-visible{outline:2px solid var(--fg);outline-offset:2px}
svg{display:block;width:100%;height:auto;margin:0 0 var(--lh);border:2px solid var(--fg)}
.key{font-size:1.125rem;font-weight:700;text-align:center;letter-spacing:.04em;padding:calc(var(--lh)/2) 0;border:2px solid var(--fg)}
code{font:inherit;font-weight:700}
</style>
<main>
{{if .Home}}<h1>mandate</h1>
<p class="center">Set up.</p>
{{else}}<h1>Initial setup</h1>
{{if .Href}}{{.QR}}
{{if .Key}}<p class="key">Key {{.Key}}</p>
{{end}}<p class="center"><a class="button" id="open" href="{{.Href}}">Open in mandate</a> <button type="button" id="copy" hidden>Copy link</button></p>
<script>` + pairingJS + `</script>
{{else if .Waiting}}<p>mandate is waiting to be set up. On the server, run <code>sudo mandate setup</code> and open the link it prints.</p>
{{else}}{{if .Wrong}}<p><strong>That password is not right.</strong></p>{{end}}
<form method="post" action="/setup"><p><label>Setup password<input name="password" autocomplete="off" autocapitalize="characters" spellcheck="false" required autofocus></label></p>
<p><button>Continue</button></p></form>
{{end}}{{end}}</main>`))

type setupPage struct {
	// Home: a device is paired, so / only says so.
	Home bool
	// Waiting: no device is paired and no setup password waits.
	Waiting bool
	Wrong   bool
	// Href is the pairing link, behind the QR code and the button.
	Href template.URL
	QR   template.HTML
	// Key is the server key's fingerprint, in own-key TLS mode only.
	Key string
}

func renderSetup(w http.ResponseWriter, status int, p setupPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", setupCSP)
	w.WriteHeader(status)
	_ = setupTmpl.Execute(w, p)
}

// home answers /: setup until a device is paired, then a page saying so.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	paired, _ := s.keys.Head()
	s.mu.Unlock()
	if paired == 0 {
		http.Redirect(w, r, "/setup", http.StatusFound)
		return
	}
	renderSetup(w, http.StatusOK, setupPage{Home: true})
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	_, open := s.setupOpen()
	paired, _ := s.keys.Head()
	s.mu.Unlock()
	switch {
	case open:
		renderSetup(w, http.StatusOK, setupPage{})
	case paired == 0:
		renderSetup(w, http.StatusOK, setupPage{Waiting: true})
	default:
		http.NotFound(w, r)
	}
}

// setupPassword answers the password form, the fallback for the link
// mandate setup prints.
func (s *Server) setupPassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	password := r.PostFormValue("password")
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
	// The pairing page lives at the setup link, so that reloading it works.
	http.Redirect(w, r, "/setup/"+url.PathEscape(password), http.StatusSeeOther)
}

// setupLink opens the pairing page straight from the link mandate setup
// prints. Once a device is paired it redirects to /; until then a wrong
// password gets the page /setup shows.
func (s *Server) setupLink(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	h, ok := s.setupOpen()
	if ok && subtle.ConstantTimeCompare([]byte(hashPassword(r.PathValue("password"))), []byte(h.SHA256)) == 1 {
		defer s.mu.Unlock()
		s.pairingPage(w, r)
		return
	}
	paired, _ := s.keys.Head()
	s.mu.Unlock()
	if paired > 0 {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	s.setupPage(w, r)
}

// pairingPage shows the pairing link as a QR code. The caller holds s.mu
// and has checked the setup password.
func (s *Server) pairingPage(w http.ResponseWriter, r *http.Request) {
	spki := r.Header.Get("Mandate-Spki-Sha256")
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
	p := setupPage{Href: template.URL(link), QR: template.HTML(svg)}
	if !s.certVerified {
		p.Key = fingerprint(spki)
	}
	renderSetup(w, http.StatusOK, p)
}

// fingerprint is the first 80 bits of a hex SPKI SHA-256 as five groups of
// four, short enough for people to compare.
func fingerprint(spki string) string {
	var b strings.Builder
	for i := 0; i < 20 && i+4 <= len(spki); i += 4 {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(spki[i : i+4])
	}
	return b.String()
}

// hostOnly is host without its port.
func hostOnly(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
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
	s.note(activity{Kind: "device.joined", Detail: key.Name})
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

// unstored marks the setup pages' answers, whose links may hold the setup
// password, as never to be kept or passed on as a referrer.
func unstored(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h(w, r)
	}
}

func printSetup(ctx context.Context, cfg *config.Config, out io.Writer, password string) {
	// The name the certificate is for, so that the browser accepts it.
	host := cfg.TLS.Domain
	if cfg.TLS.Mode == "cert" {
		host, _ = certHost(cfg.TLS.Cert)
	}
	if host == "" {
		host, _ = os.Hostname()
	}
	key := ""
	if cfg.TLS.Mode == "own" {
		key = serverKey(ctx, cfg)
	}
	writeSetup(out, setupURL(cfg, host, password), key)
}

// certHost is the first DNS name of the certificate in the PEM file at
// path, or its common name if it has none.
func certHost(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for {
		var block *pem.Block
		if block, b = pem.Decode(b); block == nil {
			return "", fmt.Errorf("%s: no certificate", path)
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		if len(c.DNSNames) > 0 {
			return c.DNSNames[0], nil
		}
		if c.Subject.CommonName == "" {
			return "", fmt.Errorf("%s: no DNS name", path)
		}
		return c.Subject.CommonName, nil
	}
}

// setupURL is the link that opens the pairing page with password.
func setupURL(cfg *config.Config, host, password string) string {
	addr := host
	if _, port, _ := net.SplitHostPort(cfg.Listen); port != "443" {
		addr = net.JoinHostPort(host, port)
	}
	return "https://" + addr + "/setup/" + password
}

// writeSetup prints the setup link and, in own-key mode, the key people
// compare with the app's.
func writeSetup(out io.Writer, link, key string) {
	fmt.Fprintf(out, "Initial setup  %s\n", link)
	if key != "" {
		fmt.Fprintf(out, "Key            %s\n", key)
	}
}

// serverKey is the fingerprint people compare in own-key mode.
func serverKey(ctx context.Context, cfg *config.Config) string {
	// Front may have just been started.
	var spki string
	var err error
	for range 10 {
		if spki, err = front.SPKI(ctx, cfg); err == nil {
			return fingerprint(spki)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Sprintf("unknown (%v); the setup page shows it", err)
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
	// systemctl waits only for the jobs it was asked for, and stopping a
	// target finishes before the units that are PartOf it, so name them too.
	if _, err := exec.LookPath("systemctl"); err == nil &&
		exec.CommandContext(ctx, "systemctl", "stop", "mandate.target", "mandate-*.service").Run() == nil {
		return exec.CommandContext(ctx, "systemctl", "start", "mandate.target").Run, nil
	}
	socks := []string{config.Socket("owner"), config.Socket("gateway")}
	for name := range cfg.Connectors {
		socks = append(socks, config.ConnectorSocket(name))
	}
	for _, sock := range socks {
		d := net.Dialer{Timeout: time.Second}
		if c, err := d.DialContext(ctx, "unix", sock); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s answers: stop mandate's services first", sock)
		}
	}
	return nil, nil
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
