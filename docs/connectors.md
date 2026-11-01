# Writing a connector

A connector is a program that holds one login and serves the connector
protocol on a Unix socket. The gateway and console learn what it can do from
its spec.

| Route | |
|---|---|
| `GET /spec` | name, protocol version, operations and their parameters |
| `GET /health` | `ok`, `setup` (needs the owner), `resting`, or `down` |
| `POST /setup` | linking driven from the console, e.g. a QR code or a pasted token |
| `POST /unlink` | forget the link, for an `Unlinker` |
| `POST /read` | read operations, run without approval |
| `POST /prepare` | normalize a write and render what the owner will see |
| `POST /execute` | run a write the owner approved |
| `GET /forward/…` | relay agents' GETs to an API, for a `Forwarder` |
| `GET`, `PUT /keys` | the owner's trusted keys, kept in sync by the console |

`connector.Main` serves all of it. `/execute` checks the owner's passkey
signature over the exact record against the connector's own key list (or,
in an unlock window, the passkey's delegation to a session key and that key's
signature over the record), re-runs `Prepare` and refuses if anything
changed, spends the nonce, and only then calls your `Execute`.

## A webhook connector

```go
package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"

	"github.com/adeci/mandate/approval"
	"github.com/adeci/mandate/connector"
)

type config struct {
	URL string `json:"url"`
}

func (c config) Validate() error {
	if !strings.HasPrefix(c.URL, "https://") {
		return errors.New("url must be https")
	}
	return nil
}

type webhook struct {
	url    string
	secret connector.Secret
	mu     sync.Mutex
	token  []byte // nil until linked
}

func (w *webhook) Spec() connector.Spec {
	return connector.Spec{
		Name:  "webhook",
		Setup: []connector.Param{{Name: "token", Doc: "the hook's bearer token", Required: true, Secret: true}},
		Ops: []connector.Op{{
			Name: "post", Kind: connector.Write, Summary: "Post a message to the team channel",
			Params: []connector.Param{{Name: "text", Required: true, Editable: true}},
		}},
	}
}

func (w *webhook) current() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.token
}

func (w *webhook) Health(context.Context) connector.Health {
	if w.current() == nil {
		return connector.Health{State: connector.NeedsSetup, Detail: "paste the token in the console"}
	}
	return connector.Health{State: connector.Healthy}
}

func (w *webhook) Setup(_ context.Context, args map[string]string) (connector.SetupStep, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.token != nil {
		return connector.SetupStep{}, &connector.Error{Status: http.StatusConflict, Message: "already linked"}
	}
	token := strings.TrimSpace(args["token"])
	if token == "" {
		return connector.SetupStep{}, connector.Invalid("token is required")
	}
	if err := w.secret.Store([]byte(token)); err != nil {
		return connector.SetupStep{}, err
	}
	w.token = []byte(token)
	return connector.SetupStep{Done: true}, nil
}

func (w *webhook) Unlink(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.token = nil
	return w.secret.Delete()
}

func (w *webhook) Prepare(_ context.Context, _ string, args map[string]string) (connector.Prepared, error) {
	text := args["text"]
	if strings.TrimSpace(text) == "" {
		return connector.Prepared{}, connector.Invalid("text is required")
	}
	return connector.Prepared{
		Args:    map[string]string{"text": text},
		Display: []approval.Field{{Label: "Text", Value: text, Param: "text"}},
	}, nil
}

func (w *webhook) Execute(ctx context.Context, rec approval.Record) connector.Result {
	token := w.current()
	if token == nil {
		return connector.Result{Status: connector.Failed, Detail: "not linked"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, strings.NewReader(rec.Args["text"]))
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return connector.Result{Status: connector.Unknown, Detail: err.Error()}
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return connector.Result{Status: connector.Failed, Detail: resp.Status}
	}
	return connector.Result{Status: connector.Done}
}

func main() {
	connector.Main(func(_ context.Context, env *connector.Env, cfg config) (connector.Connector, error) {
		secret := env.Secret("token")
		token, err := secret.Load()
		if err != nil {
			return nil, err
		}
		return &webhook{url: cfg.URL, secret: secret, token: token}, nil
	})
}
```

## Rules

- **Render `Display` from `Args`**, not from anything a third party chose
  (like a sender's display name). The owner approves what `Display` says;
  `Args` is what runs.
- **`Prepare` must be idempotent**: preparing its own output gives the same
  args and display, which is how the connector detects tampering.
- **Don't retry in `Execute`.** If the request may have reached the service,
  return `Unknown`; the owner decides what happens next.
- **Name**: `Spec().Name` must match the name you give the connector in Nix.
- **Reading** is optional. Implement `connector.Reader` for read ops. Write
  messages to `env.DataPath()` with the `store` package and agents get
  `chats`, `messages` and `search` over them, with the owner's hide rules and
  login-code redaction applied.
- **Chats**: mark a param `Container` and the gateway refuses the op unless
  it names a stored, visible chat.
- **Health and linking** are optional: `connector.Checker`,
  `connector.Setupper` with its inputs declared in `Spec.Setup`, and
  `connector.Unlinker`, which lets the owner unlink and relink. The console
  calls `Setup` every 2s until it is done; a step can show a QR code or a
  code, and `Confirm` asks a yes-or-no question, such as whether the code
  matches another device. The answer comes as the `confirm` arg alone.
- **Secrets** such as API tokens come from the owner through `Setup`: mark
  the input `Secret` and keep it with `env.Secret`. Settings come only from
  the typed config, which `Main` decodes strictly and the build checks with
  `--check-config`.
- **Tests**: `approval/approvaltest` is a software passkey, so you can test
  approved writes end to end through `connector.Server`.

## Running it

```nix
services.mandate.connectors.webhook = {
  package = inputs.mandate-webhook.packages.${system}.default; # meta.mainProgram must be set
  settings.url = "https://hooks.example.com/team";
};
```

It runs as its own user (`mandate-webhook`), shows up in `mandate --help` and
the console on the next deploy, and asks for its token on the Connections page.

The module starts it as

```
mandate-webhook --socket PATH --state DIR --config FILE --rp-id HOST --origin URL
```

and checks the settings at build time with `mandate-webhook --check-config
FILE`. `connector.Main` handles both. Secrets belong in `DIR/private`; the
gateway can read the rest of `DIR`.
