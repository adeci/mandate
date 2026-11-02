# mandate

mandate lets AI agents read your accounts and propose writes. Reads need a
grant you sign with your passkey, and a write waits until you approve it.

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
  notify.ntfy.url = "http://ntfy.example.ts.net/mandate"; # optional
};
```

After deploying:

1. Sign in to the console with `sudo cat /var/lib/mandate/admin-password`
   and add a passkey under Keys.
2. Link your accounts under Connections.
3. Create an agent under Access and give it the token your browser shows,
   which is shown only once.

The gateway (port 8780) and the console (443) listen on all addresses, and
the firewall opens them on `tailscale0` only (`openFirewallOn`).
Set `adminPasswordHashFile` to manage the admin password yourself.

## Agents

```sh
export MANDATE_URL=http://box:8780 MANDATE_TOKEN_FILE=~/.config/mandate/token
mandate --help
mandate access                                     # what this agent may read now
mandate request whatsapp --chat Family --for 30m --since 7d --reason "find the dinner plan"
mandate whatsapp send 15551234567@s.whatsapp.net "running 10 late" --wait 10m
mandate whatsapp media 15551234567@s.whatsapp.net 812   # save that message's photo
```

On a machine without Nix, download the CLI from the
[latest release](https://github.com/adeci/mandate/releases/latest)
(`mandate-linux-amd64` or `mandate-linux-arm64`). `MANDATE_TOKEN` works in
place of a token file. Give the agent
[skills/mandate/SKILL.md](skills/mandate/SKILL.md) as a skill or in its
instructions.

## Access

Each agent has its own token, and reads or writes nothing until you grant it.
The Access page lists agents, grants, pending requests and recent reads. A
grant, signed with your passkey, covers one agent and one connector, every
chat or the ones you check, during a window:

- **once**, from now for a duration, or
- **on a schedule**, such as weekdays at 08:00 for 30 minutes, in your
  browser's time zone,

and a horizon: messages older than 24 hours, 7 days or 30 days stay hidden,
or none with all history. Checked chats are exact: a thread or channel
created later is not included. A grant also lets the agent propose writes
to the chats it covers. Grants on `http` cover every allowlisted read, with
no chats or horizon. Revoking a grant or deleting an agent needs no passkey
and takes effect within 15 seconds.

An agent without a grant gets a 403 that tells it to run `mandate request`.
The request shows in the inbox and on the Access page, with the terms that
matched no chat. Check the chats to allow, shorten the time or horizon, then
approve it with your passkey, or deny it. Undecided requests expire after an
hour, and an agent may have 3 waiting at a time. With `notify.ntfy.url` (and
`notify.ntfy.tokenFile` for a protected topic) set, the console posts to
that ntfy topic whenever a write or a request waits for you.

## Approving

Writes wait in the console's inbox. Approving with a passkey unlocks the tab
for 10 minutes, so further approvals need no touch; lock or close the tab to
end it sooner. Agents and grants always take a touch. To add a passkey,
register it under Keys and approve the addition with a trusted one; removing
one needs a trusted one too. Once a passkey is trusted, the admin password no
longer signs in. Lost them all? `sudo mandate-reset-keys` lets the admin
password sign in and add a first key again; agents and grants signed by the
old keys stop working, so create them again.

To hide a chat from agents, whatever they are granted, list it in
`/var/lib/mandate-gateway/exposure.toml`:

```toml
[whatsapp]
hide_containers = ["15551234567@s.whatsapp.net"]
```

Hiding a chat also hides the chats under it, such as a Discord server's
channels and their threads; `hide_senders` hides a person's messages and
DMs. The gateway logs a warning for each listed ID that matches nothing it
has stored.

## Connectors

`whatsapp`, `signal`, `matrix`, `discord`, `gmessages` (Google Messages, for
SMS and RCS), `fastmail` and `http` (any token API behind an allowlist) are
included. Each runs as its own user with only its own login. To write one,
see [docs/connectors.md](docs/connectors.md); for what each part is trusted
with, see [SECURITY.md](SECURITY.md).

## License

[AGPL-3.0](LICENSE). The WhatsApp, Signal, Discord and Google Messages
connectors are unofficial clients: using them may break those services' terms
and get an account limited or banned. mandate comes with no warranty, and its
authors are not liable for what happens to your accounts (sections 15 and 16
of the license).
