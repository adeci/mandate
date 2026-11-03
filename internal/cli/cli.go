// Package cli is the agent side of docs/spec/agent-api.md: mandate login and
// the commands agents run.
package cli

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Above the gateway's own timeouts for connector reads (120 s) and files
// (10 min), so its answer always wins. Long polls add their wait on top.
const (
	requestTimeout = 150 * time.Second
	mediaTimeout   = 11 * time.Minute
)

// Exit codes besides 0 and 1. TryLater is EX_TEMPFAIL from sysexits.h.
const (
	exitUsage    = 2
	exitTryLater = 75
)

var (
	errUsage    = errors.New("usage")
	errTryLater = errors.New("try again later")
)

// ExitCode is the exit status for an error Run returned.
func ExitCode(err error) int {
	var api *apiError
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errTryLater), errors.As(err, &api) && api.status == http.StatusTooManyRequests:
		return exitTryLater
	case errors.Is(err, errUsage):
		return exitUsage
	}
	return 1
}

// exitError carries a message for the agent and the exit status it maps to,
// without the code's own text.
type exitError struct {
	msg  string
	code error
}

func (e *exitError) Error() string { return e.msg }
func (e *exitError) Unwrap() error { return e.code }

func usage(text string, err error) error {
	text = "usage: " + text
	if err != nil {
		text = err.Error() + "\n" + text
	}
	return &exitError{msg: text, code: errUsage}
}

// apiError is a refusal from mandate.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

type cli struct {
	out, errOut io.Writer
	tty         bool
	getenv      func(string) string
	roots       *x509.CertPool // nil: the system's

	url, token string
	http       *http.Client

	full bool   // print bodies whole
	seq  int64  // the one message asked for, if any
	name string // the connector
}

// Run runs the agent command in args.
func Run(ctx context.Context, args []string) error {
	fi, err := os.Stdout.Stat()
	tty := err == nil && fi.Mode()&os.ModeCharDevice != 0
	c := &cli{out: os.Stdout, errOut: os.Stderr, tty: tty, getenv: os.Getenv}
	return c.run(ctx, args)
}

func (c *cli) run(ctx context.Context, args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return c.help()
	}
	cmd, args := args[0], args[1:]
	if cmd == "login" {
		return c.login(ctx, args)
	}
	if err := c.connect(); err != nil {
		return err
	}
	switch cmd {
	case "connectors":
		return c.listConnectors(ctx, args)
	case "access":
		return c.access(ctx, args)
	case "request":
		return c.request(ctx, args)
	case "status":
		return c.status(ctx, args)
	}
	return c.connector(ctx, cmd, args)
}

// connect sets up the client from the credentials and the environment,
// which overrides them.
func (c *cli) connect() error {
	path, err := c.credentialsPath()
	if err != nil {
		return err
	}
	cr, err := readCredentials(path)
	if err != nil {
		return err
	}
	c.url = strings.TrimRight(cmp.Or(c.getenv("MANDATE_URL"), cr.URL), "/")
	c.token = cmp.Or(c.getenv("MANDATE_TOKEN"), cr.Token)
	if c.url == "" || c.token == "" {
		return errors.New("not logged in; run mandate login https://HOST with the address the owner gives you")
	}
	conf := &tls.Config{RootCAs: c.roots}
	if cr.SPKISHA256 != "" && c.url == cr.URL {
		conf = pinned(cr.SPKISHA256)
	}
	c.http = client(conf)
	return nil
}

func client(conf *tls.Config) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = conf
	return &http.Client{Transport: t}
}

func isHelp(s string) bool { return s == "-h" || s == "--help" || s == "help" }

var errNegativeWait = errors.New("--wait must not be negative")

// parse takes flags anywhere among args and wants between lo and hi
// positional arguments (hi < 0: no limit).
func (c *cli) parse(fs *flag.FlagSet, help string, args []string, lo, hi int) ([]string, error) {
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return nil, usage(help, err)
	}
	if len(pos) < lo || hi >= 0 && len(pos) > hi {
		return nil, usage(help, nil)
	}
	return pos, nil
}

// parseInterspersed lets flags follow positional arguments, which the flag
// package alone does not. Everything after "--" is positional.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if n := len(args) - len(rest); n > 0 && args[n-1] == "--" {
			return append(positional, rest...), nil
		}
		if len(rest) == 0 {
			return positional, nil
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
}

// query builds a query string from key, value pairs, leaving out zero values.
func query(kv ...any) string {
	q := url.Values{}
	for i := 0; i < len(kv); i += 2 {
		switch v := kv[i+1].(type) {
		case string:
			if v != "" {
				q.Set(kv[i].(string), v)
			}
		case int:
			if v != 0 {
				q.Set(kv[i].(string), strconv.Itoa(v))
			}
		}
	}
	if len(q) == 0 {
		return ""
	}
	return "?" + q.Encode()
}

// waitQuery is the wait parameter of one long poll towards deadline,
// capped at the server's 900 seconds; longer waits take several.
func waitQuery(deadline time.Time) (string, time.Duration) {
	poll := min(time.Until(deadline), 900*time.Second)
	if poll <= 0 {
		return "", requestTimeout
	}
	secs := int((poll + time.Second - 1) / time.Second)
	return "?wait=" + strconv.Itoa(secs), time.Duration(secs)*time.Second + requestTimeout
}

// do sends a request and reads the answer; out, if given, takes it as JSON.
func (c *cli) do(ctx context.Context, timeout time.Duration, method, path string, body, out any) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, 0, err
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return nil, 0, fmt.Errorf("%s %s: %w", method, path, err)
		}
	}
	return raw, resp.StatusCode, nil
}

// send answers a successful response for the caller to read and close, and
// turns anything else into an error for the agent.
func (c *cli) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, rd)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w; check the address, and tell the owner if mandate stays unreachable", err)
	}
	if resp.StatusCode/100 == 2 {
		return resp, nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	var e struct {
		Error             string `json:"error"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if json.Unmarshal(data, &e) != nil || e.Error == "" {
		e.Error = strings.TrimSpace(string(data))
	}
	msg := cmp.Or(sanitize(e.Error), http.StatusText(resp.StatusCode))
	if e.RetryAfterSeconds > 0 {
		msg += fmt.Sprintf("; retry in %s", time.Duration(e.RetryAfterSeconds)*time.Second)
	}
	return nil, &apiError{status: resp.StatusCode, msg: msg}
}
