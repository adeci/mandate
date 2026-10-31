package approvals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/adeci/mandate/internal/approval"
)

// Ntfy publishes push notifications. They carry no authority and, since ntfy
// servers keep them, as little content as possible.
type Ntfy struct {
	URL   string
	Topic string
	// Token is sent as a Bearer token when set.
	Token  string
	Client *http.Client
}

type ntfyMessage struct {
	Topic   string `json:"topic"`
	Title   string `json:"title"`
	Message string `json:"message"`
	Click   string `json:"click,omitempty"`
}

func (n *Ntfy) publish(ctx context.Context, title, message, click string) error {
	body, err := json.Marshal(ntfyMessage{Topic: n.Topic, Title: title, Message: message, Click: click})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// JSON publishing goes to the server root, not the topic URL.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(n.URL, "/")+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if n.Token != "" {
		req.Header.Set("Authorization", "Bearer "+n.Token)
	}
	client := n.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ntfy: %s", resp.Status)
	}
	return nil
}

// summary is the push text for a proposal: the first field not tied to a
// param, e.g. "to Farid". Param fields are the editable ones, i.e. message
// bodies, which must not leave the box.
func summary(display []approval.Field) string {
	for _, f := range display {
		if f.Param != "" {
			continue
		}
		v := strings.Join(strings.Fields(f.Value), " ")
		if utf8.RuneCountInString(v) > 60 {
			v = string([]rune(v)[:60]) + "…"
		}
		return strings.ToLower(f.Label) + " " + v
	}
	return ""
}
