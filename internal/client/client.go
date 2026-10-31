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
	"github.com/adeci/mandate/internal/proposal"
)

// Above the gateway's own 120 s connector read timeout, so its answer always
// wins.
const requestTimeout = 150 * time.Second

const (
	readUsage    = "usage: mandate read CONNECTOR OP [KEY=VALUE...]"
	proposeUsage = "usage: mandate propose CONNECTOR OP [KEY=VALUE...] [--note TEXT]"
	actionUsage  = "usage: mandate action ID"
	httpUsage    = "usage: mandate http METHOD SERVICE PATH [--data JSON]"
)

type client struct {
	url   string
	token string
	http  *http.Client
	out   io.Writer
	table bool
}

// fromEnv reads MANDATE_URL and MANDATE_TOKEN (or MANDATE_TOKEN_FILE).
func fromEnv(getenv func(string) string, out io.Writer, table bool) (*client, error) {
	url := strings.TrimRight(getenv("MANDATE_URL"), "/")
	if url == "" {
		return nil, errors.New("MANDATE_URL is not set (e.g. http://lachesis:8780)")
	}
	token := getenv("MANDATE_TOKEN")
	if token == "" && getenv("MANDATE_TOKEN_FILE") != "" {
		b, err := os.ReadFile(getenv("MANDATE_TOKEN_FILE"))
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return nil, errors.New("MANDATE_TOKEN or MANDATE_TOKEN_FILE is not set")
	}
	return &client{url: url, token: token, http: &http.Client{Timeout: requestTimeout}, out: out, table: table}, nil
}

func newClient() (*client, error) {
	fi, err := os.Stdout.Stat()
	table := err == nil && fi.Mode()&os.ModeCharDevice != 0
	return fromEnv(os.Getenv, os.Stdout, table)
}

// Query runs `mandate query SQL [PARAM...]`.
func Query(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mandate query SQL [PARAM...]")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	params := make([]any, len(args)-1)
	for i, p := range args[1:] {
		params[i] = p
	}
	return c.rows(ctx, "/v1/query", map[string]any{"sql": args[0], "params": params})
}

// Schema runs `mandate schema`.
func Schema(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: mandate schema")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	return c.schema(ctx)
}

// Read runs `mandate read CONNECTOR OP [KEY=VALUE...]`.
func Read(ctx context.Context, args []string) error {
	return run(ctx, args, (*client).read)
}

// Propose runs `mandate propose CONNECTOR OP [KEY=VALUE...] [--note TEXT]`.
func Propose(ctx context.Context, args []string) error {
	return run(ctx, args, (*client).propose)
}

// Action runs `mandate action ID`.
func Action(ctx context.Context, args []string) error {
	return run(ctx, args, (*client).action)
}

// HTTP runs `mandate http METHOD SERVICE PATH [--data JSON]`.
func HTTP(ctx context.Context, args []string) error {
	return run(ctx, args, (*client).passthrough)
}

func run(ctx context.Context, args []string, cmd func(*client, context.Context, []string) error) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	return cmd(c, ctx, args)
}

func (c *client) read(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New(readUsage)
	}
	kv, err := keyValues(args[2:])
	if err != nil {
		return fmt.Errorf("%w\n%s", err, readUsage)
	}
	return c.rows(ctx, "/v1/read/"+url.PathEscape(args[0])+"/"+url.PathEscape(args[1]), map[string]any{"args": kv})
}

func (c *client) propose(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("propose", flag.ContinueOnError)
	note := fs.String("note", "", "why, shown to the owner")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, proposeUsage)
	}
	if len(positional) < 2 {
		return errors.New(proposeUsage)
	}
	kv, err := keyValues(positional[2:])
	if err != nil {
		return fmt.Errorf("%w\n%s", err, proposeUsage)
	}
	var raw json.RawMessage
	if err := c.post(ctx, "/v1/actions", proposal.Request{
		Connector: positional[0], Op: positional[1], Args: kv, Note: *note,
	}, &raw); err != nil {
		return err
	}
	if !c.table {
		_, err := c.out.Write(append(raw, '\n'))
		return err
	}
	var p proposal.Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	printProposal(c.out, p)
	fmt.Fprintf(c.out, "waiting for the owner; check with: mandate action %s\n", p.ID)
	return nil
}

func (c *client) action(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return errors.New(actionUsage)
	}
	var raw json.RawMessage
	if err := c.get(ctx, "/v1/actions/"+url.PathEscape(args[0]), &raw); err != nil {
		return err
	}
	var p proposal.Proposal
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	if c.table {
		printProposal(c.out, p)
	} else if _, err := c.out.Write(append(raw, '\n')); err != nil {
		return err
	}
	if p.Status == proposal.Pending || p.Status == proposal.Executing {
		return fmt.Errorf("proposal %s is %s; the owner has not decided yet, check again later: %w",
			p.ID, p.Status, exitcode.ErrTryLater)
	}
	return nil
}

func printProposal(w io.Writer, p proposal.Proposal) {
	fmt.Fprintf(w, "%s  %s  %s %s\n", p.ID, p.Status, p.Connector, p.Op)
	for _, f := range p.Display {
		fmt.Fprintf(w, "  %s: %s\n", f.Label, f.Value)
	}
	if r := p.Result; r != nil {
		fmt.Fprintf(w, "result: %s", r.Status)
		if r.Ref != "" {
			fmt.Fprintf(w, " ref=%s", r.Ref)
		}
		if r.Detail != "" {
			fmt.Fprintf(w, " (%s)", r.Detail)
		}
		fmt.Fprintln(w)
	}
}

func (c *client) passthrough(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("http", flag.ContinueOnError)
	data := fs.String("data", "", "JSON request body")
	positional, err := parseInterspersed(fs, args)
	if err != nil {
		return fmt.Errorf("%w\n%s", err, httpUsage)
	}
	if len(positional) != 3 {
		return errors.New(httpUsage)
	}
	var body []byte
	if *data != "" {
		if !json.Valid([]byte(*data)) {
			return errors.New("--data is not valid JSON")
		}
		body = []byte(*data)
	}
	method, service, path := strings.ToUpper(positional[0]), positional[1], strings.TrimPrefix(positional[2], "/")
	out, err := c.send(ctx, method, "/v1/http/"+url.PathEscape(service)+"/"+path, body)
	if err != nil {
		return err
	}
	_, err = c.out.Write(out)
	return err
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

func (c *client) schema(ctx context.Context) error {
	var raw json.RawMessage
	if err := c.get(ctx, "/v1/schema", &raw); err != nil {
		return err
	}
	if !c.table {
		_, err := c.out.Write(append(raw, '\n'))
		return err
	}
	var s struct {
		Views []struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Columns     []string `json:"columns"`
		} `json:"views"`
		Connectors []connector.Spec `json:"connectors"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	for _, v := range s.Views {
		fmt.Fprintf(c.out, "%s (%s)\n  %s\n\n", v.Name, strings.Join(v.Columns, ", "), v.Description)
	}
	for _, spec := range s.Connectors {
		fmt.Fprintf(c.out, "connector %s\n", spec.Name)
		for _, op := range spec.Ops {
			verb := "read"
			if op.Kind == connector.Write {
				verb = "propose"
			}
			fmt.Fprintf(c.out, "  mandate %s %s %s", verb, spec.Name, op.Name)
			for _, p := range op.Params {
				if p.Required {
					fmt.Fprintf(c.out, " %s=...", p.Name)
				} else {
					fmt.Fprintf(c.out, " [%s=...]", p.Name)
				}
			}
			fmt.Fprintf(c.out, "\n    %s\n", op.Summary)
			for _, p := range op.Params {
				var tags []string
				if p.Container {
					tags = append(tags, "container")
				}
				if p.Editable {
					tags = append(tags, "owner may edit")
				}
				suffix := ""
				if len(tags) > 0 {
					suffix = " (" + strings.Join(tags, ", ") + ")"
				}
				fmt.Fprintf(c.out, "    %s: %s%s\n", p.Name, p.Doc, suffix)
			}
		}
		fmt.Fprintln(c.out)
	}
	return nil
}

func (c *client) get(ctx context.Context, path string, out any) error {
	data, err := c.send(ctx, http.MethodGet, path, nil)
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
	data, err := c.send(ctx, http.MethodPost, path, b)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// send returns the body of a 2xx response; anything else is an error carrying
// the gateway's message, wrapping ErrTryLater on 429.
func (c *client) send(ctx context.Context, method, path string, body []byte) ([]byte, error) {
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
		return nil, err
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
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("%s; retry in %s: %w", e.Error,
			time.Duration(e.RetryAfterSeconds)*time.Second, exitcode.ErrTryLater)
	}
	return nil, fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode)
}
