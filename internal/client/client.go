// Package client implements the commands agents run against a gateway.
package client

import (
	"bytes"
	"context"
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
	"text/tabwriter"
	"time"

	"github.com/adeci/mandate/internal/exitcode"
)

// Above the gateway's own 120 s connector read timeout, so its answer always
// wins. Long-polls add their wait on top.
const requestTimeout = 150 * time.Second

const (
	chatsUsage    = "mandate %s chats [--search TEXT] [--limit 20]"
	messagesUsage = "mandate %s messages CHAT_ID [--limit 20] [--before SEQ]"
	searchUsage   = "mandate %s search TEXT [--chat CHAT_ID] [--since YYYY-MM-DD] [--limit 50] [--before SEQ]"
	sendUsage     = "mandate %s send CHAT_ID TEXT [--note WHY] [--wait DUR]"
	opUsage       = "mandate %s %s [KEY=VALUE...] [--note WHY] [--wait DUR]"
	getUsage      = "mandate %s get PATH [--head]"
	statusUsage   = "mandate status ID [--wait DUR]"
)

type client struct {
	url    string
	token  string
	http   *http.Client
	out    io.Writer
	errOut io.Writer
	tty    bool
	json   bool
}

func fromEnv(getenv func(string) string, out, errOut io.Writer, tty bool) (*client, error) {
	url := strings.TrimRight(getenv("MANDATE_URL"), "/")
	if url == "" {
		return nil, errors.New("MANDATE_URL is not set (e.g. http://lachesis:8780); ask the owner for the gateway address")
	}
	token := getenv("MANDATE_TOKEN")
	if token == "" && getenv("MANDATE_TOKEN_FILE") != "" {
		b, err := os.ReadFile(getenv("MANDATE_TOKEN_FILE"))
		if err != nil {
			return nil, fmt.Errorf("read MANDATE_TOKEN_FILE: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return nil, errors.New("MANDATE_TOKEN or MANDATE_TOKEN_FILE is not set; ask the owner to configure this agent")
	}
	return &client{url: url, token: token, http: &http.Client{}, out: out, errOut: errOut, tty: tty}, nil
}

func newClient() (*client, error) {
	fi, err := os.Stdout.Stat()
	tty := err == nil && fi.Mode()&os.ModeCharDevice != 0
	return fromEnv(os.Getenv, os.Stdout, os.Stderr, tty)
}

// Connector runs mandate NAME ARGS.
func Connector(ctx context.Context, name string, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	return c.connector(ctx, name, args)
}

func Status(ctx context.Context, args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	return c.status(ctx, args)
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
	return &exitError{msg: text, code: exitcode.ErrUsage}
}

func (c *client) connector(ctx context.Context, name string, args []string) error {
	if len(args) == 0 || isHelp(args[0]) {
		return c.connectorHelp(ctx, name)
	}
	cmd, args := args[0], args[1:]
	base := "/v1/" + url.PathEscape(name) + "/"
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	switch cmd {
	case "chats":
		help := fmt.Sprintf(chatsUsage, name)
		search := fs.String("search", "", "")
		limit := fs.Int("limit", 0, "")
		if _, err := c.parse(fs, help, args, 0, 0); err != nil {
			return err
		}
		return c.rows(ctx, base+"chats", query("search", *search, "limit", *limit))
	case "messages":
		help := fmt.Sprintf(messagesUsage, name)
		limit := fs.Int("limit", 0, "")
		before := fs.Int("before", 0, "")
		pos, err := c.parse(fs, help, args, 1, 1)
		if err != nil {
			return err
		}
		return c.rows(ctx, base+"messages", query("chat", pos[0], "limit", *limit, "before", *before))
	case "search":
		help := fmt.Sprintf(searchUsage, name)
		chat := fs.String("chat", "", "")
		since := fs.String("since", "", "")
		limit := fs.Int("limit", 0, "")
		before := fs.Int("before", 0, "")
		pos, err := c.parse(fs, help, args, 1, -1)
		if err != nil {
			return err
		}
		return c.rows(ctx, base+"search", query("text", strings.Join(pos, " "), "chat", *chat, "since", *since, "limit", *limit, "before", *before))
	case "send":
		help := fmt.Sprintf(sendUsage, name)
		note := fs.String("note", "", "")
		wait := fs.Duration("wait", 0, "")
		pos, err := c.parse(fs, help, args, 2, -1)
		if err != nil {
			return err
		}
		if *wait < 0 {
			return usage(help, errNegativeWait)
		}
		return c.submit(ctx, base+"send", map[string]string{"chat": pos[0], "text": strings.Join(pos[1:], " ")}, *note, *wait)
	case "get":
		return c.forward(ctx, name, args)
	}
	help := fmt.Sprintf(opUsage, name, cmd)
	note := fs.String("note", "", "")
	wait := fs.Duration("wait", 0, "")
	pos, err := c.parse(fs, help, args, 0, -1)
	if err != nil {
		return err
	}
	if *wait < 0 {
		return usage(help, errNegativeWait)
	}
	kv, err := keyValues(pos)
	if err != nil {
		return usage(help, err)
	}
	return c.submit(ctx, base+url.PathEscape(cmd), kv, *note, *wait)
}

func isHelp(s string) bool { return s == "-h" || s == "--help" || s == "help" }

var errNegativeWait = errors.New("--wait must not be negative")

// parse takes flags anywhere among args, adds --json, and wants between lo
// and hi positional arguments (hi < 0: no limit).
func (c *client) parse(fs *flag.FlagSet, help string, args []string, lo, hi int) ([]string, error) {
	fs.BoolVar(&c.json, "json", false, "")
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

func keyValues(args []string) (map[string]string, error) {
	kv := make(map[string]string, len(args))
	for _, a := range args {
		k, v, ok := strings.Cut(a, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("want KEY=VALUE, got %q", a)
		}
		if _, dup := kv[k]; dup {
			return nil, fmt.Errorf("%s given twice", k)
		}
		kv[k] = v
	}
	return kv, nil
}

// forward is a raw read: PATH, with any query, goes to the connector as
// typed. The body is printed as is; --head prints the headers instead.
func (c *client) forward(ctx context.Context, name string, args []string) error {
	help := fmt.Sprintf(getUsage, name)
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	head := fs.Bool("head", false, "")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return usage(help, err)
	}
	if len(positional) != 1 {
		return usage(help, nil)
	}
	path := "/v1/forward/" + url.PathEscape(name) + "/" + strings.TrimPrefix(positional[0], "/")
	method := http.MethodGet
	if *head {
		method = http.MethodHead
	}
	_, header, data, err := c.do(ctx, requestTimeout, method, path, nil)
	if err != nil {
		return err
	}
	if *head {
		return header.Write(c.out)
	}
	_, err = c.out.Write(data)
	return err
}

type table struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	More         bool     `json:"more"`
	NextBefore   int64    `json:"next_before"`
	TotalResults int      `json:"total_results"`
	Containers   int      `json:"containers"`
}

func (c *client) rows(ctx context.Context, path, query string) error {
	_, _, raw, err := c.do(ctx, requestTimeout, http.MethodGet, path+query, nil)
	if err != nil {
		return err
	}
	return c.table(raw)
}

// table prints rows as TSV, or aligned on a terminal; the footer goes to
// stderr so stdout stays parseable.
func (c *client) table(raw []byte) error {
	if c.json {
		_, err := c.out.Write(append(bytes.TrimRight(raw, "\n"), '\n'))
		return err
	}
	var t table
	if err := json.Unmarshal(raw, &t); err != nil {
		return err
	}
	w := c.out
	var tw *tabwriter.Writer
	if c.tty {
		tw = tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
		w = tw
	}
	fmt.Fprintln(w, strings.Join(t.Columns, "\t"))
	for _, row := range t.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = cell(v)
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	if tw != nil {
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	switch {
	case t.NextBefore > 0:
		fmt.Fprintf(c.errOut, "next: --before %d\n", t.NextBefore)
	case t.More:
		fmt.Fprintln(c.errOut, "more: raise --limit or narrow the search")
	case t.TotalResults > len(t.Rows):
		fmt.Fprintf(c.errOut, "%d of %d results\n", len(t.Rows), t.TotalResults)
	}
	if t.Containers > 0 {
		fmt.Fprintf(c.errOut, "%d chats updated\n", t.Containers)
	}
	return nil
}

func cell(v any) string {
	switch v := v.(type) {
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		return strings.Join(strings.Fields(v), " ")
	default:
		return fmt.Sprint(v)
	}
}

func (c *client) do(ctx context.Context, timeout time.Duration, method, path string, body any) (int, http.Header, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%w; check MANDATE_URL, and tell the owner if the gateway stays unreachable", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	if resp.StatusCode/100 == 2 {
		return resp.StatusCode, resp.Header, data, nil
	}
	var e struct {
		Error             string `json:"error"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if json.Unmarshal(data, &e) != nil || e.Error == "" {
		e.Error = strings.TrimSpace(string(data))
	}
	if e.Error == "" {
		e.Error = http.StatusText(resp.StatusCode)
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return 0, nil, nil, &exitError{
			msg:  fmt.Sprintf("%s; retry in %s", e.Error, time.Duration(e.RetryAfterSeconds)*time.Second),
			code: exitcode.ErrTryLater,
		}
	case http.StatusUnauthorized:
		return 0, nil, nil, errors.New("the gateway refused the token; ask the owner to check MANDATE_TOKEN")
	}
	return 0, nil, nil, errors.New(e.Error)
}
