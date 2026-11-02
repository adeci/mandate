package console

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// notifier posts to an ntfy topic when something waits for the owner. The
// text names the agent and connector only: the topic's server is not
// trusted with message text or chat names.
type notifier struct {
	url, token string
	client     *http.Client
	log        *slog.Logger
}

// notify sends in the background under title; high marks what is waited on
// now, such as an agent asking to read. A lost notification is only logged.
func (n *notifier) notify(title, text, click string, high bool) {
	if n == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, strings.NewReader(text))
		if err != nil {
			n.log.Error("notify", "err", err)
			return
		}
		req.Header.Set("Title", title)
		req.Header.Set("Click", click)
		if high {
			req.Header.Set("Priority", "high")
		}
		if n.token != "" {
			req.Header.Set("Authorization", "Bearer "+n.token)
		}
		resp, err := n.client.Do(req)
		if err != nil {
			n.log.Warn("notify", "err", err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode/100 != 2 {
			n.log.Warn("notify", "status", resp.Status)
		}
	}()
}
