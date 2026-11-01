package gateway

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"time"
)

// media streams a file of a message from its connector. The message is looked
// up through the same filter as messages, so a hidden chat's or sender's
// files are as unknown as their messages.
func (h *handler) media(w http.ResponseWriter, r *http.Request, name string) {
	start := time.Now()
	q := r.URL.Query()
	chat := q.Get("chat")
	seq, err := strconv.ParseInt(q.Get("seq"), 10, 64)
	if chat == "" || err != nil || seq < 1 {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("chat and seq are required: the chat's id and the message's seq from mandate %s messages", name))
		return
	}
	index, err := intParam(q, "index", 0, 0, math.MaxInt32)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	spec, ok := h.spec(w, r, name)
	if !ok {
		return
	}
	if !spec.Media {
		writeError(w, http.StatusNotFound, fmt.Sprintf("connector %s serves no media; see mandate %s --help", name, name))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), h.cfg.Timeout)
	defer cancel()
	s, ok := h.source(ctx, w, name)
	if !ok {
		return
	}
	if !s.check(w, name, chat) {
		s.close()
		return
	}
	m, err := s.mediaMessage(chat, seq, index)
	s.close()
	if err != nil {
		slog.Error("media lookup", "source", name, "err", err)
		writeError(w, http.StatusServiceUnavailable, "gateway unavailable; tell the owner")
		return
	}
	switch {
	case m == nil:
		writeError(w, http.StatusNotFound, fmt.Sprintf("no message %d in chat %q: run mandate %s messages %s", seq, chat, name, shellWord(chat)))
		return
	case m.deleted:
		writeError(w, http.StatusNotFound, fmt.Sprintf("message %d was deleted", seq))
		return
	case m.auth:
		writeError(w, http.StatusForbidden, fmt.Sprintf("message %d may hold a login code, so its media is withheld", seq))
		return
	case m.items == 0 && !m.marked:
		writeError(w, http.StatusNotFound, fmt.Sprintf("message %d has no media", seq))
		return
	case m.items > 0 && index >= m.items:
		writeError(w, http.StatusNotFound, fmt.Sprintf("message %d has %d media files: --index 0 to %d", seq, m.items, m.items-1))
		return
	case m.size > h.cfg.MaxMedia:
		writeError(w, http.StatusRequestEntityTooLarge, tooLarge(m.size, h.cfg.MaxMedia))
		return
	}
	if !h.healthy(w, r, name) {
		return
	}

	resp, err := h.connectors.media[name].Media(r.Context(), m.id, int(index))
	attrs := []any{"remote", r.RemoteAddr, "connector", name, "seq", seq, "index", index}
	if err != nil {
		slog.Info("media", append(attrs, "err", err.Error())...)
		relay(w, "connector "+name, err)
		return
	}
	defer resp.Body.Close()
	switch {
	case resp.ContentLength < 0:
		slog.Error("media without length", attrs...)
		writeError(w, http.StatusBadGateway, fmt.Sprintf("connector %s sent media without a length; tell the owner", name))
		return
	case resp.ContentLength > h.cfg.MaxMedia:
		writeError(w, http.StatusRequestEntityTooLarge, tooLarge(resp.ContentLength, h.cfg.MaxMedia))
		return
	}
	w.Header().Set("Content-Type", cmp.Or(resp.Header.Get("Content-Type"), "application/octet-stream"))
	w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	if d := resp.Header.Get("Content-Disposition"); d != "" {
		w.Header().Set("Content-Disposition", d)
	}
	n, err := io.Copy(w, resp.Body)
	attrs = append(attrs, "bytes", n, "duration_ms", time.Since(start).Milliseconds())
	if err != nil {
		slog.Warn("media", append(attrs, "err", err)...)
		return
	}
	slog.Info("media", attrs...)
}

func tooLarge(size, limit int64) string {
	return fmt.Sprintf("the file is %.1f MiB, over the gateway's %.1f MiB limit for media", float64(size)/(1<<20), float64(limit)/(1<<20))
}
