# mandate

Let agents use your services semi-sanely.

Your agents can read your messages. They can't send one without your thumb.

mandate sits between your AI agents and your accounts, on a NixOS box you own.
Each account is held by a **connector**: a small program that keeps that one
login and nothing else. Agents get read-only SQL over what connectors store,
plus the operations connectors offer, and never see a credential. Chats you
hide don't exist for them, and messages carrying login codes come back with
the text removed.

Anything that writes is a proposal. The agent sends you a link, you open it in
the **console**, read exactly what will happen, fix the typo if you like, and
approve it with a passkey. The connector checks your signature itself and acts
at most once, so even a compromised gateway can't send anything.

What it won't do: stop an agent from repeating what it read (hide what it
shouldn't read), or protect your accounts from root on the box.

## Pieces

- **gateway**: what agents talk to, over your tailnet. Holds no credentials.
- **console**: a web app where you link connectors and approve or deny
  writes. Passkeys only (a YubiKey, or your phone's).
- **connectors**: one per service, each its own program, user and sandbox.
  Bundled: `whatsapp`, `discord` (reads only), and `http`, which puts any
  token API behind an allowlist. Writing your own is
  [a page of Go](docs/connectors.md).

## Setup

```nix
imports = [ inputs.mandate.nixosModules.default ];

services.mandate = {
  enable = true;
  tokenFile = "/run/secrets/mandate-token"; # openssl rand -hex 32

  console = {
    enable = true;
    hostname = "mandate.example.com"; # the passkeys' identity; don't change it
    tlsCertFile = "/var/lib/acme/mandate.example.com/fullchain.pem";
    tlsKeyFile = "/var/lib/acme/mandate.example.com/key.pem";
    approvers = lib.importJSON ./approvers.json;
  };

  connectors = {
    whatsapp = { };
    http = {
      settings.services.github = {
        base_url = "https://api.github.com";
        reads = [ "GET /repos/*/*/issues" ];
        writes = [ "POST /repos/*/*/issues/*/comments" ];
      };
      credentials.github = "/run/secrets/github-token";
    };
  };
};
```

The gateway (8780) and console (443) listen on `tailscale0` only; give agents
a tailnet ACL for 8780 and nothing else. Connector settings are checked when
the system builds, so a typo fails the build, not the box.

Your first passkey comes from a one-time link:

```sh
sudo mandate passkey add --name yubikey
```

The console shows the key's JSON; add it to `approvers.json` and redeploy.
Keys are only ever trusted from your config, so nothing over the network can
add one. Then sign in, link your accounts under Connections, and add more
passkeys the same way.

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
actually running: the views, a few SQL recipes, each connector's operations
and health, and how approval works. Paste it into a system prompt.

Exit codes: 0 done, 75 not yet (pending, or a connector resting), 1 no
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

The `discord` connector reads with a user token, which is against Discord's
terms; it reads slowly and politely, and the ban risk is yours. WhatsApp
unlinks a device whose phone is offline for about two weeks and turns away
old clients, so keep the flake input fresh. The console and your agents tell
you when a connector needs you.
