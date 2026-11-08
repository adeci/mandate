package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/adeci/mandate/record"
)

// Client calls a connector over its socket, as mandate's gateway or owner
// side. Refusals come back as *Error. Calls are bounded by their context.
type Client struct {
	name string
	http *http.Client
}

// Dial returns a client for the connector mandate configured as name,
// listening at socket.
func Dial(name, socket string) *Client {
	return &Client{name: name, http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
		// A connector's redirect is an answer to relay, never one to follow.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Spec refuses a connector of another name or protocol, since nothing else
// it says can be trusted to mean the same.
func (c *Client) Spec(ctx context.Context) (Spec, error) {
	var s Spec
	if err := c.do(ctx, http.MethodGet, "/spec", nil, &s); err != nil {
		return Spec{}, err
	}
	if s.Protocol != ProtocolVersion {
		return Spec{}, fmt.Errorf("connector %s speaks protocol %d, not %d", c.name, s.Protocol, ProtocolVersion)
	}
	if s.Name != c.name {
		return Spec{}, fmt.Errorf("connector %s calls itself %q", c.name, s.Name)
	}
	return s, nil
}

func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	err := c.do(ctx, http.MethodGet, "/health", nil, &h)
	return h, err
}

func (c *Client) Keys(ctx context.Context) (KeysHead, error) {
	var h KeysHead
	err := c.do(ctx, http.MethodGet, "/keys", nil, &h)
	return h, err
}

// PutKeys offers the whole key log; the connector refuses with a 409 unless
// it extends the one held.
func (c *Client) PutKeys(ctx context.Context, entries []record.Envelope) (KeysHead, error) {
	var h KeysHead
	err := c.do(ctx, http.MethodPut, "/keys", entries, &h)
	return h, err
}

func (c *Client) Prepare(ctx context.Context, op string, args map[string]string) (Prepared, error) {
	var p Prepared
	err := c.do(ctx, http.MethodPost, "/prepare", opRequest{Op: op, Args: args}, &p)
	return p, err
}

func (c *Client) Read(ctx context.Context, op string, args map[string]string) (ReadResult, error) {
	var r ReadResult
	err := c.do(ctx, http.MethodPost, "/read", opRequest{Op: op, Args: args}, &r)
	return r, err
}

// Media streams a stored message's file of at most limit bytes; the caller
// closes the body.
func (c *Client) Media(ctx context.Context, message string, index int, limit int64) (*http.Response, error) {
	resp, err := c.send(ctx, http.MethodPost, "/media", mediaRequest{Message: message, Index: index, Limit: limit})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, refusal(resp)
	}
	return resp, nil
}

// Icon fetches a chat's picture and its version; a chat without one is a 404
// *Error.
func (c *Client) Icon(ctx context.Context, chat string) (Icon, string, error) {
	resp, err := c.send(ctx, http.MethodGet, "/icon?"+url.Values{"chat": {chat}}.Encode(), nil)
	if err != nil {
		return Icon{}, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Icon{}, "", refusal(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, MaxIcon+1))
	if err != nil {
		return Icon{}, "", fmt.Errorf("connector %s: read the picture: %w", c.name, err)
	}
	if len(b) > MaxIcon {
		return Icon{}, "", fmt.Errorf("connector %s: the picture is larger than %d bytes", c.name, MaxIcon)
	}
	return Icon{Type: resp.Header.Get("Content-Type"), Body: b}, resp.Header.Get("Mandate-Icon"), nil
}

func (c *Client) Setup(ctx context.Context, step string, values map[string]string) (Step, error) {
	var s Step
	err := c.do(ctx, http.MethodPost, "/setup", setupRequest{Step: step, Values: values}, &s)
	return s, err
}

func (c *Client) Unlink(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/unlink", struct{}{}, nil)
}

// Execute sends a signed write approval.
func (c *Client) Execute(ctx context.Context, approval record.Envelope) (Result, error) {
	var r Result
	err := c.do(ctx, http.MethodPost, "/execute", approval, &r)
	return r, err
}

// Containers lists stored chats as q asks.
func (c *Client) Containers(ctx context.Context, q ContainerQuery) ([]Container, error) {
	v := url.Values{}
	if q.Q != "" {
		v.Set("q", q.Q)
	}
	if q.Parent != nil {
		v.Set("parent", *q.Parent)
	}
	if q.IDs != nil {
		v.Set("ids", strings.Join(q.IDs, ","))
	}
	if q.Limit > 0 {
		v.Set("limit", strconv.Itoa(q.Limit))
	}
	var out []Container
	err := c.do(ctx, http.MethodGet, "/containers?"+v.Encode(), nil, &out)
	return out, err
}

func (c *Client) send(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://connector"+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("connector %s: %w", c.name, err)
	}
	return resp, nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.send(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return refusal(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("connector %s: decode %s %s: %w", c.name, method, path, err)
	}
	return nil
}

func refusal(resp *http.Response) error {
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	e := &Error{Status: resp.StatusCode}
	if json.Unmarshal(b, e) != nil || e.Message == "" {
		e.Message = strings.TrimSpace(string(b))
	}
	return e
}
