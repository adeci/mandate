# Security

## Model

An agent holding the gateway token can read the chats you haven't hidden and
propose writes. A write runs only when the connector holding that account
verifies your passkey's signature over the exact request it will run, or a
signature from a tab you unlocked with your passkey in the last 10 minutes.
Each request runs at most once.

What each part is trusted with:

- **The host.** Root on the machine running mandate can do anything mandate
  can.
- **Connectors** hold full account credentials. A compromised connector can
  act on its own account without approval. It can't reach the others: each
  runs as its own user, its sessions in a `private/` directory only it can
  read, without access to private addresses.
- **The gateway** reads the connectors' databases and applies hide rules for
  agents. It can't set up, unlink, execute or change keys; connectors take
  those only from the console's user.
- **The console** shows you what you approve. Passkeys sign an opaque
  challenge, so what you see is only as honest as the page showing it.

## What to do as the owner

- Hide chats that carry login codes or anything agents shouldn't read. The
  login-code filter is a heuristic. The gateway logs hidden IDs that match
  nothing, which catches typos and WhatsApp's two IDs per contact (phone
  number and LID).
- Run agents on a machine that has no SSH keys, ssh-agent or secrets for your
  other machines, so a prompt-injected agent can do no more than the token
  allows.
- Keep extensions off the console's site, apart from your passkey manager:
  set their site access to "on click", or use a browser profile without them.
- Read recipients by their ID. A name starting with `~` was chosen by that
  person or a group member, not by you.
- Back up `/var/lib/mandate-*` encrypted. The databases hold your full message
  history in plain text, and the keys to fetch its photos and files.

## Known limits

- **Script on the console origin**, for example from a browser extension,
  can do whatever your session can: approve anything while the tab is
  unlocked, read pending requests, unlink accounts, or replace a linking QR
  code. A passkey prompt names the site, not the action, so the script can
  also turn your next touch into approving a key it registered, which lasts
  until you remove it. Your passkey manager is trusted further still: it
  holds the keys.
- **First key.** A connector with no key log, after a fresh install or
  `mandate-reset-keys`, accepts the first log the console sends it.
- **One token** gives every agent the same access, with no per-agent scope.
- **Unlock windows**: script on the console origin can have your next touch
  open a window dated into the future.
- **Identity key changes** on WhatsApp and Signal are accepted as the
  official apps accept them. A change in the last 7 days shows on the
  approval, and a change after you approve makes the send refuse.
- **Matrix**: messages under keys that don't trace back to a cross-signed
  device start with `[unverified]`, and plaintext in encrypted rooms with
  `[unencrypted]`. mandate shares room keys only with cross-signed devices
  of joined members.
- **Upstream search**: Discord's `guild-search` pages through results
  Discord filtered, so a short page reveals that some hits were hidden.
- **Unofficial clients**: the WhatsApp, Signal, Discord and Google Messages
  connectors can get an account limited or banned.

## Reporting

Report vulnerabilities privately through GitHub's
[private vulnerability reporting](https://github.com/adeci/mandate/security/advisories/new).
