package console

import (
	"context"
	"errors"
	"fmt"
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
)

type connView struct {
	Name   string
	Health connector.Health
	// Setup are the inputs the connector takes when linking.
	Setup []connector.Param
	// Keys is why the connector's key log differs from the console's.
	Keys string
}

// view asks for the spec first: a connector speaking another protocol is
// down whatever its health says.
func (s *Server) view(ctx context.Context, c *conn) connView {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	v := connView{Name: c.name}
	spec, err := c.getSpec(ctx, s.now())
	if err != nil {
		v.Health = connector.Health{State: connector.Broken, Detail: problem(err)}
		return v
	}
	v.Setup = spec.Setup
	if v.Health, err = c.client.Health(ctx); err != nil {
		v.Health = connector.Health{State: connector.Broken, Detail: problem(err)}
	}
	v.Keys = s.syncConn(ctx, c)
	return v
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
	slices.SortFunc(out, func(a, b connView) int { return strings.Compare(a.Name, b.Name) })
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
// count as not given.
type setupRequest struct {
	Args map[string]string `json:"args,omitempty"`
}

type setupView struct {
	QR           string `json:"qr_svg,omitempty"`
	Code         string `json:"code,omitempty"`
	Instructions string `json:"instructions,omitempty"`
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
	spec, err := c.getSpec(ctx, s.now())
	if pe, ok := errors.AsType[*connector.ProtocolError](err); ok {
		connector.WriteError(w, &connector.Error{Status: http.StatusBadGateway, Message: c.name + " " + pe.Error()})
		return
	}
	if err != nil {
		connector.WriteError(w, upstream(err))
		return
	}
	args := map[string]string{}
	for k, v := range req.Args {
		if !slices.ContainsFunc(spec.Setup, func(p connector.Param) bool { return p.Name == k }) {
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
	v := setupView{Code: step.Code, Instructions: step.Instructions, Done: step.Done}
	if step.QR != "" {
		if v.QR, err = qrSVG(step.QR); err != nil {
			connector.WriteError(w, fmt.Errorf("render QR code: %w", err))
			return
		}
	}
	connector.WriteJSON(w, http.StatusOK, v)
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
