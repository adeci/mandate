package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/adeci/mandate/internal/approval"
)

// Client talks to a connector over its Unix socket.
type Client struct {
	http *http.Client
}

// Dial returns a client for the connector socket at path. Timeout bounds each
// call; reads that wait for pacing need more than the default.
func Dial(path string, timeout time.Duration) *Client {
	return &Client{http: &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}}
}

func (c *Client) Spec(ctx context.Context) (Spec, error) {
	var s Spec
	err := c.Do(ctx, http.MethodGet, "/spec", nil, &s)
	return s, err
}

func (c *Client) Read(ctx context.Context, op string, args map[string]string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.Do(ctx, http.MethodPost, "/read", opRequest{Op: op, Args: args}, &out)
	return out, err
}

func (c *Client) Prepare(ctx context.Context, op string, args map[string]string) (Prepared, error) {
	var p Prepared
	err := c.Do(ctx, http.MethodPost, "/prepare", opRequest{Op: op, Args: args}, &p)
	return p, err
}

func (c *Client) Execute(ctx context.Context, a approval.Approval) (Result, error) {
	var r Result
	err := c.Do(ctx, http.MethodPost, "/execute", a, &r)
	return r, err
}

// Do sends a JSON request over the socket. Refusals come back as *Error so
// callers can relay status and message.
func (c *Client) Do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://connector"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		e := &Error{Status: resp.StatusCode}
		if json.Unmarshal(data, e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return e
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}
