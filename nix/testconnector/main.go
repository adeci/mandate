// Command testconnector is the connector of the NixOS VM test: a service
// that exists only as fixed chats and messages, whose send appends the
// message to private/sent.jsonl, so the test can count what ran. It starts
// linked unless the config says unlinked; after an unlink, setup runs a mock
// dialogue through every kind of step, so the app can be tried against it.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/adeci/mandate/connector"
	"github.com/adeci/mandate/record"
	"github.com/adeci/mandate/store"
)

type config struct {
	Account  string `json:"account"`
	Unlinked bool   `json:"unlinked"`
}

func (c *config) Validate() error {
	if c.Account == "" {
		return errors.New("account is required")
	}
	return nil
}

func main() {
	connector.Main(start)
}

type fake struct {
	account string
	st      *store.Store
	sent    string
	mu      sync.Mutex
	// linked is the fake session; polls counts the polls of the current
	// waiting step, which the mock "other device" answers after a few.
	linked bool
	polls  int
}

func start(ctx context.Context, env *connector.Env, cfg config) (connector.Connector, error) {
	st, err := store.Open(ctx, env.DataPath())
	if err != nil {
		return nil, err
	}
	if err := seed(ctx, st, time.Now()); err != nil {
		st.Close()
		return nil, err
	}
	return &fake{account: cfg.Account, st: st, sent: filepath.Join(env.PrivateDir(), "sent.jsonl"), linked: !cfg.Unlinked}, nil
}

func (f *fake) Close() error { return f.st.Close() }

// seed stores the fixture once: messages from now, one from a month ago
// beyond a week's horizon, and a login code.
func seed(ctx context.Context, st *store.Store, now time.Time) error {
	var n int
	if err := st.DB().QueryRowContext(ctx, "SELECT count(*) FROM containers").Scan(&n); err != nil || n > 0 {
		return err
	}
	sam := store.Participant{ID: "+15550003", Name: "~Sam"}
	chats := []store.Container{
		{ID: "fam", Kind: "group", Name: "Family", IsGroup: true},
		{ID: "club", Kind: "group", Name: "Book club", IsGroup: true},
		{ID: "sam", Kind: "dm", Name: "~Sam", Participants: []store.Participant{sam}},
	}
	if err := st.UpsertContainers(ctx, chats); err != nil {
		return err
	}
	old := now.AddDate(0, -1, 0)
	for _, m := range []store.Message{
		{ID: "fam/1", ContainerID: "fam", SenderID: "+15550001", SenderName: "Mum", SentAt: old, Body: "dinner at 7?"},
		{ID: "fam/2", ContainerID: "fam", SenderID: "+15550001", SenderName: "Mum", SentAt: now.Add(-2 * time.Minute), Body: "fresh family"},
		{ID: "fam/3", ContainerID: "fam", SenderID: "+15550002", SenderName: "Dad", SentAt: now.Add(-time.Minute), Body: "Your login code: 482913"},
		{ID: "club/1", ContainerID: "club", SenderID: "+15550004", SenderName: "Ann", SentAt: now.Add(-time.Minute), Body: "fresh club"},
		{ID: "sam/1", ContainerID: "sam", SenderID: sam.ID, SenderName: sam.Name, SentAt: now.Add(-time.Minute), Body: "fresh secret"},
	} {
		if err := st.UpsertMessage(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

func (f *fake) Spec() connector.Spec {
	return connector.Spec{
		Doc: "A fake service for the account " + f.account + ".",
		Ops: []connector.Op{{
			Name:    "send",
			Kind:    connector.Write,
			Summary: "Send a message",
			Params: []connector.Param{
				{Name: "chat", Doc: "chat ID", Required: true, Container: true},
				{Name: "text", Doc: "message text", Required: true, Editable: true},
			},
		}},
	}
}

func (f *fake) Health(context.Context) connector.Health {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.linked {
		return connector.Health{State: connector.NeedsSetup, Detail: "not linked"}
	}
	return connector.Health{State: connector.Healthy}
}

// Unlink forgets the fake session and keeps the store.
func (f *fake) Unlink(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.linked, f.polls = false, 0
	return nil
}

// Setup walks account (text, secret, number, choice) → qr → code → confirm
// → done. The qr and code steps poll; the fake phone answers the fifth poll.
func (f *fake) Setup(_ context.Context, step string, values map[string]string) (connector.Step, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.linked {
		return connector.Step{}, &connector.Error{Status: http.StatusConflict, Message: "linked"}
	}
	account := connector.Step{
		ID: "account", Title: "Link the test service",
		Text: "The account to link. Any password but \"wrong\" works.",
		Fields: []connector.Field{
			{Name: "account", Label: "Account", Kind: connector.TextField, Required: true},
			{Name: "password", Label: "Password", Kind: connector.SecretField, Required: true},
			{Name: "device", Label: "Device number", Kind: connector.NumberField},
			{Name: "via", Label: "Link with", Kind: connector.ChoiceField, Required: true, Options: []connector.Option{
				{Value: "qr", Label: "QR code"}, {Value: "code", Label: "Pairing code"},
			}},
		},
	}
	qr := connector.Step{
		ID: "qr", Text: "On your phone: Test app › Linked devices › Link a device, then scan this code.",
		QR: "test://link/7RJ4P2W5ZK3HXQ6N5M2C4YB7DE", Poll: 2,
	}
	code := connector.Step{ID: "code", Text: "Your phone shows a code. Check that it is this one.", Code: "4829-1375", Poll: 2}
	confirm := connector.Step{ID: "confirm", Confirm: "Link this device as \"mandate\"? It keeps a copy of your chats."}
	waited := func() bool {
		if f.polls++; f.polls < 5 {
			return false
		}
		f.polls = 0
		return true
	}
	switch step {
	case "":
		f.polls = 0
		return account, nil
	case "account":
		switch {
		case values["account"] == "" || values["password"] == "":
			return connector.Step{}, connector.Invalid("account and password are required")
		case values["password"] == "wrong":
			return connector.Step{}, connector.Invalid("the test service refused that password")
		case values["via"] == "code":
			return code, nil
		case values["via"] == "qr":
			return qr, nil
		}
		return connector.Step{}, connector.Invalid("link with qr or code")
	case "qr":
		if waited() {
			return code, nil
		}
		return qr, nil
	case "code":
		if waited() {
			return confirm, nil
		}
		return code, nil
	case "confirm":
		if values["confirm"] != "yes" {
			return account, nil
		}
		f.linked = true
		return connector.Step{ID: "done", Text: "Linked.", Done: true}, nil
	}
	return connector.Step{}, connector.Invalid("unknown step %q", step)
}

func (f *fake) Prepare(ctx context.Context, op string, args map[string]string) (connector.Prepared, error) {
	if op != "send" {
		return connector.Prepared{}, connector.ErrUnknownOp
	}
	for k := range args {
		if k != "chat" && k != "text" {
			return connector.Prepared{}, connector.Invalid("unknown arg %q", k)
		}
	}
	if args["text"] == "" {
		return connector.Prepared{}, connector.Invalid("text is required")
	}
	var name string
	err := f.st.DB().QueryRowContext(ctx, "SELECT name FROM containers WHERE id = ?", args["chat"]).Scan(&name)
	if err != nil {
		return connector.Prepared{}, connector.Invalid("unknown chat %q", args["chat"])
	}
	return connector.Prepared{
		Args: map[string]string{"chat": args["chat"], "text": args["text"]},
		Display: []record.Field{
			{Label: "To", Value: name},
			{Label: "Text", Value: args["text"], Param: "text"},
		},
		Container: args["chat"],
	}, nil
}

func (f *fake) Execute(_ context.Context, w record.Write) connector.Result {
	line, err := json.Marshal(map[string]string{"id": w.ID, "chat": w.Args["chat"], "text": w.Args["text"]})
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: err.Error()}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	file, err := os.OpenFile(f.sent, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: err.Error()}
	}
	_, err = file.Write(append(line, '\n'))
	if cerr := file.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	}
	return connector.Result{Status: connector.Done, Ref: w.ID}
}
