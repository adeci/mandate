// Package proposal is the API between the gateway and the console. The
// gateway submits proposals and access requests and reads their status, and
// fetches the key log and the signed access records it enforces; only the
// console stores them and shows them to the owner.
//
//	POST /proposals              Request -> Proposal
//	GET  /proposals/{id}         ?agent=ID -> Proposal
//	GET  /keys                   -> []approval.SignedChange
//	GET  /access                 -> {"records": []approval.Signed}
//	GET  /access/quota           ?agent=ID -> 204, or 429 over the agent's limits
//	POST /access/requests        AccessRequest -> RequestRef
//	GET  /access/requests/{id}   ?wait=SECONDS -> AccessStatus
//	POST /access/reads           ReadEvent -> 204
package proposal

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
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
	Note      string `json:"note,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
	AgentName string `json:"agent_name,omitempty"`
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

// Chat is a container an access request names, as the gateway resolved it.
type Chat struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// AccessRequest asks the owner to let an agent read Chats (all when Terms
// is empty) of Connector for For, back to Horizon (all history when 0).
type AccessRequest struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	Connector string `json:"connector"`
	// Terms are the agent's chat terms as given, trimmed; Chats are the
	// chats they matched and Unmatched the terms that matched none. They go
	// to the owner, so the agent's answer doesn't depend on them.
	Terms     []string      `json:"terms,omitempty"`
	Chats     []Chat        `json:"chats,omitempty"`
	Unmatched []string      `json:"unmatched,omitempty"`
	For       time.Duration `json:"for"`
	Horizon   time.Duration `json:"horizon"`
	// Reason is the agent's, shown to the owner as untrusted text.
	Reason string `json:"reason"`
}

type RequestRef struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// States of an access request.
const (
	AccessPending = "pending"
	AccessGranted = "granted"
	AccessDenied  = "denied"
	AccessExpired = "expired"
)

type AccessStatus struct {
	Status  string `json:"status"`
	AgentID string `json:"agent_id"`
	// GrantID and Until are set once granted.
	GrantID string    `json:"grant_id,omitempty"`
	Until   time.Time `json:"until,omitzero"`
}

// ReadEvent is one read the gateway served.
type ReadEvent struct {
	AgentID   string    `json:"agent_id"`
	Connector string    `json:"connector"`
	Verb      string    `json:"verb"`
	Chat      string    `json:"chat,omitempty"`
	Grants    []string  `json:"grants"`
	Count     int       `json:"count"`
	At        time.Time `json:"at"`
	// Dropped counts this agent's earlier events the gateway couldn't queue.
	Dropped int `json:"dropped,omitempty"`
}

type accessRecords struct {
	Records []approval.Signed `json:"records"`
}

// callTimeout bounds every call but a long poll, which adds its wait.
const callTimeout = 30 * time.Second

type Client struct {
	c *connector.Client
}

// Dial reuses the connector transport; errors are *connector.Error.
func Dial(socket string) *Client {
	return &Client{c: connector.Dial(socket, 0)}
}

func (c *Client) do(ctx context.Context, timeout time.Duration, method, path string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return c.c.Do(ctx, method, path, body, out)
}

func (c *Client) Submit(ctx context.Context, r Request) (Proposal, error) {
	var p Proposal
	err := c.do(ctx, callTimeout, http.MethodPost, "/proposals", r, &p)
	return p, err
}

// Get returns proposal id if agent made it; the console answers 404
// otherwise.
func (c *Client) Get(ctx context.Context, id, agent string) (Proposal, error) {
	var p Proposal
	path := "/proposals/" + url.PathEscape(id) + "?agent=" + url.QueryEscape(agent)
	err := c.do(ctx, callTimeout, http.MethodGet, path, nil, &p)
	return p, err
}

// Keys returns the console's key log, for the caller to verify.
func (c *Client) Keys(ctx context.Context) ([]approval.SignedChange, error) {
	var log []approval.SignedChange
	err := c.do(ctx, callTimeout, http.MethodGet, "/keys", nil, &log)
	return log, err
}

// Access returns every agent and grant not deleted or revoked, unverified.
func (c *Client) Access(ctx context.Context) ([]approval.Signed, error) {
	var out accessRecords
	err := c.do(ctx, callTimeout, http.MethodGet, "/access", nil, &out)
	return out.Records, err
}

func (c *Client) RequestAccess(ctx context.Context, r AccessRequest) (RequestRef, error) {
	var ref RequestRef
	err := c.do(ctx, callTimeout, http.MethodPost, "/access/requests", r, &ref)
	return ref, err
}

// AccessQuota answers nil when agent may make an access request now, and
// a 429 *connector.Error when it is over its limits.
func (c *Client) AccessQuota(ctx context.Context, agent string) error {
	return c.do(ctx, callTimeout, http.MethodGet, "/access/quota?agent="+url.QueryEscape(agent), nil, nil)
}

// AccessStatus answers once the request is decided or wait is over.
func (c *Client) AccessStatus(ctx context.Context, id string, wait time.Duration) (AccessStatus, error) {
	var st AccessStatus
	path := "/access/requests/" + url.PathEscape(id) + "?wait=" + strconv.Itoa(int(wait.Seconds()))
	err := c.do(ctx, wait+callTimeout, http.MethodGet, path, nil, &st)
	return st, err
}

func (c *Client) Read(ctx context.Context, e ReadEvent) error {
	return c.do(ctx, callTimeout, http.MethodPost, "/access/reads", e, nil)
}
