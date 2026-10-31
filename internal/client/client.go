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

	"github.com/adeci/mandate/internal/connector"
	"github.com/adeci/mandate/internal/exitcode"
)

// Above the gateway's own 120 s connector read timeout, so its answer always
// wins. Long-polls add their wait on top.
const requestTimeout = 150 * time.Second

const (
	queryUsage   = "usage: mandate query SQL [PARAM...]"
	schemaUsage  = "usage: mandate schema"
	guideUsage   = "usage: mandate guide"
	readUsage    = "usage: mandate read CONNECTOR OP [KEY=VALUE...]"
	proposeUsage = "usage: mandate propose CONNECTOR OP [KEY=VALUE...] [--note TEXT] [--wait DURATION]"
	statusUsage  = "usage: mandate status ID [--wait DURATION]"
	httpUsage    = "usage: mandate http METHOD SERVICE PATH [--data BODY] [--note TEXT] [--wait DURATION]"
)

type client struct {
	url    string
	token  string
	http   *http.Client
	out    io.Writer
	errOut io.Writer
	table  bool
}

// fromEnv reads MANDATE_URL and MANDATE_TOKEN (or MANDATE_TOKEN_FILE).
func fromEnv(getenv func(string) string, out, errOut io.Writer, table bool) (*client, error) {
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
	return &client{url: url, token: token, http: &http.Client{}, out: out, errOut: errOut, table: table}, nil
}

func newClient() (*client, error) {
	fi, err := os.Stdout.Stat()
	table := err == nil && fi.Mode()&os.ModeCharDevice != 0
	return fromEnv(os.Getenv, os.Stdout, os.Stderr, table)
}

// Query runs `mandate query SQL [PARAM...]`.
func Query(ctx context.Context, args []string) error { return run(ctx, args, (*client).query) }

// Schema runs `mandate schema`.
func Schema(ctx context.Context, args []string) error { return run(ctx, args, (*client).schema) }

// Guide runs `mandate guide`.
func Guide(ctx context.Context, args []string) error { return run(ctx, args, (*client).guide) }

// Read runs `mandate read CONNECTOR OP [KEY=VALUE...]`.
func Read(ctx context.Context, args []string) error { return run(ctx, args, (*client).read) }

// Propose runs `mandate propose CONNECTOR OP [KEY=VALUE...] [--note TEXT] [--wait DURATION]`.
func Propose(ctx context.Context, args []string) error { return run(ctx, args, (*client).propose) }

// Status runs `mandate status ID [--wait DURATION]`.
func Status(ctx context.Context, args []string) error { return run(ctx, args, (*client).status) }

// HTTP runs `mandate http METHOD SERVICE PATH [--data BODY] [--note TEXT] [--wait DURATION]`.
func HTTP(ctx context.Context, args []string) error { return run(ctx, args, (*client).httpRequest) }

func run(ctx context.Context, args []string, cmd func(*client, context.Context, []string) error) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	return cmd(c, ctx, args)
}

// exitError carries a message for the agent and the exit status it maps to
// (via exitcode.For on code), without the code's own text.
type exitError struct {
	msg  string
	code error
}

func (e *exitError) Error() string { return e.msg }
func (e *exitError) Unwrap() error { return e.code }

func usage(text string, err error) error {
	if err != nil {
		text = err.Error() + "\n" + text
	}
	return &exitError{msg: text, code: exitcode.ErrUsage}
}

func (c *client) query(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return usage(queryUsage, nil)
	}
	params := make([]any, len(args)-1)
	for i, p := range args[1:] {
		params[i] = p
	}
	return c.rows(ctx, "/v1/query", map[string]any{"sql": args[0], "params": params})
}

func (c *client) read(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return usage(readUsage, nil)
	}
	kv, err := keyValues(args[2:])
	if err != nil {
		return usage(readUsage, err)
	}
	return c.rows(ctx, "/v1/read/"+url.PathEscape(args[0])+"/"+url.PathEscape(args[1]), map[string]any{"args": kv})
}

// keyValues parses KEY=VALUE args; values may contain '='.
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

// parseInterspersed lets flags follow positional arguments, which the flag
// package alone does not.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

type rowsResponse struct {
	Columns      []string `json:"columns"`
	Rows         [][]any  `json:"rows"`
	Truncated    bool     `json:"truncated"`
	TotalResults int      `json:"total_results"`
	Containers   int      `json:"containers"`
}

func (c *client) rows(ctx context.Context, path string, body any) error {
	var raw json.RawMessage
	if err := c.post(ctx, path, body, &raw); err != nil {
		return err
	}
	if !c.table {
		_, err := c.out.Write(append(raw, '\n'))
		return err
	}
	var res rowsResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return err
	}
	// Reads that store containers but return no messages, e.g. syncs.
	if res.Columns == nil {
		_, err := fmt.Fprintf(c.out, "%d containers synced, %d results\n", res.Containers, res.TotalResults)
		return err
	}
	w := tabwriter.NewWriter(c.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, strings.Join(res.Columns, "\t"))
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = cell(v)
		}
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	switch {
	case res.Truncated:
		fmt.Fprintf(c.out, "(truncated at %d rows)\n", len(res.Rows))
	case res.TotalResults > len(res.Rows):
		fmt.Fprintf(c.out, "(%d of %d results)\n", len(res.Rows), res.TotalResults)
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

type schemaInfo struct {
	Views []struct {
		Name        string   `json:"name"`
		Description string   `json:"description"`
		Columns     []string `json:"columns"`
	} `json:"views"`
	Connectors []struct {
		connector.Spec
		Health connector.Health `json:"health"`
	} `json:"connectors"`
	Limits struct {
		MaxRows int `json:"max_rows"`
	} `json:"limits"`
}

func (c *client) schema(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return usage(schemaUsage, nil)
	}
	var raw json.RawMessage
	if err := c.get(ctx, "/v1/schema", &raw); err != nil {
		return err
	}
	if !c.table {
		_, err := c.out.Write(append(raw, '\n'))
		return err
	}
	var s schemaInfo
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	for _, v := range s.Views {
		fmt.Fprintf(c.out, "%s (%s)\n  %s\n\n", v.Name, strings.Join(v.Columns, ", "), v.Description)
	}
	for _, spec := range s.Connectors {
		fmt.Fprintf(c.out, "connector %s: %s\n", spec.Name, health(spec.Health))
		for _, op := range spec.Ops {
			fmt.Fprintf(c.out, "  %s\n    %s\n", opUsage(spec.Name, op), op.Summary)
			for _, p := range op.Params {
				fmt.Fprintf(c.out, "    %s: %s%s\n", p.Name, p.Doc, paramTags(p))
			}
		}
		fmt.Fprintln(c.out)
	}
	return nil
}

func opUsage(name string, op connector.Op) string {
	verb := "read"
	if op.Kind == connector.Write {
		verb = "propose"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "mandate %s %s %s", verb, name, op.Name)
	for _, p := range op.Params {
		if p.Required {
			fmt.Fprintf(&b, " %s=...", p.Name)
		} else {
			fmt.Fprintf(&b, " [%s=...]", p.Name)
		}
	}
	return b.String()
}

func paramTags(p connector.Param) string {
	var tags []string
	if p.Container {
		tags = append(tags, "container")
	}
	if p.Editable {
		tags = append(tags, "owner may edit")
	}
	if len(tags) == 0 {
		return ""
	}
	return " (" + strings.Join(tags, ", ") + ")"
}

func health(h connector.Health) string {
	s := string(h.State)
	switch h.State {
	case connector.Healthy:
		return s
	case connector.Resting:
		if !h.Until.IsZero() {
			s += " until " + h.Until.UTC().Format(time.RFC3339)
		}
	default:
		s = "needs the owner (" + s + ")"
	}
	if h.Detail != "" {
		s += ": " + h.Detail
	}
	return s
}

func (c *client) get(ctx context.Context, path string, out any) error {
	data, err := c.send(ctx, requestTimeout, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (c *client) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	data, err := c.send(ctx, requestTimeout, http.MethodPost, path, b)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// send returns the body of a 2xx response; anything else is an error carrying
// the gateway's message, mapped to exit 75 on 429.
func (c *client) send(ctx context.Context, timeout time.Duration, method, path string, body []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w; check MANDATE_URL, and tell the owner if the gateway stays unreachable", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 == 2 {
		return data, nil
	}
	var e struct {
		Error             string `json:"error"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	if json.Unmarshal(data, &e) != nil || e.Error == "" {
		e.Error = strings.TrimSpace(string(data))
	}
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return nil, &exitError{
			msg:  fmt.Sprintf("%s; retry in %s", e.Error, time.Duration(e.RetryAfterSeconds)*time.Second),
			code: exitcode.ErrTryLater,
		}
	case http.StatusUnauthorized:
		return nil, errors.New("the gateway refused the token; ask the owner to check MANDATE_TOKEN")
	}
	return nil, fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode)
}
