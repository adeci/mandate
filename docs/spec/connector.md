# Connector protocol

A connector is a program that holds one account (a WhatsApp login, a mail
account). mandate talks to it over HTTP on a Unix socket and reads the
messages it stores from a SQLite file. Any language works; Go connectors get
all of this from `connector.Main`.

Protocol version 1, store schema version 1.

## Process

Each connector runs as its own Unix user, usually a systemd dynamic user,
started by mandate as:

```
NAME --socket PATH --state DIR [--config FILE] --gateway-group GROUP --owner-group GROUP
NAME --check-config FILE
```

- `--config` is JSON; unknown keys are refused. `--check-config` exits 0 for
  a valid file, else non-zero with a message on stderr.
- The socket is mode 0666: who may call what is decided per route, by the
  caller's group (see [Callers](#callers)).
- `DIR/data.db` is the store, in SQLite's rollback-journal mode: a reader
  without write access can't rely on WAL's shared-memory file. mandate gives
  the gateway's group read access to `DIR` through a default ACL, so the
  connector creates the store and its `-journal` group-readable.
  `DIR/private/` (0700) holds the session, credentials, key log and spent
  approvals.
- It exits on SIGTERM after finishing in-flight writes. A fault only a
  restart clears ends the process non-zero; anything the owner must fix is
  reported through [health](#health).

## Callers

Two processes call a connector: the **gateway**, which answers agents and
enforces grants, and the **owner** side, which serves the app and holds
approvals. The connector reads the caller's primary group from the socket
(`SO_PEERCRED`) and answers each route only to its class; anyone else gets
`403`.

| Route | gateway | owner |
|---|---|---|
| `GET /spec`, `GET /health`, `GET /keys`, `POST /prepare` | ✓ | ✓ |
| `POST /read`, `POST /media` | ✓ | |
| `POST /setup`, `POST /unlink`, `POST /execute`, `PUT /keys`, `GET /containers` | | ✓ |

Bodies are JSON, at most 1 MiB; times are RFC 3339 UTC. A refusal is a
non-2xx status with `{"error": "…"}`, written for the agent and naming only
what it sent; `429` adds `retry_after_seconds`.

## Spec

`GET /spec`

```json
{"protocol": 1, "name": "whatsapp", "doc": "…", "media": true, "unlink": true,
 "ops": [
   {"name": "fetch", "kind": "read", "summary": "Fetch a chat's recent messages", "params": [
     {"name": "chat", "doc": "chat ID", "required": true, "container": true}]},
   {"name": "send", "kind": "write", "summary": "Send a message", "params": [
     {"name": "chat", "doc": "chat ID", "required": true, "container": true},
     {"name": "text", "doc": "message text", "required": true, "editable": true},
     {"name": "seq", "doc": "message to reply to", "message": true}]}]}
```

- `name` equals the name mandate configured. mandate refuses another
  `protocol`.
- `kind`: `read` runs inside an agent's grant; `write` is a proposal.
- `container`: a chat ID. `message`: a message `seq` in the op's one
  `container` chat. The gateway checks both against the agent's grants before
  calling the connector. `editable`: the owner may change it before approving.
- `media`: serves `POST /media`. `unlink`: serves `POST /unlink`.

## Health

`GET /health` → `{"state": "resting", "detail": "daily budget used", "until": "2026-11-03T06:00:00Z"}`

| `state` | |
|---|---|
| `ok` | working |
| `connecting` | linked, reconnecting by itself |
| `resting` | reads refused until `until` |
| `setup` | the owner must link or relink |
| `down` | broken |

`detail` is shown to the owner and to agents: no credentials, no message
content.

## Store

The connector alone writes `data.db`; mandate opens it read-only and filters
every read. The connector sets `PRAGMA user_version = 1`, migrates its own
store in place on upgrade, and mandate reads only the current version.

```sql
CREATE TABLE containers (
    id                TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,     -- dm, group, channel, thread, guild, …
    parent_id         TEXT,
    name              TEXT,
    is_group          INTEGER NOT NULL DEFAULT 0,
    participants_json TEXT NOT NULL DEFAULT '[]',  -- [{"id": "…", "name": "…"}]
    updated_at        TEXT NOT NULL,
    last_message_at   TEXT              -- last activity the service reported
);

CREATE TABLE messages (
    seq          INTEGER PRIMARY KEY AUTOINCREMENT,
    id           TEXT NOT NULL UNIQUE,
    container_id TEXT NOT NULL,
    sender_id    TEXT,
    sender_name  TEXT,
    sent_at      TEXT NOT NULL,
    is_from_me   INTEGER NOT NULL DEFAULT 0,
    body         TEXT,
    media_type   TEXT,
    reply_to_id  TEXT,
    edited_at    TEXT,
    deleted      INTEGER NOT NULL DEFAULT 0,
    ingested_at  TEXT NOT NULL
);

CREATE INDEX messages_container_sent ON messages(container_id, sent_at);
CREATE INDEX messages_sent ON messages(sent_at);
CREATE INDEX messages_sender ON messages(sender_id);

CREATE TABLE media (
    message_id TEXT NOT NULL REFERENCES messages(id),
    idx        INTEGER NOT NULL,
    type       TEXT NOT NULL,   -- MIME type
    name       TEXT,
    size       INTEGER,
    ref        BLOB,            -- what the connector needs to fetch it; never leaves it
    PRIMARY KEY (message_id, idx)
);
```

- Times are `2026-11-02T14:05:09.000Z`, so they compare as text.
- IDs are the service's and stable; a chat ID is what `container` params take.
- `parent_id` chains are acyclic and at most three deep; deeper chats are
  invisible to agents.
- A name the person chose (a profile or push name) starts with `~`;
  address-book names don't. The owner's chat with themself gets the
  service's name for it.
- A DM has `kind = 'dm'`. If its ID is not the other person's, it lists them
  in `participants_json`, so hiding a person hides their DMs.
- `is_from_me` marks the owner's messages; `reply_to_id` the message answered.
- An edit updates `body` and sets `edited_at`; an identical redelivery changes
  nothing. A delete sets `deleted = 1`.
- Files are `media` rows numbered from 0; a message whose files aren't stored
  keeps `media_type` (`image`, `video`, …).

## Reads

`POST /read` with `{"op": "fetch", "args": {"chat": "…", "limit": "50"}}`

The connector fetches from the service, stores the result and answers what
it stored; the gateway reads that back through its filter.

```json
{"message_ids": ["3EB0…", "3EB1…"], "container_ids": [], "more": true}
```

- `more`: the service has another page. Never totals: they would count what
  the agent may not see.
- `container_ids`: chats a sync stored; agents see only the visible count.
- Refusals that depend on stored state (no such seq, deleted) are `400` or
  `410`; the gateway answers both with its own `404`.

## Media

`POST /media` with `{"message": "3EB0…", "index": 0, "limit": 104857600}`
streams the file with `Content-Type`, `Content-Length` and, if named,
`Content-Disposition: attachment; filename=…`. The connector stops
downloading past `limit` (`413`), since a sender can understate a size. It
may answer `429` while other files download.

## Writes

`POST /prepare` with `{"op": "send", "args": {"chat": "…", "text": "on my way", "seq": "41"}}`:

```json
{"args": {"chat": "…", "text": "on my way", "reply_to": "3EB0…"},
 "display": [{"label": "To", "value": "Climbing crew"},
             {"label": "In reply to", "value": "~Sam: see you at 6?"},
             {"label": "Text", "value": "on my way", "param": "text"}],
 "container": "…", "message": "3EB0…"}
```

- `args` are what `execute` will run; `display` is what the owner sees and
  signs (`param` marks an `editable` arg's field).
- `container` and `message` are the chat written to and the stored message
  answered; the gateway checks them against the agent's grants.
- `prepare` is idempotent (its own `args` give the same output) and changes
  nothing anywhere.

`POST /execute` takes a write approval ([records.md](records.md)). Before
running anything the connector:

1. verifies the signature with a key its key log trusts now;
2. checks the record is unexpired and names this connector and a write op;
3. re-runs `prepare` on the record's `args` and answers `409` if `args` or
   `display` differ;
4. durably records the nonce and the record's `id`, answering `409` if
   either was seen.

Then it runs the write and answers `{"status": "done", "ref": "3EB2…"}`:
`done` (sent), `failed` (not sent, `detail` says why) or `unknown` (may have
reached the service; never retried). `output` may carry the service's answer
for the agent, without credentials.

## Key log

The connector keeps its own copy of the owner's key log
([records.md](records.md)) in `private/`.

- `GET /keys` → `{"length": 4, "head": "<hex hash of the last entry>"}`.
- `PUT /keys` takes the whole log and keeps it only if it extends the one held
  with every new entry verifying; else `409`.
- An empty log trusts the first log pushed. Only the owner uid reaches
  `PUT /keys` and root starts both, so this is trust on first use. `mandate
  reset` empties it again.

## Setup

Linking is a dialogue the connector describes as data and the app renders;
the owner side relays it unchanged. `POST /setup` with
`{"step": "", "values": {}}` starts it, and each answer is the next step:

```json
{"step": "phone", "title": "Link WhatsApp", "text": "Your phone number, with country code.",
 "fields": [{"name": "phone", "label": "Phone number", "kind": "text", "required": true}]}
```

```json
{"step": "pair", "text": "On your phone: Linked devices › Link with phone number, then type this code.",
 "code": "ABCD-1234", "poll": 2}
```

| Key | |
|---|---|
| `step` | ID the app sends back with `values` |
| `title`, `text` | plain text |
| `fields` | `name`, `label`, `kind`, `required`; `kind` is `text`, `secret` (typed hidden, never shown again), `number` or `choice` (with `options: [{"value", "label"}]`) |
| `qr` | text to show as a QR code |
| `code` | a code to type on, or compare with, another device |
| `confirm` | a yes/no question, answered as `{"confirm": "yes"}` |
| `poll` | seconds until the app re-sends the step with empty `values` |
| `done` | linked |

A refused value answers `400` with an `error` for the owner; the app keeps
the step. `POST /setup` while linked answers `409`. `POST /unlink` forgets the
session and credentials, keeps stored messages, and health becomes `setup`.

## Chats for the owner

`GET /containers?q=fam&parent=&limit=50` lists stored chats for the app's
chat picker, so the owner side never opens the store:

```json
[{"id": "…", "name": "Family 🏡", "kind": "group", "parent_id": "", "last_activity": "2026-11-02T08:10:00Z"}]
```

`q` matches name or ID, `parent` lists a chat's children, `limit` is at most
500.

## Conformance kit

`mandate-conformance CONNECTOR [--config FILE] [--store data.db]` runs the
connector with a fresh state directory and checks what needs no account:

- the spec, and that every route refuses the wrong caller;
- `--check-config` refuses unknown keys;
- health has a valid shape;
- the empty store's schema and `user_version`;
- setup steps have a valid shape;
- `execute` refuses a bad signature, an untrusted or expired key, another
  connector's record and a changed display, and `PUT /keys` refuses a
  rewritten log.

With `--store`, it also checks an existing `data.db` against the store rules.
Successful writes and replay are each connector's own tests; the Go SDK
handles replay centrally.
