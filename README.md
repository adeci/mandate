# mandate

Let agents read your services semi-sanely.

mandate holds your WhatsApp session and Discord token on a box you own and gives
agents read-only SQL over your messages. They can look, not send. Chats you
hide don't exist to them, and anything carrying a login code comes back with
the text removed.

Reading Discord with a user token is against Discord's terms. mandate reads
slowly and politely, but the risk is yours.

## Setup

```nix
imports = [ inputs.mandate.nixosModules.default ];

services.mandate = {
  enable = true;
  tokenFile = "/run/secrets/mandate-token";
  whatsapp.enable = true;
  discord.enable = true;
  discord.tokenFile = "/run/secrets/discord-token";
};
```

Link WhatsApp, then start it:

```sh
sudo -u mandate-whatsapp mandate whatsapp pair --session /var/lib/mandate-whatsapp/private/session.db
sudo systemctl start mandate-whatsapp
```

To hide a chat or person, find its ID with `sudo mandate list <data.db>` and add
it to `/var/lib/mandate-gateway/exposure.toml`:

```toml
[whatsapp]
hide_containers = ["15551234567@s.whatsapp.net"]
```

## Use

```sh
export MANDATE_URL=http://host:8780 MANDATE_TOKEN=...

mandate schema
mandate query "SELECT sender_name, body FROM whatsapp_messages WHERE container_name = ?1 ORDER BY seq DESC LIMIT 20" Farid

mandate discord sync                       # your servers and DMs
mandate discord search <guild> "mounting brackets"
mandate discord fetch <channel> --limit 50
```

Output is a table in a terminal and JSON when piped. Discord is fetched on
demand, a few seconds apart, at most 150 requests a day. When it's resting,
commands exit 75 and say when to try again.
