package console

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"rsc.io/qr"

	"github.com/adeci/mandate/connector"
)

const (
	healthTimeout = 5 * time.Second
	setupTimeout  = 30 * time.Second
	// downGrace is how long a connector may be down before the console
	// shows it as failed: services have short outages and phones sleep,
	// and connectors retry by themselves.
	downGrace = 3 * time.Minute
)

type connView struct {
	Name   string
	Health connector.Health
	// Tone is how the state shows; Retrying marks a connector down for less
	// than downGrace.
	Tone     string
	Retrying bool
	// Setup are the inputs the connector takes when linking. With Required
	// set it links only with them, all given at once.
	Setup    []connector.Param
	Required bool
	Unlink   bool
	// Keys is why the connector's key log differs from the console's.
	Keys string
}

func (s *Server) view(ctx context.Context, c *conn) connView {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	v := connView{Name: c.name}
	spec, err := s.check(ctx, c)
	v.Health, v.Tone = c.shown(s.now())
	v.Retrying = v.Health.State == connector.Broken && v.Tone != "alert"
	if err != nil {
		return v
	}
	v.Setup, v.Unlink = spec.Setup, spec.Unlink
	v.Required = slices.ContainsFunc(spec.Setup, func(p connector.Param) bool { return p.Required })
	v.Keys = s.syncConn(ctx, c)
	return v
}

// check asks c for its health and records it, and returns the spec or why
// there is none. A connector speaking another protocol is down whatever its
// health says.
func (s *Server) check(ctx context.Context, c *conn) (connector.Spec, error) {
	spec, specErr := c.getSpec(ctx, s.now())
	h, err := connector.Health{}, specErr
	if err == nil {
		h, err = c.client.Health(ctx)
	}
	if err != nil {
		h = connector.Health{State: connector.Broken, Detail: problem(err)}
	}
	now := s.now()
	c.hmu.Lock()
	defer c.hmu.Unlock()
	if h.State != connector.Broken {
		c.downSince = time.Time{}
	} else if c.downSince.IsZero() {
		c.downSince = now
	}
	c.health = h
	return spec, specErr
}

// shown is c's last health and its tone; down is a wait until downGrace has
// passed.
func (c *conn) shown(now time.Time) (connector.Health, string) {
	c.hmu.Lock()
	defer c.hmu.Unlock()
	t := tone(c.health.State)
	if c.health.State == connector.Broken && now.Sub(c.downSince) < downGrace {
		t = "wait"
	}
	return c.health, t
}

// Watch checks every connector's health now and every interval, so the nav
// and open pages follow it.
func (s *Server) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		var wg sync.WaitGroup
		for _, c := range s.conns {
			wg.Go(func() {
				ctx, cancel := context.WithTimeout(ctx, healthTimeout)
				defer cancel()
				_, _ = s.check(ctx, c)
			})
		}
		wg.Wait()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// health sums up the connectors as last seen: each one's state and tone in
// name order, how many need attention, and the worst tone among those.
func (s *Server) health() (states string, n int, worst string) {
	names := slices.Sorted(maps.Keys(s.conns))
	now := s.now()
	for _, name := range names {
		h, t := s.conns[name].shown(now)
		states += "." + string(h.State) + "/" + t
		switch t {
		case "alert":
			n, worst = n+1, t
		case "wait":
			n++
			if worst == "" {
				worst = t
			}
		}
	}
	return states, n, worst
}

// unreachable means nothing serves the connector's socket.
func unreachable(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

func problem(err error) string {
	if unreachable(err) {
		return "not running"
	}
	if pe, ok := errors.AsType[*connector.ProtocolError](err); ok {
		return pe.Error()
	}
	return err.Error()
}

func (s *Server) connections(ctx context.Context) []connView {
	out := make([]connView, 0, len(s.conns))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, c := range s.conns {
		wg.Go(func() {
			v := s.view(ctx, c)
			mu.Lock()
			out = append(out, v)
			mu.Unlock()
		})
	}
	wg.Wait()
	// Connections that need the owner come first.
	slices.SortFunc(out, func(a, b connView) int {
		aOK, bOK := a.Health.State == connector.Healthy, b.Health.State == connector.Healthy
		if aOK != bOK {
			if bOK {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

func (s *Server) connectionHealth(w http.ResponseWriter, r *http.Request, _ session) {
	c, ok := s.conns[r.PathValue("name")]
	if !ok {
		connector.WriteError(w, &connector.Error{Status: http.StatusNotFound, Message: "no such connection"})
		return
	}
	connector.WriteJSON(w, http.StatusOK, s.view(r.Context(), c).Health)
}

// setupRequest carries the connector's declared setup inputs; empty ones
// count as not given. An answer to a step's question comes alone, as
// connector.SetupConfirm.
type setupRequest struct {
	Args map[string]string `json:"args,omitempty"`
}

type setupView struct {
	QR           string `json:"qr_svg,omitempty"`
	Code         string `json:"code,omitempty"`
	Instructions string `json:"instructions,omitempty"`
	Confirm      string `json:"confirm,omitempty"`
	Done         bool   `json:"done,omitempty"`
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request, _ session) {
	c, ok := s.conns[r.PathValue("name")]
	if !ok {
		connector.WriteError(w, &connector.Error{Status: http.StatusNotFound, Message: "no such connection"})
		return
	}
	var req setupRequest
	if !connector.DecodeJSON(w, r, &req, maxBody) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), setupTimeout)
	defer cancel()
	spec, ok := s.linkSpec(ctx, w, c)
	if !ok {
		return
	}
	args := map[string]string{}
	for k, v := range req.Args {
		if k == connector.SetupConfirm {
			if len(req.Args) != 1 || v != "yes" && v != "no" {
				connector.WriteError(w, &connector.Error{Status: http.StatusBadRequest, Message: "confirm is yes or no, given alone"})
				return
			}
		} else if !slices.ContainsFunc(spec.Setup, func(p connector.Param) bool { return p.Name == k }) {
			connector.WriteError(w, &connector.Error{Status: http.StatusBadRequest, Message: fmt.Sprintf("%s takes no setup input %q", c.name, k)})
			return
		}
		if v != "" {
			args[k] = v
		}
	}
	step, err := c.client.Setup(ctx, args)
	if err != nil {
		connector.WriteError(w, upstream(err))
		return
	}
	v := setupView{Code: step.Code, Instructions: step.Instructions, Confirm: step.Confirm, Done: step.Done}
	if step.QR != "" {
		if v.QR, err = qrSVG(step.QR); err != nil {
			connector.WriteError(w, fmt.Errorf("render QR code: %w", err))
			return
		}
	}
	connector.WriteJSON(w, http.StatusOK, v)
}

func (s *Server) unlink(w http.ResponseWriter, r *http.Request, _ session) {
	c, ok := s.conns[r.PathValue("name")]
	if !ok {
		connector.WriteError(w, &connector.Error{Status: http.StatusNotFound, Message: "no such connection"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), setupTimeout)
	defer cancel()
	if _, ok := s.linkSpec(ctx, w, c); !ok {
		return
	}
	if err := c.client.Unlink(ctx); err != nil {
		connector.WriteError(w, upstream(err))
		return
	}
	connector.WriteJSON(w, http.StatusOK, struct{}{})
}

// linkSpec writes the error when c can't be asked to link or unlink.
func (s *Server) linkSpec(ctx context.Context, w http.ResponseWriter, c *conn) (connector.Spec, bool) {
	spec, err := c.getSpec(ctx, s.now())
	if pe, ok := errors.AsType[*connector.ProtocolError](err); ok {
		connector.WriteError(w, &connector.Error{Status: http.StatusBadGateway, Message: c.name + " " + pe.Error()})
		return spec, false
	}
	if err != nil {
		connector.WriteError(w, upstream(err))
		return spec, false
	}
	return spec, true
}

// qrSVG draws text as a QR code with a 4-module quiet zone, black on white
// in either theme so phone cameras read it.
func qrSVG(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4
	n := code.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" class="qr" role="img" aria-label="QR code" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := range code.Size {
		for x := 0; x < code.Size; x++ {
			if !code.Black(x, y) {
				continue
			}
			run := 1
			for code.Black(x+run, y) {
				run++
			}
			fmt.Fprintf(&b, "M%d %dh%dv1h-%dz", x+quiet, y+quiet, run, run)
			x += run
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}
