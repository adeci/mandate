# Owner API

The app talks to the owner side under `/owner/v1/`, on the one HTTPS port. `front` terminates TLS and forwards `/owner/` and `/setup` to the owner side's socket.

Bodies are JSON, and durations are in integer seconds. Records and envelopes are as in `records.md`.

## TLS

TLS uses one of three:

- ACME for a domain, with the TLS-ALPN-01 challenge, so the port must be 443 and reachable by the CA;
- the owner's own certificate;
- a key mandate makes itself.

Pairing gives the app the server's SPKI SHA-256, which it checks on first connect. If the certificate chains to a root the device trusts (ACME, the owner's own certificate), normal checks apply from then on; otherwise the app pins the key.

## Pairing

**First device.**

1. `sudo mandate setup` prints the `/setup` address, a setup password (also kept in a root-only file) and the server key's fingerprint, which the app shows before pairing.
2. The setup page asks for the password, then shows a QR code and the same link as text:

   ```
   mandate://pair?url=https%3A%2F%2Fbox.example%3A8443&spki=<hex>&secret=<32 bytes, base64url>
   ```

3. The app makes its signing key and a device token, then calls `POST /owner/v1/pair` with:
   - `secret`;
   - `token_sha256`;
   - `entry`: key log entry seq 0, adding its key, signed by that key. The key's `name` is the device's name.

   It answers `{"kind": "key", "id": "<key id>"}`.
4. That spends the secret and the setup password.

The setup page exists only before the first pairing and again after `mandate reset`. Whoever pairs first owns the server: if the app's pairing succeeds, its key is seq 0 and nobody paired before it; if the secret is already spent, run `mandate reset`.

**Another device.** It uses the code exchange in [`agent-api.md`, Code exchange](agent-api.md#code-exchange):

- base `/owner/v1`, kind `device`;
- payload: the device's signing key, as a key object, and its `token_sha256`.

On an existing device, the owner:

1. picks the exchange from `GET /owner/v1/exchanges`;
2. types the code (`POST /owner/v1/exchanges/{id}/code`);
3. signs the `add` entry from `POST /owner/v1/exchanges/{id}/challenge`.

**`mandate reset`** (root) stops `mandate.target`, then wipes:

- the owner side's key log, identities, grants, device tokens, requests and proposals;
- every connector's key log, and the gateway's copy of the live set.

It keeps never-share, activity and every connector's spent approvals, so nothing signed before the reset can run again. Then it issues a new setup password and starts `mandate.target` again.

## Device token

- The app makes 32 random bytes, in lowercase hex as an agent's token is, and keeps them in the platform keystore. Only the `token_sha256` of that text reaches the server ([agent-api.md, Derivations](agent-api.md#derivations)).
- It is the bearer token (`Authorization: Bearer …`) for every call below. Signing still needs the device key.
- Removing the device's key revokes its token.

## Signing

Changes that grant access take two calls:

1. A `…/challenge` call returns an unsigned envelope: `{kind, record, nonce, issued_at}`.
2. The app shows the owner what the record says, rendered from the record bytes. The owner authenticates on the device, and the app signs and posts the envelope:

   ```
   POST /owner/v1/signed {kind, record, nonce, issued_at, key, sig, authenticator_data?}
   → {"kind": "grant", "id": "…"}
   ```

The server accepts a challenge:

- once;
- within 5 minutes of issuing it;
- only while the state it was made for still holds: the request is still waiting, the grant to extend is still live, the routine to resume is still paused.

Close, Stop all and deleting an identity cancel the challenges they affect.

With several devices, the first approval wins, and the others get `changed`.

Chat names that start with `~` were chosen by other people. The app shows chat IDs beside the names.

## Endpoints

| route | |
|---|---|
| `GET /owner/v1/now` | What waits (access requests and proposals), open grants, routines, stopped routines, and one day's activity: each read and write by an identity, with time, connector, chats and message count, never message text. `?day=YYYY-MM-DD&before=<RFC 3339 time>` pages back. |
| `POST /owner/v1/requests/{id}/challenge` | `{chats?, for?, horizon?, zone?}`: a grant answering the request. `chats`: any chats of the connector, chosen by the owner; the matched ones are a suggestion. `for` and `horizon` at most what was asked. `zone` is the device's IANA zone, for a routine. |
| `POST /owner/v1/requests/{id}/deny` | |
| `GET /owner/v1/proposals/{id}` | The display fields, which of them can be edited, and the agent's note. |
| `POST /owner/v1/proposals/{id}/challenge` | `{edits: {param: value}}`: the write record. |
| `POST /owner/v1/proposals/{id}/deny` | |
| `POST /owner/v1/grants/challenge` | `{identity, connector, chats?, for \| schedule, horizon, note?}` |
| `POST /owner/v1/grants/{id}/extend/challenge` | `{for}`: the grant again from now, until `for` past whichever is later, its end or now. |
| `POST /owner/v1/grants/{id}/resume/challenge` | A paused routine, under a new ID. |
| `POST /owner/v1/grants/{id}/close` | Ends a window or pauses a routine. |
| `POST /owner/v1/stop-all` | Closes every grant, denies every waiting request, and cancels every challenge. Proposals stay. Returns `{grants, requests}`. |
| `GET /owner/v1/exchanges` | Code exchanges waiting: `[{id, kind, name, address, started_at}]`. |
| `POST /owner/v1/exchanges/{id}/code` | `{code}`. Returns `200 {payload}` once the new party confirms, or `403 {attempts_left}`. |
| `POST /owner/v1/exchanges/{id}/challenge` | The `identity` record (kind `agent`) or the key `add` entry (kind `device`) for the payload. |
| `GET /owner/v1/identities` | Names, first and last seen, and grants. |
| `PATCH /owner/v1/identities/{id}` | `{name}` |
| `DELETE /owner/v1/identities/{id}` | Ends its grants, and refuses its waiting requests and proposals. |
| `GET /owner/v1/connectors` | Names, health, and whether setup is needed. |
| `GET /owner/v1/connectors/{name}/setup` | Setup steps as data (`connector.md`). |
| `POST /owner/v1/connectors/{name}/setup` | `{step, values}` |
| `POST /owner/v1/connectors/{name}/unlink` | |
| `GET /owner/v1/connectors/{name}/chats` | `?q=` or `?parent=`: chats for the picker, with ID, name, kind, parent and last activity. |
| `GET /owner/v1/never-share` | `[{connector, kind, id, name}]`, `kind` being `chat` or `person` |
| `PUT /owner/v1/never-share/{connector}/{kind}/{id}` | `{name?}`. Hides the chat, or the person's messages and DMs, from every read and chat list, whatever the grants say. |
| `DELETE /owner/v1/never-share/{connector}/{kind}/{id}` | |
| `GET /owner/v1/keys` | The key log as envelopes, and the trusted keys with their device names. |
| `POST /owner/v1/keys/{id}/remove/challenge` | The `remove` entry. |

Calls that return nothing above answer `{}`.

### Bodies

Times are RFC 3339 UTC, whole seconds. `…` stands for what the example would repeat.

A grant is its record (`records.md`) plus `identity_name`, `chat_names` where the request named them, and `closed_at` once closed or paused:

```json
{"id": "7RJ4P2W5ZK3HXQ6N5M2C4YB7DE", "identity": "QX3M7E2KQ4F5PN6ZJ7LDV2Y3AU", "identity_name": "laptop",
 "connector": "signal", "chats": ["group.6f1b2c"], "chat_names": {"group.6f1b2c": "~Family"},
 "horizon": 604800, "from": "2026-11-03T09:15:00Z", "until": "2026-11-03T10:15:00Z",
 "created_at": "2026-11-03T09:15:00Z", "closed_at": "2026-11-03T09:40:00Z"}
```

A proposal, as `GET /owner/v1/proposals/{id}` and `now` show it; `editable` lists the `param`s of `display`:

```json
{"id": "Z2K7Q4M3P6X5W2N7D4R3J6YB5C", "identity": "QX3M…", "identity_name": "laptop", "connector": "signal", "op": "send",
 "display": [{"label": "To", "value": "~Family"}, {"label": "Text", "value": "See you at 8", "param": "text"}],
 "editable": ["text"], "note": "the owner asked me to reply", "status": "pending",
 "created_at": "2026-11-03T09:30:00Z", "expires_at": "2026-11-04T09:30:00Z"}
```

`GET /owner/v1/now`:

```json
{"requests": [{"id": "B5TQ…", "identity": "QX3M…", "identity_name": "laptop", "connector": "signal", "kind": "once",
               "terms": [{"term": "fam", "chats": [{"id": "group.6f1b2c", "name": "~Family"}]}, {"term": "climbing", "chats": []}],
               "for": 1800, "horizon": 604800, "reason": "summarize the weekend plans",
               "created_at": "2026-11-03T09:14:00Z", "status": "pending"}],
 "proposals": [<proposal>],
 "grants": [<grant>], "routines": [<grant>], "stopped": [<grant>],
 "activity": [{"at": "2026-11-03T09:20:00Z", "identity": "QX3M…", "connector": "signal", "verb": "messages",
               "chats": [{"id": "group.6f1b2c", "name": "~Family"}], "count": 20}],
 "more": false}
```

- A request without `terms` asks for every chat; a term with no chats matched nothing. A routine has `schedule` (`days`, `start`, `length`) instead of `for`; `extend` has only `for`.
- `verb` is a read (`chats`, `messages`, `search`, `media`) or a write op. Activity is newest first, at most 500; `more` means `before` the last `at` finds more.

`GET /owner/v1/identities`:

```json
[{"id": "QX3M…", "name": "laptop", "created_at": "2026-11-03T09:10:00Z",
  "first_seen": "2026-11-03T09:14:00Z", "last_seen": "2026-11-03T09:20:00Z", "grants": [<grant>]}]
```

`GET /owner/v1/connectors`, with `health` as in `connector.md`:

```json
[{"name": "signal", "health": {"state": "resting", "detail": "daily budget used", "until": "2026-11-04T06:00:00Z"}, "setup": false}]
```

`GET /owner/v1/keys`:

```json
{"log": [<key envelope>, …], "trusted": [{"id": "b903…", "name": "Pixel 9", "public_key": "BN4C…", "type": "p256"}]}
```

`POST /owner/v1/exchanges/{id}/code` answers `{"payload": {"token_sha256": "…"}}` for an agent, with `key` added for a device. `GET /owner/v1/connectors/{name}/chats` answers as `GET /containers` in `connector.md`.

## Stream

`GET /owner/v1/stream` is server-sent events. The app keeps it open, refetches `now` after reconnecting, and gets a comment line every 30 seconds.

| event | data |
|---|---|
| `waiting` | `{kind: "request" \| "proposal", id, identity, connector}` |
| `exchange` | `{id, kind, name}` |
| `changed` | `{what: "now" \| "identities" \| "connectors" \| "keys"}` |

A notification shows the identity, the connector and what kind of thing it is, e.g. "praxis wants to read Signal". It never shows reasons or message text.

## Errors

```json
{"error": "the request was decided or expired meanwhile", "code": "conflict"}
```

| status | `code` | |
|---|---|---|
| 400 | `invalid` | malformed input, or a narrowing that widens |
| 401 | `token` | missing or unknown device token |
| 403 | `forbidden` | bad signature, untrusted key, wrong code |
| 404 | `not_found` | |
| 409 | `conflict` | decided, closed, replaced, or the challenge was cancelled |
| 410 | `expired` | challenge, proposal or exchange past its time |
| 429 | `rate_limited` | |
| 502 | `connector` | the connector is down or refused for its own reasons |
