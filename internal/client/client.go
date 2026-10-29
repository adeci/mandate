// Package client implements the commands agents run against a gateway:
// query, schema, and on-demand Discord reads.
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
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/adeci/sieve/internal/discordapi"
	"github.com/adeci/sieve/internal/exitcode"
)

// Above the gateway's own 110 s Discord timeout, so its answer always wins.
const requestTimeout = 2 * time.Minute

type client struct {
	url   string
	token string
	http  *http.Client
	out   io.Writer
	table bool
}

// fromEnv reads SIEVE_URL and SIEVE_TOKEN (or SIEVE_TOKEN_FILE).
func fromEnv(getenv func(string) string, out io.Writer, table bool) (*client, error) {
	url := strings.TrimRight(getenv("SIEVE_URL"), "/")
	if url == "" {
		return nil, errors.New("SIEVE_URL is not set (e.g. http://lachesis:8780)")
	}
	token := getenv("SIEVE_TOKEN")
	if token == "" && getenv("SIEVE_TOKEN_FILE") != "" {
		b, err := os.ReadFile(getenv("SIEVE_TOKEN_FILE"))
		if err != nil {
			return nil, err
		}
		token = strings.TrimSpace(string(b))
	}
	if token == "" {
		return nil, errors.New("SIEVE_TOKEN or SIEVE_TOKEN_FILE is not set")
	}
	return &client{url: url, token: token, http: &http.Client{Timeout: requestTimeout}, out: out, table: table}, nil
}

func newClient() (*client, error) {
	fi, err := os.Stdout.Stat()
	table := err == nil && fi.Mode()&os.ModeCharDevice != 0
	return fromEnv(os.Getenv, os.Stdout, table)
}

// Query runs `sieve query SQL [PARAM...]`.
func Query(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: sieve query SQL [PARAM...]")
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

// Schema runs `sieve schema`.
func Schema(ctx context.Context, args []string) error {
	if len(args) != 0 {
		return errors.New("usage: sieve schema")
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	return c.schema(ctx)
}

// Discord runs `sieve discord sync|search|fetch`.
func Discord(ctx context.Context, args []string) error {
	const usage = `usage: sieve discord sync [GUILD_ID]
       sieve discord search GUILD_ID [TEXT] [--author ID] [--channel ID] [--has KIND] [--offset N]
       sieve discord fetch CHANNEL_ID [--before ID | --after ID | --around ID] [--limit N]`
	if len(args) == 0 {
		return errors.New(usage)
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	return c.discord(ctx, args[0], args[1:], usage)
}

func (c *client) discord(ctx context.Context, sub string, args []string, usage string) error {
	switch sub {
	case "sync":
		if len(args) > 1 {
			return errors.New(usage)
		}
		var req discordapi.SyncRequest
		if len(args) == 1 {
			req.GuildID = args[0]
		}
		var resp discordapi.SyncResponse
		if err := c.post(ctx, "/v1/discord/sync", req, &resp); err != nil {
			return err
		}
		_, err := fmt.Fprintf(c.out, "%d containers synced\n", resp.Containers)
		return err

	case "search":
		fs := flag.NewFlagSet("search", flag.ContinueOnError)
		var req discordapi.SearchRequest
		fs.StringVar(&req.AuthorID, "author", "", "author user ID")
		fs.StringVar(&req.ChannelID, "channel", "", "channel ID")
		fs.StringVar(&req.Has, "has", "", "link, embed, file, video, image, or sound")
		fs.IntVar(&req.Offset, "offset", 0, "skip this many results (pages of 25)")
		positional, err := parseInterspersed(fs, args)
		if err != nil {
			return err
		}
		if len(positional) < 1 || len(positional) > 2 {
			return errors.New(usage)
		}
		req.GuildID = positional[0]
		if len(positional) == 2 {
			req.Content = positional[1]
		}
		return c.rows(ctx, "/v1/discord/search", req)

	case "fetch":
		fs := flag.NewFlagSet("fetch", flag.ContinueOnError)
		var req discordapi.FetchRequest
		fs.StringVar(&req.Before, "before", "", "messages before this message ID")
		fs.StringVar(&req.After, "after", "", "messages after this message ID")
		fs.StringVar(&req.Around, "around", "", "messages around this message ID")
		fs.IntVar(&req.Limit, "limit", 50, "messages to fetch (1-100)")
		positional, err := parseInterspersed(fs, args)
		if err != nil {
			return err
		}
		if len(positional) != 1 {
			return errors.New(usage)
		}
		req.ChannelID = positional[0]
		return c.rows(ctx, "/v1/discord/fetch", req)
	}
	return errors.New(usage)
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
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return err
	}
	for _, v := range s.Views {
		fmt.Fprintf(c.out, "%s (%s)\n  %s\n\n", v.Name, strings.Join(v.Columns, ", "), v.Description)
	}
	return nil
}

func (c *client) get(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, out)
}

func (c *client) post(ctx context.Context, path string, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, path, b, out)
}

func (c *client) do(ctx context.Context, method, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, c.url+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 == 2 {
		return json.Unmarshal(data, out)
	}
	var e discordapi.ErrorResponse
	if json.Unmarshal(data, &e) != nil || e.Error == "" {
		e.Error = strings.TrimSpace(string(data))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%s; retry in %s: %w", e.Error,
			time.Duration(e.RetryAfterSeconds)*time.Second, exitcode.ErrTryLater)
	}
	return fmt.Errorf("%s (HTTP %d)", e.Error, resp.StatusCode)
}
