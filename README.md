# mandate

mandate lets AI agents read your accounts and propose writes, which wait for
your passkey. It runs on a NixOS machine you own: agents reach a gateway over
your tailnet, and you approve writes in a web console.

## Setup

```nix
imports = [ inputs.mandate.nixosModules.default ];

services.mandate = {
  enable = true;
  hostname = "mandate.example.com";
  useACMEHost = "mandate.example.com"; # or tlsCertFile and tlsKeyFile
  connectors.whatsapp = { };
};
```

The gateway (8780) and the console (443) are open on `tailscale0` only. On
first start the module generates the agent token and the console's admin
password; read them with `sudo cat /var/lib/mandate/{token,admin-password}`,
or set `tokenFile` and `adminPasswordHashFile`.

Sign in to the console with the admin password and add a passkey under Keys.
The first one is trusted right away; adding or removing keys after that needs
a trusted key. Then link your accounts under Connections. Lost every key?
`sudo mandate-reset-keys` lets the password add a first key again.

Approving or signing in with a passkey unlocks that tab for 10 minutes: more
approvals need no touch, and each connector still checks that a trusted key
opened the window. Lock, reload or close the tab to end it sooner.

To hide a chat from agents, add its ID to
`/var/lib/mandate-gateway/exposure.toml`:

```toml
[whatsapp]
hide_containers = ["15551234567@s.whatsapp.net"]
```

## Agents

```sh
export MANDATE_URL=http://box:8780 MANDATE_TOKEN_FILE=~/.config/mandate/token
mandate --help
mandate whatsapp send 15551234567@s.whatsapp.net "running 10 late" --wait 10m
```

## Connectors

Each account is held by a connector, a program running as its own user with
only its own credentials. `whatsapp`, `signal`, `matrix`, `discord` and `http`
(any token API behind an allowlist) are included. Any other program that
speaks the [connector interface](docs/connectors.md) works too: point
`services.mandate.connectors.<name>.package` at it.
