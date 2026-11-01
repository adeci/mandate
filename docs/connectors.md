# Writing a connector

A connector is a program that holds one login and serves the connector
protocol on a Unix socket. The gateway and console learn what it can do from
its spec.

| Route | |
|---|---|
| `GET /spec` | name, protocol version, operations and their parameters |
| `GET /health` | `ok`, `setup` (needs the owner), `resting`, or `down` |
| `POST /setup` | linking driven from the console, e.g. a QR code |
| `POST /read` | read operations, run without approval |
| `POST /prepare` | normalize a write and render what the owner will see |
| `POST /execute` | run a write the owner approved |
| `GET /forward/…` | relay agents' GETs to an API, for a `Forwarder` |
| `GET`, `PUT /keys` | the owner's trusted keys, kept in sync by the console |

`connector.Main` serves all of it. `/execute` checks the owner's passkey
signature over the exact record against the connector's own key list, re-runs
`Prepare` and refuses if anything changed, spends the nonce, and only then
calls your `Execute`.

## A webhook connector

```go
package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

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
	url   string
	token []byte
}

func (w *webhook) Spec() connector.Spec {
	return connector.Spec{Name: "webhook", Ops: []connector.Op{{
		Name: "post", Kind: connector.Write, Summary: "Post a message to the team channel",
		Params: []connector.Param{{Name: "text", Required: true, Editable: true}},
	}}}
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.url, strings.NewReader(rec.Args["text"]))
	if err != nil {
		return connector.Result{Status: connector.Failed, Detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+string(w.token))
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
		token, err := env.Credential("token")
		if err != nil {
			return nil, err
		}
		return &webhook{url: cfg.URL, token: token}, nil
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
- **Health and linking** are optional: `connector.Checker` and
  `connector.Setupper`, with linking inputs declared in `Spec.Setup`.
- **Credentials** come only from `env.Credential`; settings only from the
  typed config, which `Main` decodes strictly and the build checks with
  `--check-config`.
- **Tests**: `approval/approvaltest` is a software passkey, so you can test
  approved writes end to end through `connector.Server`.

## Running it

```nix
services.mandate.connectors.webhook = {
  package = inputs.mandate-webhook.packages.${system}.default; # meta.mainProgram must be set
  settings.url = "https://hooks.example.com/team";
  credentials.token = "/run/secrets/webhook-token";
};
```

It runs as its own user (`mandate-webhook`) with only that token, and shows up
in `mandate --help` and the console on the next deploy.

The module starts it as

```
mandate-webhook --socket PATH --state DIR --config FILE --rp-id HOST --origin URL
```

with credentials as files in `$CREDENTIALS_DIRECTORY`, and checks the
settings at build time with `mandate-webhook --check-config FILE`.
`connector.Main` handles both. Secrets belong in `DIR/private`; the gateway
can read the rest of `DIR`.
