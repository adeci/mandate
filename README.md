# mandate

Let agents use your services semi-sanely.

mandate sits between your AI agents and your accounts, on a NixOS box you own.
Each account is held by a **connector**: a small program that keeps that one
login. Agents read what connectors store and call the operations they offer,
without seeing a credential. Chats you hide are left out, and messages
carrying login codes come back with the text removed.

Writes are proposals. The agent tells you one is waiting, you open the
**console**, check what will happen, edit it if you like, and approve it with
a passkey. The connector checks your signature itself and acts at most once.

mandate can't stop an agent from repeating what it read, so hide what it
shouldn't read. Root on the box can reach your accounts.

## Pieces

- **gateway**: what agents talk to, over your tailnet. Holds no credentials.
- **console**: a web app where you link connectors, manage keys and approve
  or deny writes.
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
    hostname = "mandate.example.com"; # passkeys are bound to it
    tlsCertFile = "/var/lib/acme/mandate.example.com/fullchain.pem";
    tlsKeyFile = "/var/lib/acme/mandate.example.com/key.pem";
    adminPasswordHashFile = "/run/secrets/mandate-admin-password-hash"; # mkpasswd -m bcrypt
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
the system builds.

Sign in to the console with the admin password and add a passkey under Keys;
the first one is trusted right away. Every key after that, and every removal,
waits until one of your trusted keys approves it, from any device. Approving
writes always takes a key. Then link your accounts under Connections.

Lost every key? `sudo mandate-reset-keys` clears the key lists so the
password can add a first key again.

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
mandate --help
mandate whatsapp chats --search farid
mandate whatsapp messages 15551234567@s.whatsapp.net
mandate whatsapp send 15551234567@s.whatsapp.net "running 10 late" --wait 10m
```

Help is generated from what's running. Output is TSV when piped, a table on a
terminal, JSON with `--json`. Chats are addressed by exact ID; errors say
which command to run next.

Exit codes: 0 done, 75 not yet (pending, or a connector resting), 1 no
(rejected, failed, or `unknown`: it may have gone out and won't be retried),
2 usage.

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
mandate-NAME --socket PATH --state DIR [--config FILE] [--rp-id ID --origin URL]
mandate-NAME --check-config FILE
```

`--config` is JSON decoded strictly into `Config`, then checked by its
`Validate` method if it has one; `--check-config` does only that. Credentials
are files in `$CREDENTIALS_DIRECTORY`. Secrets belong in `DIR/private`,
including the connector's own copy of your keys; a connector that stores
messages writes them to `DIR/data.db` with package `store`.

## Caveats

The `discord` connector reads with a user token, which is against Discord's
terms; it reads slowly and politely, and the ban risk is yours. WhatsApp
unlinks a device whose phone is offline for about two weeks and turns away
old clients, so keep the flake input fresh. The console and your agents tell
you when a connector needs you.
