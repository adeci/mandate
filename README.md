# sieve

Let AI agents read your WhatsApp and Discord without giving them anything
that can send messages as you.

sieve runs on a machine you own. It keeps the WhatsApp session and Discord
token to itself and gives agents a read-only SQL API over your messages:

- agents can query, but never write or send
- chats and people you hide are invisible to them
- messages with login codes or magic links come back with the text removed

It's a NixOS module. Use it at your own risk: reading Discord with a user
token is against Discord's terms.

## Setup

```nix
{
  inputs.sieve.url = "github:adeci/sieve";

  # in your NixOS configuration
  imports = [ inputs.sieve.nixosModules.default ];

  services.sieve = {
    enable = true;
    tokenFile = "/run/secrets/sieve-token"; # random, 32+ characters
    whatsapp.enable = true;
    discord.enable = true;
    discord.tokenFile = "/run/secrets/discord-token";
  };
}
```

The API listens on port 8780 and is only reachable over Tailscale.

Pair WhatsApp once (scan the QR code under Linked devices on your phone),
then start it:

```sh
sudo -u sieve-whatsapp sieve whatsapp pair --session /var/lib/sieve-whatsapp/private/session.db
sudo systemctl start sieve-whatsapp
```

## Hiding things

List chat and sender IDs:

```sh
sudo sieve list /var/lib/sieve-whatsapp/data.db
sudo sieve list --senders /var/lib/sieve-whatsapp/data.db
```

Put the ones you want hidden in `/var/lib/sieve-gateway/exposure.toml`. Changes
apply immediately. A Discord server ID hides everything in it.

```toml
[whatsapp]
hide_containers = ["123456789@s.whatsapp.net"]
hide_senders = []

[discord]
hide_containers = []
hide_senders = []
```

## Using it

```sh
curl -H "Authorization: Bearer $TOKEN" http://host:8780/v1/schema

curl -H "Authorization: Bearer $TOKEN" http://host:8780/v1/query -d '{
  "sql": "SELECT sent_at, sender_name, body FROM whatsapp_messages WHERE container_name LIKE ?1 ORDER BY seq DESC LIMIT 20",
  "params": ["%Farid%"]
}'
```

Discord is read on demand, slowly, like a person would:

```sh
curl ... /v1/discord/sync   -d '{}'                          # list servers and DMs
curl ... /v1/discord/sync   -d '{"guild_id": "..."}'         # list a server's channels
curl ... /v1/discord/search -d '{"guild_id": "...", "content": "mounting"}'
curl ... /v1/discord/fetch  -d '{"channel_id": "...", "limit": 50}'
```

Requests are spaced 5–12 seconds apart with breaks, capped at 150 a day, and
only made between 8am and midnight. If an agent asks too fast it gets a 429
with `retry_after_seconds`. If Discord rejects the token or asks for a captcha
or verification, sieve stops until you fix it.

## Google Messages

There is a Google Messages (SMS/RCS) source, but Google removed QR pairing, so
it can't be paired yet.

## Development

```sh
nix develop
go test ./...
nix flake check
```
