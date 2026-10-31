package console

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"rsc.io/qr"

	"github.com/adeci/mandate/internal/connector"
)

const (
	healthTimeout = 5 * time.Second
	setupTimeout  = 30 * time.Second
)

type connView struct {
	Name   string
	Health connector.Health
}

func (s *Server) health(ctx context.Context, c *conn) connector.Health {
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()
	h, err := c.client.Health(ctx)
	if err != nil {
		return connector.Health{State: connector.Broken, Detail: err.Error()}
	}
	return h
}

func (s *Server) connections(ctx context.Context) []connView {
	out := make([]connView, 0, len(s.conns))
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	for _, c := range s.conns {
		wg.Go(func() {
			h := s.health(ctx, c)
			mu.Lock()
			out = append(out, connView{Name: c.name, Health: h})
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
		writeErr(w, &connector.Error{Status: http.StatusNotFound, Message: "no such connection"})
		return
	}
	writeJSON(w, http.StatusOK, s.health(r.Context(), c))
}

type setupRequest struct {
	// Phone asks for a pairing code for this number instead of a QR code.
	Phone string `json:"phone,omitempty"`
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
		writeErr(w, &connector.Error{Status: http.StatusNotFound, Message: "no such connection"})
		return
	}
	var req setupRequest
	if !decode(w, r, &req) {
		return
	}
	args := map[string]string{}
	if req.Phone != "" {
		args["phone"] = req.Phone
	}
	ctx, cancel := context.WithTimeout(r.Context(), setupTimeout)
	defer cancel()
	step, err := c.client.Setup(ctx, args)
	if err != nil {
		writeErr(w, upstream(err))
		return
	}
	v := setupView{Code: step.Code, Instructions: step.Instructions, Done: step.Done}
	if step.QR != "" {
		if v.QR, err = qrSVG(step.QR); err != nil {
			writeErr(w, fmt.Errorf("render QR code: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, v)
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
