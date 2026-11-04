# Design

The specs in [spec/](spec/) are mandate's public contracts. This is how the server is put together behind them.

## One binary

`cmd/mandate` holds:
- `mandate serve front|gateway|owner`: the three server processes;
- `mandate setup`, `mandate reset`: root, on the server;
- `mandate store-acl NAME`: root, before connector NAME starts (see systemd below);
- `mandate units --out DIR`: renders the systemd units from mandate.toml;
- `mandate conformance PROGRAM`: connector.md's kit, also run by the flake check;
- the agent CLI (`login`, reads, `request`, proposals, …).

## Processes, groups and paths

Every process runs as a systemd dynamic user (`DynamicUser=yes`), so there are no fixed users. Its primary group is fixed (`Group=`), and that group is how the others recognize it: every socket is mode 0666 and answers each route only to the right group, read with `SO_PEERCRED`. Anyone else gets `403`.

| Process | Group | State | Socket |
|---|---|---|---|
| front | `mandate-front` | `/var/lib/mandate/front` (own TLS key, ACME cache) | listens on the one TCP port |
| gateway | `mandate-gateway` | `/var/lib/mandate/gateway` (live identities and grants, key log copy) | `/run/mandate/gateway/socket` |
| owner | `mandate-owner` | `/var/lib/mandate/owner` (key log, records, device tokens, never-share, requests, proposals, activity) | `/run/mandate/owner/socket` |
| connector NAME | dynamic, its own | `/var/lib/mandate/connectors/NAME` (`data.db`, `private/`) | `/run/mandate/connectors/NAME/socket` |

So a server has three fixed groups and no fixed users.

Each unit has its own `RuntimeDirectory=` (`mandate/gateway`, `mandate/owner`, `mandate/connectors/NAME`), because dynamic users can't share one: systemd chowns it to the unit's user and removes it on stop. With `DynamicUser=`, connector state really lives under `/var/lib/private`, so the gateway unit reaches the stores with `BindReadOnlyPaths=/var/lib/private/mandate/connectors:/var/lib/mandate/connectors`.

| Socket | Route | Caller group |
|---|---|---|
| gateway | `/agent/` | `mandate-front` |
| gateway | `/internal/` | `mandate-owner` |
| owner | `/owner/`, `/setup`, `/setup/` | `mandate-front` |
| owner | `/internal/` | `mandate-gateway` |
| connector | per connector.md | `mandate-gateway`, `mandate-owner` |

- **front** reaches only the gateway and owner sockets. It never touches stores or connectors.
- **gateway** reads each `data.db`. Each connector's unit puts a default ACL on its state directory, `group:mandate-gateway:r-X`. An ACL names a group without making the connector a member, so connectors can't read each other's stores.
- **owner** calls connectors as the owner caller. It never opens `data.db`.
- **gateway ↔ owner:** each serves the other an internal HTTP API on its own socket, under `/internal/`, which front never forwards.
  - gateway → owner: new access requests and proposals, and activity entries.
  - owner → gateway: the live set (identities, grants, key log head) as signed envelopes, which the gateway verifies itself, plus never-share.

## front

- **TLS modes:** `own` (P-256 key and self-signed certificate, made once, in front's state), `acme` (autocert with TLS-ALPN-01, cache in front's state) and `cert` (files, reloaded when they change).
- **Routing:** `/agent/` goes to the gateway socket, `/owner/`, `/setup` and `/setup/` to the owner socket, and everything else is `404`.
- **Agent-only port:** with `agent_listen` set, front also listens there with the same TLS and forwards only `/agent/`, so a network rule that filters by port (a Tailscale grant) can keep agent machines away from the owner side.
- **Headers:** front sets these, overwriting anything the client sent:
  - `X-Forwarded-For`: the client's IP;
  - `Mandate-Spki-Sha256`: the hex SHA-256 of the SPKI of the certificate served on this connection, needed for `server_confirm`.
- **Streaming:** server-sent events stream without buffering, and long polls of up to 900 seconds stay open.

## Config: `/etc/mandate/mandate.toml`

Every process reads it. The NixOS module writes the same file; elsewhere `mandate setup` writes a default unless there is one: the values below, and every connector `install.sh` put in `/usr/local/lib/mandate/connectors`.

```toml
listen = ":8443"
agent_listen = ""       # optional: a second port serving only /agent/

[tls]
mode = "own"            # own | acme | cert
domain = ""             # acme
cert = ""               # cert
key = ""                # cert

[connectors.whatsapp]
command = "/usr/local/lib/mandate/connectors/whatsapp"   # NixOS: the package path
config = {}             # written as JSON for the connector's --config
```

Unknown keys are refused. Paths are fixed (the table above) rather than configurable. The NixOS VM test can override them through `MANDATE_ROOT`, a prefix for `/etc`, `/var/lib` and `/run` used only in tests.

Connector names match `[a-z][a-z0-9-]{0,23}`, so that `mandate-NAME` fits useradd's 32-character limit, and `command` is an absolute path. ACME does not force `listen` to port 443, since the port the CA sees may be forwarded. Front caps request bodies at 1 MiB, as connectors do.

## Internal API (gateway ↔ owner)

JSON over the two sockets, under `/internal/`, and caller-checked by group as in the table above. The owner side owns all state the owner decides on. The gateway owns filtering and relays the agent's waits.

**The owner serves the gateway:**

| Route | Body → answer |
|---|---|
| `POST /internal/requests` | `{identity, connector, kind: once\|routine\|extend, terms: [{term, chats: [{id, name}]}], for, horizon, schedule, reason}` → `{id}`. The gateway has already matched the terms against the visible chats; a term with no chats reaches the owner as "no visible chat matched". |
| `GET /internal/requests/{id}?identity=…&wait=` | `{status, grant_id, until}`. `404` for another identity's request. |
| `POST /internal/proposals` | `{identity, connector, op, prepared: {args, display, container, message}, note}` → `{id}`. The gateway has already called `prepare` and checked the chat and message against the grants. |
| `GET /internal/proposals/{id}?identity=…&wait=` | `{id, status, result}` |
| `POST /internal/activity` | `[{at, identity, connector, verb, chats, count}]`, batched |
| `POST /internal/join`, `GET /internal/join/{id}?wait=`, `POST /internal/join/{id}` | The agent side of the code exchange, relayed as-is from `/agent/v1/join…`. Exchanges live on the owner side, for agents and devices alike, and the gateway passes `X-Forwarded-For` and `Mandate-Spki-Sha256` on. |

**The gateway serves the owner:**

| Route | Body |
|---|---|
| `PUT /internal/live` | `{keys: [envelopes], identities: [envelopes], grants: [envelopes], never_share: [{connector, kind, id}]}`. This is the whole live set, pushed after every change and on start. The gateway extends its own key log copy, verifies every identity and grant against it, and serves only what verifies. What is missing from the set is not live: closed grants, paused routines, deleted identities, and grants that ended more than 24 hours ago. |

**Writes:**
1. The gateway calls `prepare` and checks the chat and message against the grants.
2. The proposal goes to the owner side.
3. The owner may edit it, which runs `prepare` again as the owner caller, then signs it in the app.
4. The owner side calls `execute` with the envelope and keeps the result. The agent's poll reaches it through the gateway.

## systemd

Every mandate unit (front, gateway, owner, and one per connector) is `PartOf=` and `WantedBy=` `mandate.target`. `mandate reset` stops the target, wipes, and starts it again.

- **One definition:** the units, sandbox and all, come from one template in the binary (`internal/owner/systemd.tmpl`), rendered from mandate.toml by `mandate units`. The NixOS module installs them as a package and adds only `wantedBy`, so NixOS options on a mandate unit become drop-ins. Elsewhere `mandate setup` reconciles on every run: it makes the three groups and the default config, writes each connector's `--config` file (`/etc/mandate/connectors/NAME.json`, which the module also writes) and the units, removes those of connectors no longer configured, restarts what changed and starts the target. A unit's `X-Restart-Triggers=` is mandate.toml's hash, so a config change restarts every unit on both.
- **Unit user names:** a dynamic user can't share its name with a static group, so the units run as `User=mandate_front`, `mandate_gateway` and `mandate_owner`, with the fixed `Group=` names. The underscore keeps them from clashing with connector users (`mandate-NAME`).
- **Connector state ACL:**
  - With dynamic users, systemd keeps state under `/var/lib/private`, owned by `nobody` on disk and mapped into each unit's view. A named-group ACL set inside the unit fails, and systemd's recursive chown removes ACLs.
  - So `ExecStartPre=+mandate store-acl NAME` sets the ACL through `/proc/1/root/var/lib/private/mandate/connectors/NAME` on every start, writing the ACL xattrs itself, since not every host has `setfacl`.
  - The gateway reads the stores through `BindReadOnlyPaths=/var/lib/private/mandate/connectors:/var/lib/mandate/connectors`.
  - The VM test checks all of this first.
