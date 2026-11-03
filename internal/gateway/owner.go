package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
)

// ownerClient calls the owner side's /internal/ routes. Refusals come back
// as *connector.Error, whose shape the owner side shares.
type ownerClient struct {
	socket string
	http   *http.Client
}

func dialOwner(socket string) *ownerClient {
	return &ownerClient{socket: socket, http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", socket)
			},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func (o *ownerClient) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://owner"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("owner side: %w", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return fmt.Errorf("owner side: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		e := &connector.Error{Status: resp.StatusCode}
		if json.Unmarshal(b, e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("owner side: decode %s %s: %w", method, path, err)
	}
	return nil
}

// joinRelay passes /agent/v1/join… on to the owner side's /internal/join…
// as it came, with the headers front set.
func (o *ownerClient) joinRelay() http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = "http"
			pr.Out.URL.Host = "owner"
			pr.Out.URL.Path = "/internal" + strings.TrimPrefix(pr.In.URL.Path, "/agent/v1")
			pr.Out.URL.RawPath = ""
			// Rewrite starts without X-Forwarded-For; front's is the client's.
			if xff := pr.In.Header.Get("X-Forwarded-For"); xff != "" {
				pr.Out.Header.Set("X-Forwarded-For", xff)
			}
			pr.Out.Header.Del("Authorization")
		},
		Transport: o.http.Transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() == nil {
				slog.Error("join relay", "path", r.URL.Path, "err", err)
			}
			writeError(w, http.StatusServiceUnavailable, "the owner side is not answering; try again later")
		},
	}
}

// relay answers an owner side failure: its refusals as they came, anything
// else as unavailable.
func relay(w http.ResponseWriter, cl *caller, what string, err error) {
	if e, ok := errors.AsType[*connector.Error](err); ok {
		writeJSON(w, e.Status, e)
		return
	}
	unavailable(w, cl, what, err)
}

func query(identity string, wait int) string {
	v := url.Values{"identity": {identity}}
	if wait > 0 {
		v.Set("wait", fmt.Sprint(wait))
	}
	return "?" + v.Encode()
}

type chatRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// entry is one read, as the owner shows it in the day's activity.
type entry struct {
	At        time.Time `json:"at"`
	Identity  string    `json:"identity"`
	Connector string    `json:"connector"`
	Verb      string    `json:"verb"`
	Chats     []chatRef `json:"chats"`
	Count     int       `json:"count"`
}

const (
	activityEvery = 2 * time.Second
	// maxPending bounds what waits for an owner side that does not answer.
	maxPending = 10000
)

// activity batches entries to the owner side, so a slow owner side never
// holds up reads.
type activity struct {
	owner *ownerClient
	log   *slog.Logger

	mu      sync.Mutex
	pending []entry
	dropped int
}

func (a *activity) add(e entry) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.pending) >= maxPending {
		a.dropped++
		return
	}
	a.pending = append(a.pending, e)
}

// run sends what is pending every activityEvery, and once more when ctx
// ends.
func (a *activity) run(ctx context.Context) {
	t := time.NewTicker(activityEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			flush, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			a.flush(flush)
			cancel()
			return
		case <-t.C:
			a.flush(ctx)
		}
	}
}

func (a *activity) flush(ctx context.Context) {
	a.mu.Lock()
	batch, dropped := a.pending, a.dropped
	a.pending, a.dropped = nil, 0
	a.mu.Unlock()
	if dropped > 0 {
		a.log.Warn("activity dropped while the owner side was not answering", "entries", dropped)
	}
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := a.owner.do(ctx, http.MethodPost, "/internal/activity", batch, nil); err != nil {
		a.log.Warn("send activity", "entries", len(batch), "err", err)
		a.mu.Lock()
		a.pending = append(batch, a.pending...)
		if over := len(a.pending) - maxPending; over > 0 {
			a.pending, a.dropped = a.pending[over:], a.dropped+over
		}
		a.mu.Unlock()
	}
}
