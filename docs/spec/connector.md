# Connector protocol

A connector is a program that holds one account (a WhatsApp login, a mail
account). mandate talks to it over HTTP on a Unix socket and reads the
messages it stores from a SQLite file. Any language works; Go connectors get
all of this from `connector.Main`.

Protocol version 1, store schema version 2.

## Process

Each connector runs as its own Unix user, usually a systemd dynamic user,
started by mandate as:

```
PROGRAM --name NAME --socket PATH --state DIR [--config FILE] --gateway-group GROUP --owner-group GROUP
PROGRAM --check-config FILE
```

- `--name` is the name mandate configured it under, so one program can serve
  two accounts of the same service.
- `--config` is JSON; unknown keys are refused. `--check-config` exits 0 for
  a valid file, else non-zero with a message on stderr.
- The socket is mode 0666: who may call what is decided per route, by the
  caller's group (see [Callers](#callers)).
- `DIR/data.db` is the store, in SQLite's rollback-journal mode, not WAL:
  the gateway reads it without write access, and its SQLite can't open a
  WAL store's `-wal` read-only. mandate gives the gateway's group read
  access to `DIR` through a default ACL, so the connector creates the store
  and its `-journal` group-readable. Readers give up a query after 2 s, so a
  write waiting for their lock needs a busy timeout well above that; the Go
  SDK's is 10 s. `DIR/private/` (0700) holds the session, credentials, key
  log and spent approvals.
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
| `POST /setup`, `POST /unlink`, `POST /execute`, `PUT /keys`, `GET /containers`, `GET /icon` | | ✓ |

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

- `name` equals `--name`. mandate refuses another `protocol`.
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
every read. The connector sets `PRAGMA user_version = 2`, migrates its own
store in place on upgrade, and mandate reads only the current version.
Version 2 added `containers.icon` (`ALTER TABLE containers ADD COLUMN icon
TEXT`). A
connector may add its own tables, prefixed with the program's name
(`whatsapp_identity_changes`); mandate ignores them, but the gateway can
read them, so they hold nothing secret.

```sql
CREATE TABLE containers (
    id                TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,     -- dm, group, channel, announcement, voice, stage, thread, guild, forum, media, space, mailbox, …
    parent_id         TEXT,
    name              TEXT,
    is_group          INTEGER NOT NULL DEFAULT 0,
    participants_json TEXT NOT NULL DEFAULT '[]',  -- [{"id": "…", "name": "…", "handle": "…"}]
    updated_at        TEXT NOT NULL,
    last_message_at   TEXT,             -- last activity the service reported
    icon              TEXT              -- the service's handle for the chat's picture; never leaves the connector
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
  Where the service gives two chats one ID (a Discord server and its first
  channel), one gets a prefix (`guild:`, `mailbox:`).
- `parent_id` chains are acyclic and at most three deep; deeper chats are
  invisible to agents.
- A grant covers exactly the chats it names, never chats under them, except
  for mail: a mailbox is a chat of kind `mailbox`, a thread's `parent_id` is
  the mailbox it is filed in, and a grant on a mailbox also covers the chats
  filed directly in it that aren't mailboxes, as filed when read.
- Chats that only hold others, of kind `guild`, `space`, `forum` or `media`,
  hold no messages; agents never get them by name, only the chats in them.
- A name the person chose (a profile or push name) starts with `~`;
  address-book names and the owner's own name don't. The owner's chat with
  themself gets the service's name for it.
- A DM has `kind = 'dm'` and lists its person in `participants_json`, so
  hiding a person hides their DMs. Where known, the person has a `handle`,
  the @name or number the service shows them by (`@sam_p`,
  `@bob:example.org`, `+15551234567`), and terms find the DM by it.
- A group lists its members in `participants_json` where the service tells
  them, never the owner.
- `is_from_me` marks the owner's messages; `reply_to_id` the message answered.
- An edit updates `body` and sets `edited_at`; an identical redelivery changes
  nothing. A delete sets `deleted = 1`.
- A reader holding the store past the connector's busy timeout makes its
  writes fail with `SQLITE_BUSY`, and nothing may be lost to that: the write
  waits in memory and is stored in order once the reader lets go, so an edit
  never lands before its message. An event the service delivers again stays
  unacknowledged until then; the record of a message the owner sent waits
  the same way.
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

`GET /containers` lists stored chats for the app's chat picker, so the owner
side never opens the store:

| Query | Answers |
|---|---|
| none | the newest chats anywhere, no shelves |
| `parent=` (empty) | the top level: every shelf without a parent, then the chats without one |
| `parent=ID` | the same in ID |
| `q` | the shelves whose own name has every word of `q`, as headings, then the chats `q` finds as an agent's request term does ([agent-api.md](agent-api.md#access)); in `parent` at any depth if set |
| `ids=a,b` | exactly those chats, in that order |

A shelf is a holder or a mailbox. Each list is the newest first, a shelf
being as recent as the newest chat in it. `limit` (default 50, at most 500)
cuts the chats, never the shelves.

```json
[{"id": "guild:1100000000000000001", "kind": "guild", "name": "main", "parents": [],
  "last_activity": "2026-11-07T10:01:00Z", "opens": true, "icon": "9f2c4a1b0e7d"},
 {"id": "1100000000000000011", "kind": "channel", "name": "#general",
  "parents": [{"id": "guild:1100000000000000001", "name": "main", "icon": "9f2c4a1b0e7d"}],
  "last_activity": "2026-11-07T10:01:00Z"},
 {"id": "1300000000000000002", "kind": "group", "name": "climbing", "parents": [], "members": 3},
 {"id": "1300000000000000001", "kind": "dm", "name": "~Sam Park", "parents": [], "handle": "@sam_p",
  "participants": [{"id": "1000000000000000002", "name": "~Sam Park", "handle": "@sam_p"}]},
 {"id": "mailbox:mb-inbox", "kind": "mailbox", "name": "Inbox", "parents": [], "opens": true, "threads": 7}]
```

- `parents` are the chats it is in, the outermost first; its path is their
  names and its own.
- `opens`: a shelf, or a chat with chats in it, such as a channel's threads.
- `members` counts a group's stored members and the owner; `threads` the
  chats filed directly in a mailbox.
- `handle`: a DM's person's handle.
- `participants`: a DM's person; with `ids`, a group's members too.
- `icon` is the version of the chat's picture, a short hash of `icon`'s ref
  that changes with it; absent when the chat has none.
- The owner side adds `"hidden": true` to a chat that never-share hides: it,
  a chat it is in, or a DM's person is never shared. The connector doesn't
  know never-share.

## Pictures

`GET /icon?chat=ID` (owner only) answers the chat's picture, fetched from the
service as its stored `icon` names it: PNG or JPEG of at most 64 KiB, about
64 px, with its version in `Mandate-Icon`. A chat without a picture, or a
connector without pictures, is `404`; past two at once, `429`. A DM's
picture is its person's. Agents never get pictures. Go connectors implement
`IconFetcher`.

## Conformance kit

`mandate conformance PROGRAM [--config FILE] [--store data.db]` runs the
connector with a fresh state directory and checks what needs no account:

- the spec, and that every route refuses the wrong caller;
- `--check-config` refuses unknown keys;
- health has a valid shape;
- the empty store's schema and `user_version`;
- setup steps have a valid shape;
- `execute` refuses a bad signature, an untrusted, retired or removed key, an expired
  record, another connector's record and a changed display, and `PUT /keys`
  refuses a rewritten log.

With `--store`, it also checks an existing `data.db` against the store rules.
Successful writes and replay are each connector's own tests; the Go SDK
handles replay centrally.
