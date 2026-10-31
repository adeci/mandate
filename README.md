# mandate

Let agents use your services semi-sanely.

mandate holds your WhatsApp session, Discord token, and API tokens on a box you
own. Agents read your messages through SQL and call APIs through an allowlist;
they never see a credential. Chats you hide don't exist to them, and anything
carrying a login code comes back with the text removed.

Agents can't send anything on their own. They propose, your phone buzzes, you
read the exact message (and fix it if you like), and approve it with a passkey.
The service that sends checks your signature itself and sends at most once.

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

  approvals = {
    enable = true;
    hostname = "box.example.ts.net";
    tlsCertFile = "/var/lib/certs/box.crt";
    tlsKeyFile = "/var/lib/certs/box.key";
    ntfy.url = "https://ntfy.example.com";
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

Read rules run without asking, so keep them to requests that change nothing.

Link WhatsApp, then start it:

```sh
sudo -u mandate-whatsapp mandate whatsapp pair --session /var/lib/mandate-whatsapp/private/session.db
sudo systemctl start mandate-whatsapp
```

Enroll your phone's passkey, then add the JSON it shows to `approvers.json`
and redeploy. Nothing can approve until it's in your config:

```sh
sudo mandate approver enroll --name phone
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

mandate read discord sync
mandate read discord search guild_id=<guild> content="mounting brackets"
mandate http GET github /repos/adeci/mandate/issues

mandate propose whatsapp send to=Farid text="running late" --note "he asked when I'd arrive"
mandate action <id>
```

Output is a table in a terminal and JSON when piped. `mandate action` exits 75
until you've decided. Discord is fetched on demand, a few seconds apart, at most
150 requests a day; when it's resting, commands exit 75 and say when to try
again.
