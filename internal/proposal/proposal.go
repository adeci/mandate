// Package proposal is the API between the gateway and the console. The
// gateway submits proposals and reads their status; only the console stores
// them, shows them to the owner, and hands approved records to connectors.
//
//	POST /proposals        Request -> Proposal
//	GET  /proposals/{id}   -> Proposal
package proposal

import (
	"context"
	"net/http"
	"time"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

type Status string

const (
	Pending   Status = "pending"
	Executing Status = "executing"
	Done      Status = "done"
	Failed    Status = "failed"
	// Unknown: the write may have reached the service; it is never retried.
	Unknown  Status = "unknown"
	Rejected Status = "rejected"
	Expired  Status = "expired"
)

type Request struct {
	Connector string            `json:"connector"`
	Op        string            `json:"op"`
	Args      map[string]string `json:"args"`
	// Note is the agent's explanation, shown to the owner as untrusted text.
	Note string `json:"note,omitempty"`
}

type Proposal struct {
	ID        string            `json:"id"`
	Connector string            `json:"connector"`
	Op        string            `json:"op"`
	Display   []approval.Field  `json:"display"`
	Note      string            `json:"note,omitempty"`
	Status    Status            `json:"status"`
	Result    *connector.Result `json:"result,omitempty"`
	// URL is the proposal's page in the console; agents send it to the owner.
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

type Client struct {
	c *connector.Client
}

// Dial reuses the connector transport; errors are *connector.Error.
func Dial(socket string) *Client {
	return &Client{c: connector.Dial(socket, 30*time.Second)}
}

func (c *Client) Submit(ctx context.Context, r Request) (Proposal, error) {
	var p Proposal
	err := c.c.Do(ctx, http.MethodPost, "/proposals", r, &p)
	return p, err
}

func (c *Client) Get(ctx context.Context, id string) (Proposal, error) {
	var p Proposal
	err := c.c.Do(ctx, http.MethodGet, "/proposals/"+id, nil, &p)
	return p, err
}
