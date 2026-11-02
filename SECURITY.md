# Security

## Model

Each agent has its own token, made by your browser when you create the agent;
the console sees only its hash. A token reads nothing by itself: reads need a
grant you signed with your passkey, which names the agent, one connector,
optionally some chats, a time window (once, or a recurring schedule) and a
horizon, the age past which messages stay invisible. A grant covers exactly
the chats you checked; chats created later, such as new threads under a
checked channel, are not included. Outside every open grant, reads answer
403 and the agent can only ask you for one. Hide rules apply on top of
grants.

An agent may also propose writes, under a grant on the connector that is
open, or that ended in the last 24 hours and was not closed. The grant must
cover the chat a write names and, for a reply, the message it answers; once
a grant has ended, only messages from before its end count. A chat or
message outside its grants answers the same as one that doesn't exist, and
the agent sees only the proposal's status and result, never how the
connector rendered it. A write runs only when the connector holding that
account verifies your passkey's signature over the exact request it will
run, or a signature from a tab you unlocked with your passkey in the last 10
minutes. Each request runs at most once.

Grants and agents are signed on the console, and the gateway checks each
signature against the key log itself, as connectors do for writes. So a
stolen agent token, or a prompt-injected agent, can read only what its open
grants cover, and only while they are open. An access request tells the
agent nothing about which chats exist: it always gets a request ID, and you
see the terms that matched nothing.

What each part is trusted with:

- **The host.** Root on the machine running mandate can do anything mandate
  can.
- **Connectors** hold full account credentials. A compromised connector can
  act on its own account without approval. It can't reach the others: each
  runs as its own user, its sessions in a `private/` directory only it can
  read, without access to private addresses.
- **The gateway** reads the connectors' databases and applies grants and
  hide rules for agents. It can't set up, unlink, execute or change keys;
  connectors take those only from the console's user.
- **The console** shows you what you approve. Passkeys sign an opaque
  challenge, so what you see is only as honest as the page showing it. It
  reaches the connectors' sockets but not their databases, and can't make
  the reads, file downloads or forwards agents make. Through routes only it
  may call, it lists chats for the chat picker and has connectors prepare
  writes for your approval, so it sees chat names and the recipients,
  numbers, group members and email subjects a preview shows, but no stored
  message text or files. It keeps no agent tokens. It can't create agents or
  grants without your passkey, nor bring back one you deleted or revoked:
  the gateway remembers every agent and grant that went missing.

## What to do as the owner

- Hide chats that carry login codes or anything agents shouldn't read. The
  login-code filter is a heuristic over message text, chat names and file
  names; a code inside a picture gets through. The gateway logs hidden IDs
  that match nothing, which catches typos and WhatsApp's two IDs per contact
  (phone number and LID).
- Run agents on a machine that has no SSH keys, ssh-agent or secrets for your
  other machines, so a prompt-injected agent can do no more than its token
  and grants allow.
- Grant narrowly: the chats a task needs, the shortest window, a short
  horizon. Close a grant you no longer need, which also ends its day of
  writes. Today, on Now, shows who read what, and counts the reads it could
  not log.
- Keep extensions off the console's site, apart from your passkey manager:
  set their site access to "on click", or use a browser profile without them.
- Read recipients by their ID. A name starting with `~` was chosen by that
  person or a group member, not by you.
- Back up `/var/lib/mandate-*` encrypted. The databases hold your full message
  history in plain text, and per-file keys to fetch its photos and files;
  clearing the `media` table's `ref` column in the copies keeps those out.
  Back up the gateway's `access.json` and `keys.json` with the console's
  `console.db`, and restore them together.

## Known limits

- **Script on the console origin**, for example from a browser extension,
  can do whatever your session can: approve anything while the tab is
  unlocked, read pending requests, list every chat's name and ID through the
  chat picker, hidden ones included, unlink accounts, replace a linking QR
  code, read a token or Google cookies as you paste them, or read an agent's
  token as it is shown. A passkey prompt names the site, not the action, so
  the script can also turn your next touch into approving a key it
  registered, which lasts until you remove it, or into signing an agent or
  grant of its choosing. Your passkey manager is trusted further still: it
  holds the keys.
- **First key.** A connector with no key log accepts the first log the
  console sends: after a fresh install or `mandate-reset-keys`, when you add
  a connector later, or when its `private/keys.json` is lost. A gateway with
  no key log waits until every connector answers: if every log is empty it
  takes the console's, if they agree it starts from theirs, and otherwise it
  serves no agents, so one connector that keeps crashing locks every agent
  out. Once it has a log, the gateway no longer compares it with the
  connectors'.
- **The gateway** reads every `data.db` directly. Whoever controls its
  process reads everything, grants or not.
- **A compromised console** sees every chat's stored name, kind, parent and
  last activity, hidden chats included and without login-code redaction;
  Fastmail chat names are email subjects. If it is compromised while you
  create an agent, it can capture the token your browser makes, or have
  your touch sign a hash of its own.
- **Inside an open grant**, an agent reads whatever the grant covers, and
  can pass it on. A grant limits what leaks, not where it goes.
- **Revoking** stops reads once the gateway next syncs with the console,
  every 15 seconds; a new agent's token likewise works after the next sync.
  A media download (up to 10 minutes) or a forwarded read already under
  way runs to the end, and so does a wait on a proposal's status. A
  compromised console can't undo a revocation the gateway saw, but it can
  hide one from the gateway by never sending it, so a grant revoked on a
  compromised console may stay open until it ends.
- **Waiting writes** stay approvable after the grant that admitted them
  closes or is revoked, and after Stop all, and the agent can still read
  their result. Deleting the agent denies them. Stop all cancels the agent
  and grant approvals you had started but not finished; agents keep their
  tokens and can ask again.
- **Writes after a window**: for 24 hours after a grant's window ends, the
  agent may still propose the writes it covered, including writes that name
  no chat, such as Fastmail's `send` or `http` requests. A routine's day of
  writes comes back after each window. An ended one-off grant leaves Open
  now but keeps this until the 24 hours pass, you close it, or you Stop all.
- **Console outages**: while the gateway can't reach the console, it keeps
  serving the last agents and grants it verified, and recurring grants keep
  opening on schedule.
- **No lifetime cap**: an agent lasts until you delete it, and a recurring
  grant until you revoke it. One touch you were tricked into lasts until you
  notice.
- **Failed verification is final.** The gateway treats an agent or grant
  whose signature stops verifying, for example after its key was removed or
  the console's hostname changed, as revoked for good. Create it again.
- **Restoring `console.db`** without the gateway's `access.json` from the
  same time lets the console serve again what you revoked after the backup.
- **What agents see without a grant**: `GET /v1/connectors` lists every
  connector, its operations and health, including the `http` connector's
  services and allowlist. `GET /v1/access` names the chats in an agent's
  grants, by their current names, also while the grant is closed.
- **`http` grants** cover every allowlisted read of every configured
  service, and let the agent propose `http` writes; chats and horizon don't
  apply. There is no write-only grant.
- **Writes that name no chat**, such as Fastmail's `send`, need only a grant
  on the connector: a grant on one email thread lets the agent propose mail
  to any address. You still approve the recipients.
- **One-tap approvals**: Approve signs what the card shows, with the
  agent's duration and horizon preselected; a request for every chat is a
  single tap. A single matched contact starts ticked when its name wasn't
  chosen by that person, which relies on each connector marking such names
  with `~`. One-off summaries give times in the server's time zone. An agent
  can send you up to 10 high-priority notifications an hour.
- **The read log** is best effort: the console counts the reads it could
  not log, not which they were, and loses the count when the gateway
  restarts. It records no search terms or forwarded paths, and a search
  across chats is logged without the chats it touched.
- **Agent setup**: a token copied from the setup sheet lands in your
  clipboard history. The install line checks the binary against
  `SHA256SUMS` from the same release, which catches a broken download, not a
  forged release.
- **Access requests** carry the agent's own words in the reason. Read the
  connector, chats, duration and horizon on the request, not the reason.
  Chat terms are resolved only once a request fits the agent's limits (3
  waiting, 10 an hour); how long resolving takes may hint at a match.
- **Notifications** go to your ntfy topic and name the agent and connector,
  never message text or chats.
- **Unlock windows**: script on the console origin can have your next touch
  open a window dated into the future.
- **Identity key changes** on WhatsApp and Signal are accepted as the
  official apps accept them. A change in the last 7 days shows on the
  approval, and a change after you approve makes the send refuse.
- **Email senders**: a From header proves nothing. Fastmail mail counts as
  yours only when it sits in Sent.
- **Google Messages** pairs with a Google session. Pair it with a Google
  account that holds nothing else, set in Messages on the phone; its cookies
  can act as that account.
- **Matrix**: messages under keys that don't trace back to a cross-signed
  device start with `[unverified]`, and plaintext in encrypted rooms with
  `[unencrypted]`. mandate shares room keys only with cross-signed devices
  of joined members.
- **Upstream search**: Discord's `guild-search` reports no further pages
  once grants or hide rules removed a hit, so its results can end early.
- **Unofficial clients**: the WhatsApp, Signal, Discord and Google Messages
  connectors can get an account limited or banned.

## If the server is compromised

Unlinking in the console deletes mandate's copy of a login; it doesn't end
the session at the service. To end each one there:

| What | Where to end it |
|---|---|
| Agent token | delete the agent under Agents and create a new one |
| Passkeys | remove them under Keys, or `sudo mandate-reset-keys` |
| WhatsApp | phone: Settings → Linked devices → mandate → Log out |
| Signal | phone: Settings → Linked devices → mandate → Unlink |
| Matrix | Element: Settings → Sessions → mandate → Sign out |
| Discord | change the account password, which ends every token |
| Google Messages | phone: Messages → Device pairing → Unpair; and the pairing Google account: Security → Your devices → sign out the browser session the cookies came from |
| Fastmail | Settings → Privacy & Security → Connected apps & API tokens → revoke |
| `http` services | revoke the token at each service |

## Reporting

Report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/adeci/mandate/security/advisories/new).
