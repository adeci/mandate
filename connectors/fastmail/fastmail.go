// Command mandate-fastmail reads a Fastmail account over JMAP with an API
// token the owner pastes in the console, and serves the fastmail connector.
//
// The only emails it sends are writes the owner approved with a passkey,
// verified by connector.Server.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/store"
)

// pollEvery is how often Email/changes asks what is new.
const pollEvery = 30 * time.Second

func main() {
	connector.Main(start)
}

func start(ctx context.Context, env *connector.Env, _ struct{}) (connector.Connector, error) {
	st, err := store.Open(ctx, env.DataPath())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	c, err := newConnector(ctx, st, env.Secret("token"), env.Log, &http.Client{Timeout: time.Minute}, defaultSessionURL, pollEvery)
	if err != nil {
		st.Close()
		return nil, err
	}
	return c, nil
}

type fmConnector struct {
	// ctx outlives requests: the sync loop runs until shutdown.
	ctx        context.Context
	st         *store.Store
	token      connector.Secret
	log        *slog.Logger
	http       *http.Client
	sessionURL string
	poll       time.Duration

	setup sync.Mutex // serializes Setup and Unlink

	mu   sync.Mutex
	acct *account // nil until linked
}

// account is a linked token and its sync loop.
type account struct {
	client
	cancel context.CancelFunc
	done   chan struct{}
	kick   chan struct{} // asks the loop to sync now

	mu      sync.Mutex
	conn    *conn // nil until the session is fetched
	syncing bool  // the first sync is running
	err     error // of the last sync
}

// conn is what the account's session and settings say.
type conn struct {
	api, download, accountID, username string
	roles                              map[string]string // mailbox role → ID
	identities                         []identity
}

func newConnector(ctx context.Context, st *store.Store, token connector.Secret, log *slog.Logger,
	hc *http.Client, sessionURL string, poll time.Duration,
) (*fmConnector, error) {
	if _, err := st.DB().ExecContext(ctx, schema); err != nil {
		return nil, fmt.Errorf("create fastmail tables: %w", err)
	}
	c := &fmConnector{ctx: ctx, st: st, token: token, log: log, http: hc, sessionURL: sessionURL, poll: poll}
	raw, err := token.Load()
	if err != nil {
		return nil, fmt.Errorf("load token: %w", err)
	}
	if raw != nil {
		c.link(&account{client: client{http: hc, token: string(raw)}})
	}
	return c, nil
}

// link makes a the linked account and starts syncing it.
func (c *fmConnector) link(a *account) {
	ctx, cancel := context.WithCancel(c.ctx)
	a.cancel, a.done, a.kick = cancel, make(chan struct{}), make(chan struct{}, 1)
	c.mu.Lock()
	c.acct = a
	c.mu.Unlock()
	go func() {
		defer close(a.done)
		c.run(ctx, a)
	}()
}

func (c *fmConnector) current() *account {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acct
}

func (a *account) stop() {
	a.cancel()
	<-a.done
}

func (a *account) state() (*conn, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.conn, a.syncing, a.err
}

func (a *account) poke() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

var setupParams = []connector.Param{{
	Name: "token", Required: true, Secret: true,
	Doc: "A Fastmail API token. In Fastmail: Settings → Privacy & Security → Connected apps & API tokens → " +
		"Manage API tokens → New API token, type JMAP, with the scopes Email and Email submission and without Read-only access.",
}}

var errScope = connector.Invalid("this token cannot both read and send email: create one with the scopes Email and Email submission, without Read-only access")

func (c *fmConnector) Setup(ctx context.Context, args map[string]string) (connector.SetupStep, error) {
	for k := range args {
		if k != "token" {
			return connector.SetupStep{}, connector.Invalid("unknown arg %q", k)
		}
	}
	token := strings.TrimSpace(args["token"])
	if token == "" {
		return connector.SetupStep{}, connector.Invalid("token is required")
	}
	if !c.setup.TryLock() {
		return connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "linking already in progress"}
	}
	defer c.setup.Unlock()
	if c.current() != nil {
		return connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "already linked"}
	}
	a := &account{client: client{http: c.http, token: token}}
	cn, err := c.connect(ctx, a)
	switch {
	case errors.Is(err, errUnauthorized):
		return connector.SetupStep{}, connector.Invalid("Fastmail refused the token")
	case errors.Is(err, errScope):
		return connector.SetupStep{}, err
	case err != nil:
		return connector.SetupStep{}, &connector.Error{Status: http.StatusBadGateway, Message: "reach Fastmail: " + err.Error()}
	}
	if err := c.token.Store([]byte(token)); err != nil {
		return connector.SetupStep{}, err
	}
	a.conn = cn
	c.link(a)
	c.log.Info("linked", "user", cn.username)
	return connector.SetupStep{Done: true}, nil
}

// Unlink forgets the token. Fastmail keeps it valid until the owner removes
// it in their settings.
func (c *fmConnector) Unlink(context.Context) error {
	c.setup.Lock()
	defer c.setup.Unlock()
	c.mu.Lock()
	a := c.acct
	c.acct = nil
	c.mu.Unlock()
	if a != nil {
		a.stop()
	}
	return c.token.Delete()
}

func (c *fmConnector) Health(context.Context) connector.Health {
	a := c.current()
	if a == nil {
		return connector.Health{State: connector.NeedsSetup, Detail: "not linked"}
	}
	cn, syncing, err := a.state()
	switch {
	case errors.Is(err, errUnauthorized):
		return connector.Health{State: connector.Broken, Detail: "Fastmail refused the API token, which may have been revoked; unlink and link a new one"}
	case err != nil:
		return connector.Health{State: connector.Broken, Detail: err.Error()}
	case cn == nil:
		return connector.Health{State: connector.Healthy, Detail: "connecting to Fastmail"}
	case syncing:
		return connector.Health{State: connector.Healthy, Detail: "first sync running"}
	}
	return connector.Health{State: connector.Healthy}
}

// Close stops syncing once serving has stopped, then closes the store.
func (c *fmConnector) Close() error {
	c.mu.Lock()
	a := c.acct
	c.acct = nil
	c.mu.Unlock()
	if a != nil {
		a.stop()
	}
	return c.st.Close()
}

// connect fetches the session and checks that the token may read and send
// mail, then reads the mailboxes and identities.
func (c *fmConnector) connect(ctx context.Context, a *account) (*conn, error) {
	s, err := a.session(ctx, c.sessionURL)
	if err != nil {
		return nil, err
	}
	mail, sub := s.PrimaryAccounts[capMail], s.PrimaryAccounts[capSubmission]
	if mail == "" || mail != sub || s.Accounts[mail].IsReadOnly {
		return nil, errScope
	}
	return c.refresh(ctx, a, &conn{api: s.APIURL, download: s.DownloadURL, accountID: mail, username: s.Username})
}

// refresh reads cn's mailboxes and identities anew.
func (c *fmConnector) refresh(ctx context.Context, a *account, cn *conn) (*conn, error) {
	resps, err := a.call(ctx, cn.api,
		invocation{"Mailbox/get", map[string]any{"accountId": cn.accountID, "properties": []string{"id", "role"}}, "mb"},
		invocation{"Identity/get", map[string]any{"accountId": cn.accountID}, "id"},
	)
	if err != nil {
		return nil, err
	}
	boxes, err := result[getResponse[mailbox]](resps, "Mailbox/get", "mb")
	if err != nil {
		return nil, err
	}
	ids, err := result[getResponse[identity]](resps, "Identity/get", "id")
	if err != nil {
		return nil, err
	}
	next := *cn
	next.roles = map[string]string{}
	for _, m := range boxes.List {
		if m.Role != "" {
			next.roles[m.Role] = m.ID
		}
	}
	next.identities = ids.List
	return &next, nil
}

// skipped reports whether e is a draft, or only in Trash, Spam or Drafts.
func (cn *conn) skipped(e email) bool {
	if e.Keywords["$draft"] {
		return true
	}
	for id := range e.MailboxIDs {
		if id != cn.roles["trash"] && id != cn.roles["junk"] && id != cn.roles["drafts"] {
			return false
		}
	}
	return true
}

// identity returns the identity that sends as addr: one with that address,
// or else a wildcard one (*@example.com) for its domain.
func (cn *conn) identity(addr string) (identity, bool) {
	var wild identity
	found := false
	for _, id := range cn.identities {
		switch {
		case strings.EqualFold(id.Email, addr):
			return id, true
		case !found && strings.HasPrefix(id.Email, "*@") && strings.HasSuffix(strings.ToLower(addr), strings.ToLower(id.Email[1:])):
			wild, found = id, true
		}
	}
	return wild, found
}

// defaultIdentity is the one for the login address, or else the first.
func (cn *conn) defaultIdentity() (identity, bool) {
	if id, ok := cn.identity(cn.username); ok && !strings.HasPrefix(id.Email, "*@") {
		return id, true
	}
	for _, id := range cn.identities {
		if !strings.HasPrefix(id.Email, "*@") {
			return id, true
		}
	}
	return identity{}, false
}
