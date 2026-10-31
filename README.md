# mandate

Let agents use your services semi-sanely.

Your agents can read your messages. They can't send one without your thumb.

mandate runs on a NixOS box you own and keeps the logins agents shouldn't
have: your WhatsApp session, a Discord token, API tokens. Agents get
read-only SQL over your chats and an allowlisted window onto your APIs. Chats
you hide don't exist for them, and messages carrying login codes come back
with the text removed.

Anything that writes (a message, a POST) is a proposal. The agent sends you a
link, you open it in the console, read exactly what will be sent, fix the typo
if you like, and approve it with a passkey. The service holding the session
checks your signature itself and sends at most once.

What it won't do: stop an agent from repeating what it read (hide what it
shouldn't read), or protect your accounts from root on the box.

## Setup

```nix
imports = [ inputs.mandate.nixosModules.default ];

services.mandate = {
  enable = true;
  tokenFile = "/run/secrets/mandate-token"; # openssl rand -hex 32
  whatsapp.enable = true;

  console = {
    enable = true;
    hostname = "mandate.example.com"; # the passkeys' identity; don't change it
    tlsCertFile = "/var/lib/acme/mandate.example.com/fullchain.pem";
    tlsKeyFile = "/var/lib/acme/mandate.example.com/key.pem";
    approvers = lib.importJSON ./approvers.json;
  };

  http.services.github = {
    baseUrl = "https://api.github.com";
    tokenFile = "/run/secrets/github-token";
    reads = [ "GET /repos/*/*/issues" ];
    writes = [ "POST /repos/*/*/issues/*/comments" ];
  };
};
```

The gateway (8780) and the console (443) listen on `tailscale0` only. Give
agents a tailnet ACL for 8780 and nothing else. The console reads its
certificate at start, so restart `mandate-console` when it renews.

Your first passkey comes from a one-time link:

```sh
sudo mandate passkey add --name yubikey
```

The console shows the key's JSON; add it to `approvers.json` and redeploy.
Keys are only ever trusted from your config, so nothing over the network can
add one. Then sign in, link WhatsApp under Connections, and add more passkeys
from the console the same way.

To hide a chat or person, find its ID and add it to
`/var/lib/mandate-gateway/exposure.toml`:

```sh
sudo mandate list /var/lib/mandate-whatsapp/data.db
```

```toml
[whatsapp]
hide_containers = ["15551234567@s.whatsapp.net"]
```

## Agents

```sh
export MANDATE_URL=http://box:8780 MANDATE_TOKEN_FILE=~/.config/mandate/token
mandate guide
```

`mandate guide` prints everything an agent needs, generated from what's
actually configured: the views, a few SQL recipes, the connectors and their
health, the API rules, and the approval flow. Paste it into a system prompt.

```sh
mandate query "SELECT sent_at, sender_name, body FROM whatsapp_messages
  WHERE container_name LIKE ?1 ORDER BY sent_at DESC LIMIT 20" %Farid%
mandate http GET github /repos/adeci/mandate/issues
mandate propose whatsapp send to=Farid text="running late" --wait 15m
```

Exit codes: 0 done, 75 not yet (pending, or Discord resting), 1 no
(rejected, failed, or `unknown`, which may have gone out and is never
retried), 2 usage.

## Connectors

Everything that holds a credential is a connector: its own program, serving
the protocol in package `connector` on a Unix socket. WhatsApp, Discord and
the HTTP API proxy live in `connectors/`, each its own Go module; yours can
live anywhere. A connector is a `connector.Main` call:

```go
func main() {
	connector.Main(func(ctx context.Context, env *connector.Env, cfg Config) (connector.Connector, error) {
		token, err := env.Credential("token")
		...
	})
}
```

```
mandate-NAME --socket PATH --state DIR [--config FILE]
             [--approvers FILE --rp-id ID --origin URL]
mandate-NAME --check-config FILE
```

`--config` is JSON decoded strictly into `Config`, then checked by its
`Validate` method if it has one; `--check-config` does only that. Credentials
are files in `$CREDENTIALS_DIRECTORY`. Secrets belong in `DIR/private`; a
connector that stores messages writes them to `DIR/data.db` with package
`store`, and the gateway serves that as its views. Without approvers every
write is refused.

## Caveats

Reading Discord with a user token is against Discord's terms. mandate reads
slowly and politely; the ban risk is yours. WhatsApp unlinks if your phone is
offline for about two weeks, and turns away old clients, so keep the flake
input fresh. Either way the console tells you, and so do your agents.
