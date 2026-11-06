package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/adeci/mandate/internal/exchange"
)

const loginUsage = "mandate login https://HOST[:PORT] [--name NAME]"

type credentials struct {
	URL        string `json:"url"`
	Token      string `json:"token"`
	SPKISHA256 string `json:"spki_sha256,omitempty"`
}

func (c *cli) credentialsPath() (string, error) {
	dir := c.getenv("XDG_CONFIG_HOME")
	if dir == "" {
		home := c.getenv("HOME")
		if home == "" {
			return "", errors.New("neither XDG_CONFIG_HOME nor HOME is set, so there is no place for credentials")
		}
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "mandate", "credentials.json"), nil
}

// readCredentials answers none if there is no file.
func readCredentials(path string) (credentials, error) {
	var cr credentials
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cr, nil
	}
	if err != nil {
		return cr, err
	}
	if err := json.Unmarshal(b, &cr); err != nil {
		return cr, fmt.Errorf("%s: %w", path, err)
	}
	return cr, nil
}

func writeCredentials(path string, cr credentials) error {
	b, err := json.Marshal(cr)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(append(b, '\n'))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func spkiSHA256(cert *x509.Certificate) []byte {
	h := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return h[:]
}

// pinned trusts exactly the server key pinned at login.
func pinned(spki string) *tls.Config {
	return &tls.Config{
		// The pin replaces the chain check: the certificate is the
		// server's own, which no root signed.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || hex.EncodeToString(spkiSHA256(cs.PeerCertificates[0])) != spki {
				return errors.New("the server's TLS key is not the one confirmed at login; tell the owner")
			}
			return nil
		},
	}
}

// serverKey is the key the server shows during login. The code exchange,
// not the certificate, vouches for it, so every connection must show the
// same one.
type serverKey struct {
	mu     sync.Mutex
	leaf   *x509.Certificate
	rooted bool
}

func (k *serverKey) config(host string, roots *x509.CertPool) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // server_confirm checks the key instead
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("the server showed no certificate")
			}
			leaf := cs.PeerCertificates[0]
			k.mu.Lock()
			defer k.mu.Unlock()
			if k.leaf == nil {
				inter := x509.NewCertPool()
				for _, c := range cs.PeerCertificates[1:] {
					inter.AddCert(c)
				}
				_, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots, Intermediates: inter})
				k.leaf, k.rooted = leaf, err == nil
				return nil
			}
			if !bytes.Equal(leaf.RawSubjectPublicKeyInfo, k.leaf.RawSubjectPublicKeyInfo) {
				return errors.New("the server's TLS key changed during login; tell the owner")
			}
			return nil
		},
	}
}

func (k *serverKey) spki() []byte {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.leaf == nil {
		return nil
	}
	return spkiSHA256(k.leaf)
}

// login runs the code exchange as the new party and keeps the token.
func (c *cli) login(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	name := fs.String("name", "", "")
	pos, err := c.parse(fs, loginUsage, args, 1, 1)
	if err != nil {
		return err
	}
	u, err := url.Parse(pos[0])
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || (u.Path != "" && u.Path != "/") {
		return usage(loginUsage, fmt.Errorf("%q is not an https://HOST[:PORT] address", pos[0]))
	}
	if *name == "" {
		if *name, err = os.Hostname(); err != nil {
			return err
		}
	}
	path, err := c.credentialsPath()
	if err != nil {
		return err
	}
	key := &serverKey{}
	c.url, c.token = "https://"+u.Host, ""
	c.http = client(key.config(u.Hostname(), c.roots))

	token := exchange.NewToken()
	payload := exchange.Payload{TokenSHA256: exchange.TokenSHA256(token)}
	// A read blocks until a line comes, so it runs aside and Ctrl-C ends the
	// prompt at once.
	type read struct {
		line string
		err  error
	}
	in, reads := bufio.NewReader(c.in), make(chan read, 1)
	var id string
	for id == "" {
		fmt.Fprint(c.errOut, "Code from Agents → Add an agent in the mandate app: ")
		go func() {
			line, err := in.ReadString('\n')
			reads <- read{line, err}
		}()
		var r read
		select {
		case <-ctx.Done():
			fmt.Fprintln(c.errOut)
			return ctx.Err()
		case r = <-reads:
		}
		code := strings.Join(strings.Fields(r.line), "")
		switch {
		case code == "" && r.err != nil:
			return errors.New("no code given; the owner opens Agents → Add an agent in the mandate app, which shows it")
		case len(code) != 6 || strings.Trim(code, "0123456789") != "":
			fmt.Fprintln(c.errOut, "The code is 6 digits.")
		default:
			if id, err = c.tryCode(ctx, *name, code, payload, key); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(c.errOut, "Code confirmed; waiting for the owner to let this machine in.")
	for {
		var poll struct{ Status, Reason string }
		if _, _, err := c.do(ctx, time.Minute+requestTimeout, http.MethodGet, "/agent/v1/join/"+id+"?wait=60", nil, &poll); err != nil {
			return err
		}
		switch poll.Status {
		case "approved":
			cr := credentials{URL: c.url, Token: token}
			if !key.rooted {
				cr.SPKISHA256 = hex.EncodeToString(key.spki())
			}
			if err := writeCredentials(path, cr); err != nil {
				return err
			}
			fmt.Fprintf(c.out, "logged in to %s as %s\n", c.url, *name)
			return nil
		case "ended":
			if poll.Reason == "denied" {
				return errors.New("Not let in.") //nolint:staticcheck // shown as is
			}
			return errors.New("the login ended before the owner let this machine in; run mandate login again")
		}
	}
}

// tryCode runs one attempt at code. It answers the exchange ID once the code
// is confirmed, or "" after a wrong code with attempts left.
func (c *cli) tryCode(ctx context.Context, name, code string, p exchange.Payload, key *serverKey) (string, error) {
	var started struct{ ID, Msg string }
	if _, _, err := c.do(ctx, requestTimeout, http.MethodPost, "/agent/v1/join",
		map[string]string{"kind": exchange.Agent, "name": name}, &started); err != nil {
		return "", err
	}
	id, err1 := hex.DecodeString(started.ID)
	msg, err2 := hex.DecodeString(started.Msg)
	if err1 != nil || err2 != nil || len(id) != 16 {
		return "", fmt.Errorf("the server answered exchange %q with message %q", started.ID, started.Msg)
	}
	reply, err := exchange.Respond(exchange.Agent, id, code, msg, p)
	if err != nil {
		return "", err
	}
	var answer struct{ Confirm string }
	_, _, err = c.do(ctx, requestTimeout, http.MethodPost, "/agent/v1/join/"+started.ID, map[string]string{
		"msg":     hex.EncodeToString(reply.Msg),
		"confirm": hex.EncodeToString(reply.Confirm),
		"sealed":  hex.EncodeToString(reply.Sealed),
	}, &answer)
	if api, ok := errors.AsType[*apiError](err); ok && api.status == http.StatusForbidden {
		if api.attemptsLeft == 0 {
			return "", errors.New("wrong code three times, so the login ended; ask the owner to open Add an agent in the mandate app again")
		}
		fmt.Fprintf(c.errOut, "Wrong code; attempts left: %d.\n", api.attemptsLeft)
		return "", nil
	}
	if err != nil {
		return "", err
	}
	confirm, err := hex.DecodeString(answer.Confirm)
	if err != nil || reply.Check(confirm, key.spki()) != nil {
		return "", errors.New("the server's confirmation does not match the TLS key it showed, so something between here and mandate may be intercepting; nothing was saved, tell the owner")
	}
	return started.ID, nil
}
