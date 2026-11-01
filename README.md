# mandate

mandate lets AI agents read your accounts and propose writes. A write waits
until you approve it with your passkey.

It runs on a NixOS machine you own. Agents reach a gateway over your tailnet,
you approve in a web console, and each account lives in its own connector,
which checks your passkey's signature before it acts.

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

After deploying:

1. Sign in to the console with `sudo cat /var/lib/mandate/admin-password`
   and add a passkey under Keys.
2. Link your accounts under Connections.
3. Give agents the token in `/var/lib/mandate/token`.

The gateway (port 8780) and the console (443) listen on all addresses, and
the firewall opens them on `tailscale0` only (`openFirewallOn`).
Set `tokenFile` and `adminPasswordHashFile` to manage those secrets yourself.

## Agents

```sh
export MANDATE_URL=http://box:8780 MANDATE_TOKEN_FILE=~/.config/mandate/token
mandate --help
mandate whatsapp send 15551234567@s.whatsapp.net "running 10 late" --wait 10m
```

## Approving

Writes wait in the console's inbox. Approving with a passkey unlocks the tab
for 10 minutes, so further approvals need no touch; lock or close the tab to
end it sooner. Adding or removing passkeys needs a trusted one; an added
passkey then waits until you run the `sudo mandate-confirm-key` command the
console shows. Once a passkey is trusted, the admin password no longer signs
in. Lost them all? `sudo mandate-reset-keys` lets the admin password sign in
and add a first key again.

To hide a chat from agents, list it in
`/var/lib/mandate-gateway/exposure.toml`:

```toml
[whatsapp]
hide_containers = ["15551234567@s.whatsapp.net"]
```

## Connectors

`whatsapp`, `signal`, `matrix`, `discord`, `gmessages` (Google Messages, for
SMS and RCS), `fastmail` and `http` (any token API behind an allowlist) are
included. Each runs as its own user with only its own login. To write one,
see [docs/connectors.md](docs/connectors.md).

## License

[AGPL-3.0](LICENSE). The WhatsApp, Signal, Discord and Google Messages
connectors are unofficial clients: using them may break those services' terms
and get an account limited or banned. mandate comes with no warranty, and its
authors are not liable for what happens to your accounts (sections 15 and 16
of the license).
