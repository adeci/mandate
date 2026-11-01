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

	"github.com/adeci/mandate/approval"
)

type Client struct {
	http *http.Client
}

// Timeout bounds each call; reads that wait for pacing need more than the
// default.
func Dial(path string, timeout time.Duration) *Client {
	return &Client{http: &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
		// A connector's redirect is an answer to relay, never one to follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Forward sends a raw read to /forward/ plus target, an escaped path with an
// optional query, as is. The caller closes the response body.
func (c *Client) Forward(ctx context.Context, method, target string, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://connector/forward/"+target, nil)
	if err != nil {
		return nil, err
	}
	req.Header = header
	return c.http.Do(req)
}

// Spec fails with a *ProtocolError for a connector that speaks another
// protocol version, since nothing else it says can be trusted to mean the
// same.
func (c *Client) Spec(ctx context.Context) (Spec, error) {
	var s Spec
	if err := c.Do(ctx, http.MethodGet, "/spec", nil, &s); err != nil {
		return Spec{}, err
	}
	if s.Protocol != ProtocolVersion {
		return Spec{}, &ProtocolError{Protocol: s.Protocol}
	}
	return s, nil
}

type ProtocolError struct {
	Protocol int
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("speaks protocol %d, this mandate speaks %d", e.Protocol, ProtocolVersion)
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.Do(ctx, http.MethodGet, "/health", nil, &h)
	return h, err
}

func (c *Client) Setup(ctx context.Context, args map[string]string) (SetupStep, error) {
	var s SetupStep
	err := c.Do(ctx, http.MethodPost, "/setup", setupRequest{Args: args}, &s)
	return s, err
}

func (c *Client) Unlink(ctx context.Context) error {
	return c.Do(ctx, http.MethodPost, "/unlink", nil, &struct{}{})
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

func (c *Client) Keys(ctx context.Context) (KeysHead, error) {
	var h KeysHead
	err := c.Do(ctx, http.MethodGet, "/keys", nil, &h)
	return h, err
}

// PutKeys offers log; the connector refuses it with a 409 *Error unless it
// extends the log it holds.
func (c *Client) PutKeys(ctx context.Context, log []approval.SignedChange) (KeysHead, error) {
	var h KeysHead
	err := c.Do(ctx, http.MethodPut, "/keys", log, &h)
	return h, err
}

// Media streams a stored message's file; the caller closes the body.
// Refusals come back as *Error.
func (c *Client) Media(ctx context.Context, message string, index int) (*http.Response, error) {
	b, err := json.Marshal(mediaRequest{Message: message, Index: index})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://connector/media", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, refusal(resp)
	}
	return resp, nil
}

// Do sends a JSON request over the socket; refusals come back as *Error.
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
	if resp.StatusCode/100 != 2 {
		return refusal(resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s response: %w", path, err)
	}
	return nil
}

func refusal(resp *http.Response) error {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	e := &Error{Status: resp.StatusCode}
	if json.Unmarshal(data, e) != nil || e.Message == "" {
		e.Message = strings.TrimSpace(string(data))
	}
	return e
}
