package owner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/adeci/mandate/record"
)

type event struct {
	name string
	data []byte
}

// publish sends an event to every open stream. A stream that can't keep up
// is closed; the app refetches after reconnecting. The caller holds s.mu.
func (s *Server) publish(name string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	for ch := range s.subs {
		select {
		case ch <- event{name, b}:
		default:
			delete(s.subs, ch)
			close(ch)
		}
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	ch := make(chan event, 32)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if _, ok := s.subs[ch]; ok {
			delete(s.subs, ch)
			close(ch)
		}
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	io.WriteString(w, ": open\n\n")
	if rc.Flush() != nil {
		return
	}
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
		case <-ping.C:
			io.WriteString(w, ": ping\n\n")
		case <-r.Context().Done():
			return
		case <-s.stopping.Done():
			return
		}
		if rc.Flush() != nil {
			return
		}
	}
}

// loop pushes the live set and the key log after every change, retrying
// until they land, and every 15 seconds expires what ran out of time and
// checks the connectors' health.
func (s *Server) loop(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	retry := time.NewTimer(0) // push on start
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.housekeep(ctx)
			continue
		case <-s.push:
		case <-retry.C:
		}
		if err := s.sync(ctx); err != nil {
			s.log.Warn("push live set", "err", err)
			retry.Reset(5 * time.Second)
		}
	}
}

func (s *Server) housekeep(ctx context.Context) {
	s.mu.Lock()
	now := s.now()
	if changed, expired := s.st.expire(now); changed {
		_ = s.commit("now") // logged; the next change saves again
		for _, a := range expired {
			s.note(a)
		}
	}
	s.expireJoins(now)
	s.mu.Unlock()

	for name, c := range s.conns {
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		h, err := c.Health(hctx)
		cancel()
		state := h.State
		if err != nil {
			state = "down"
		}
		s.mu.Lock()
		if old, ok := s.health[name]; ok && old != state {
			s.publish("changed", map[string]string{"what": "connectors"})
		}
		s.health[name] = state
		s.mu.Unlock()
	}
}

// sync pushes the live set to the gateway and the key log to every
// connector. Once the gateway took a request's grant, the request says
// granted, so the agent's next read works.
func (s *Server) sync(ctx context.Context) error {
	s.mu.Lock()
	l := s.st.live(s.now())
	granting := map[string]string{}
	for id, r := range s.st.Requests {
		if r.Status == "pending" && r.GrantID != "" {
			granting[id] = r.GrantID
		}
	}
	s.mu.Unlock()

	if err := s.putLive(ctx, l); err != nil {
		return err
	}
	if len(granting) > 0 {
		s.mu.Lock()
		changed := false
		for id, gid := range granting {
			if r := s.st.Requests[id]; r != nil && r.Status == "pending" && r.GrantID == gid {
				r.Status, r.DecidedAt = "granted", s.now()
				changed = true
			}
		}
		if changed {
			_ = s.commit("now") // logged; the next change saves again
		}
		s.mu.Unlock()
	}
	var errs []error
	for name := range s.conns {
		errs = append(errs, s.pushKeys(ctx, name))
	}
	return errors.Join(errs...)
}

// pushKeys gives the connector the key log unless it has it already.
func (s *Server) pushKeys(ctx context.Context, name string) error {
	s.mu.Lock()
	n, head := s.keys.Head()
	entries := s.keys.Entries()
	done := n == 0 || s.pushedKeys[name] == head
	s.mu.Unlock()
	if done {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := s.conns[name].PutKeys(ctx, entries); err != nil {
		return fmt.Errorf("key log to connector %s: %w", name, err)
	}
	s.mu.Lock()
	s.pushedKeys[name] = head
	s.mu.Unlock()
	return nil
}

func (s *Server) putLive(ctx context.Context, l live) error {
	b, err := json.Marshal(l)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://gateway/internal/live", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.gateway.Do(req)
	if err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("gateway refused the live set: %s: %s", resp.Status, bytes.TrimSpace(msg))
	}
	return nil
}

// appendKey verifies e as the key log's next entry and keeps it. The caller
// holds s.mu.
func (s *Server) appendKey(e record.Envelope) error {
	if err := s.keys.Append(e); err != nil {
		return err
	}
	s.st.Keys = s.keys.Entries()
	return nil
}
