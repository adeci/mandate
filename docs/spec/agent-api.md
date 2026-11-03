# Agent API

Agents reach mandate over HTTPS through an **agent identity**: a credential
on a machine that every agent there uses. The `mandate` CLI is the reference
client.

An identity reads a connector only inside a grant the owner signed: exact
chats or all, a window (once or a routine), and a horizon (how far back).
Writes are proposals the owner approves on a device.

## Basics

- Routes are under `/agent/v1/`. Protocol version 1.
- The client checks the server's TLS key during the code exchange. If the
  certificate chains to a root the machine trusts (ACME, the owner's own
  certificate), normal checks apply from then on; otherwise the client pins
  the key.
- Bodies are JSON. Times are RFC 3339 in UTC. Durations are integer seconds.
- Every route except `join` needs `Authorization: Bearer <token>`.

A refusal is a non-2xx status with `error`, written for the agent:

```json
{"error": "no read access to signal now; ask the owner with: mandate request signal --reason WHY",
 "request": "mandate request signal --reason WHY"}
```

| Status | Meaning |
|---|---|
| `400` | bad arguments |
| `401` | unknown or revoked token |
| `403` | no grant open on that connector now; `request` holds the command to ask for one |
| `404` | unknown connector, op, request or proposal, or a chat or message the identity may not see (these look the same) |
| `409` | conflicts with state, e.g. nothing to extend |
| `413` | file over the media limit |
| `429` | rate limited or the connector is resting; `retry_after_seconds` says how long |
| `503` | the connector is connecting or needs the owner; `error` says which |

## Code exchange

A new party (`mandate login`, or a new device's app, see
[owner-api.md](owner-api.md)) joins by showing a 6-digit code that the owner
types into the app. A PAKE turns the code into a shared key, so a relayed or
mistyped code fails and nobody can guess it offline.

1. The new party draws a random 6-digit code and shows it. The code never
   goes over the wire.
2. It starts the exchange: `POST {base}/join` with `{"kind": "agent", "name": "laptop"}`
   → `201 {"id": "<32 hex>"}`. For agents `{base}` is `/agent/v1`. `name`
   defaults to the hostname.
3. The owner sees the pending item in the app (name, source address, time),
   picks it, types the code and unlocks the device. The server, as CPace
   initiator, sends `Ya`.
4. The new party polls `GET {base}/join/{id}?wait=SECONDS` → `{"msg": "<Ya hex>"}`,
   runs CPace as responder and posts:
   `POST {base}/join/{id}` with `{"msg": "<Yb>", "confirm": "<party_confirm>", "sealed": "<sealed>"}`.
5. The server checks `party_confirm`. If it fails, the answer is `403`, the
   attempt counts as a wrong code, and the new party polls again for the
   owner's next try. If it holds, the server answers
   `{"confirm": "<server_confirm>"}`. The new party checks it against the
   TLS key of this connection.
6. The owner's device signs the record for the payload: the `identity`
   record for an agent, the `key` `add` entry for a device
   ([records.md](records.md)). The new party's poll then answers
   `{"status": "approved"}`, and its token works.

Three wrong codes or 10 minutes end the exchange (`{"status": "ended"}`).
`join` needs no token, so at most 3 exchanges wait at once and one address
may start 5 an hour; beyond that, `429`.

### Derivations

CPace is [draft-irtf-cfrg-cpace](https://datatracker.ietf.org/doc/draft-irtf-cfrg-cpace/),
CPACE-RISTR255-SHA512, with its own test vectors there. Inputs:

| | |
|---|---|
| PRS | the 6 ASCII digits |
| CI | `mandate join` ‖ kind (`agent` or `device`), ASCII |
| sid | the 16 bytes of `id` |
| ADa, ADb | empty |

From its output key `ISK` (64 bytes):

```
party_confirm  = HMAC-SHA-256(ISK, "mandate exchange party")
server_confirm = HMAC-SHA-256(ISK, "mandate exchange server" ‖ spki_sha256)
seal_key       = HMAC-SHA-256(ISK, "mandate exchange seal")
sealed         = AES-256-GCM(seal_key, nonce = 12 zero bytes, aad = id, payload)
```

`spki_sha256` is the SHA-256 of the server's DER SubjectPublicKeyInfo. Each
key seals one message, so the fixed nonce is safe. The payload is JCS JSON:

- agent: `{"token_sha256": hex(SHA-256(token))}`, where the token is 32 random
  bytes in lowercase hex and is hashed as that 64-character string;
- device: `{"key": <key object, records.md>, "token_sha256": hex(SHA-256(device token))}`.

Vector (all hex unless quoted):

```
ISK            000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f
               202122232425262728292a2b2c2d2e2f303132333435363738393a3b3c3d3e3f
id             000102030405060708090a0b0c0d0e0f
spki_sha256    f910152c80be10ee2280a04cb011ce9131b993408adb19cf47f1495c331bcdbc
token          "1111111111111111111111111111111111111111111111111111111111111111"
payload        '{"token_sha256":"3138bb9bc78df27c473ecfd1410f7bd45ebac1f59cf3ff9cfe4db77aab7aedd3"}'
party_confirm  fb60f9fc9ad3d93847f5d4a12457d11afa66dc6d37cd258035a0c8660d847325
server_confirm 732f5b9afa026f4715bddcae71c864e6bb6473d00c70b5bc4f80b676df94f00c
seal_key       2d66336f1cff1e116b70ac5df2ae840353d9650752ff1f583ae6de5e96e7c905
sealed         3b3b1c233fad60e0e148b7ac79f6a6845841af004b042d709ed21d95a840babe
               2767ef6877114b374635f0bfe7bfc67102d13a9d47515a4b15f6174911ccc39e
               7e88321efea491d5516290aa3f82f4df7e0707d788af33678c8bbf295e7977cf
               77b217
```

The CLI keeps the result in `~/.config/mandate/credentials.json`, mode 0600:

```json
{"url": "https://host:8443", "token": "9f2c…", "spki_sha256": "4b1a…"}
```

## Connectors

`GET /agent/v1/connectors`

```json
[{"name": "signal", "doc": "", "media": true,
  "ops": [{"name": "send", "kind": "write", "summary": "Send a message", "params": [ … ]}],
  "health": {"state": "ok"},
  "access": {"open": true, "until": "2026-11-03T08:00:00Z"}}]
```

`ops`, `media` and `health` come from the connector ([connector.md](connector.md)).
`access` is this identity's access now: `open` with `until`, or the `next`
time a grant opens.

## Reads

A read needs a grant open now on the connector, else `403`. Within open
grants, a message is readable if one of them names its chat (or all chats)
and it was sent at or after that grant's cutoff (now minus the horizon). The
owner's never-share list hides chats and people on top, and messages that
look like login codes read `[login code hidden]`.

Reads answer a table:

```json
{"columns": ["seq", "sent_at", "sender_id", "sender", "reply_to", "body"],
 "rows": [[41, "2026-11-02 08:10", "+15550001", "~Sam", null, "see you at 6?"]],
 "more": true, "next_before": 41}
```

- `next_before` is the `before` for the next page; `more` alone means a
  narrower query or higher `limit` finds more.
- `sender` is `me` for the owner; a leading `~` marks a name the person chose.
- Bodies may carry `[image/jpeg]`, `[#0 …]` (one of several files),
  `[deleted]` or `[login code hidden]`.

| Route | Parameters | Columns |
|---|---|---|
| `GET {c}/chats` | `search`, `limit` (20, max 500) | `id kind name last_message` |
| `GET {c}/messages` | `chat`*, `before`, `limit` (20) | `seq sent_at sender_id sender reply_to body` |
| `GET {c}/search` | `text`*, `chat`, `since` (YYYY-MM-DD), `before`, `limit` (50) | `seq sent_at chat_id chat sender_id sender reply_to body` |
| `GET {c}/media` | `chat`*, `seq`*, `index` | the file |
| `POST {c}/{op}` (read op) | `{"args": {…}}` | as `search` |

`{c}` is `/agent/v1/{connector}`; `*` marks required parameters.

- `chats` lists chats with activity at or after their cutoff, newest first.
- `messages` and `search` page by `(sent_at, seq)`, newest first.
- `media` streams the file with `Content-Type`, `Content-Length` and
  `Content-Disposition`.
- A read op's `container` and `message` params are checked before the
  connector runs; what it stored comes back through the same filter. A grant
  that closes meanwhile answers `403`.

## Access

`GET /agent/v1/access` lists the identity's grants that are open or will
open again:

```json
[{"id": "g-…", "connector": "signal",
  "chats": [{"id": "…", "name": "Family 🏡"}],
  "schedule": {"days": [1, 2, 3, 4, 5], "start": "07:00", "length": 1200, "zone": "Europe/Berlin"},
  "horizon": 86400,
  "now": {"open": false, "next": "2026-11-03T06:00:00Z"}}]
```

A one-off grant has `from` and `until` instead of `schedule`.

`POST /agent/v1/access/requests` asks the owner for access:

| Kind | Body |
|---|---|
| once | `{"connector", "chats": ["fam"], "for", "horizon", "reason"}` |
| routine | `{"connector", "chats", "schedule": {"days", "start", "length"}, "horizon", "reason"}` |
| extend | `{"connector", "extend": true, "for", "reason"}` |

- `chats` are 1–10 search terms of 1–200 characters. Each finds at most 5
  visible chats; terms that match nothing reach the owner as such. The
  answer does not depend on what matched.
- `for` defaults to 1800, at most 43200. `horizon` defaults to 604800; 0 is
  all history.
- A routine's `days` are 0 (Sunday) to 6, empty for every day; `start` is
  `HH:MM` in the owner's zone; `length` is at most 86400.
- `extend` lengthens the current or recently ended window by `for`, keeping
  its chats and horizon; with nothing to extend, `409`.
- `reason` is required, at most 1000 characters, shown as the agent's words.
- 3 waiting requests and 10 an hour per identity; beyond that, `429` before
  anything is looked up.

The answer is `202 {"id": "r-…"}`. `GET /agent/v1/access/requests/{id}?wait=SECONDS`
(at most 900) answers `{"status", "grant_id", "until"}` with `status`
`pending`, `granted`, `denied` or `expired` (after an hour). Once it says
`granted`, the next read works.

## Proposals

`POST /agent/v1/{connector}/{op}` with a write op proposes:

```json
{"args": {"chat": "…", "text": "on my way", "seq": "41"}, "note": "the owner asked me to reply"}
```

- The chat and the message answered must be readable under a grant open
  now, or one whose window ended in the last 24 hours and was not closed;
  under an ended grant, only messages sent before its end. Anything else is
  `404`, before the connector is asked.
- `note` is shown to the owner as the agent's words.

The answer is `202 {"id": "p-…", "status": "pending"}`.
`GET /agent/v1/proposals/{id}?wait=SECONDS` (at most 900) answers
`{"id", "status", "result"}`:

| `status` | |
|---|---|
| `pending` | waiting for the owner |
| `executing` | approved, running |
| `done` | sent; `result.ref` identifies it |
| `failed` | not sent; `result.detail` says why |
| `unknown` | may have been sent; never retried |
| `rejected` | the owner said no |
| `expired` | undecided after 24 hours |

What the owner was shown, and the note, never come back to the agent.
Another identity's request or proposal is `404`.
