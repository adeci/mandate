CREATE TABLE IF NOT EXISTS proposals (
    id         TEXT PRIMARY KEY,
    connector  TEXT NOT NULL,
    op         TEXT NOT NULL,
    args       TEXT NOT NULL,     -- JSON; the prepared args, replaced by the approved ones
    display    TEXT NOT NULL,     -- JSON []approval.Field
    note       TEXT NOT NULL DEFAULT '',
    agent_id   TEXT NOT NULL DEFAULT '',  -- who proposed it, as the gateway knows them
    agent_name TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL,
    created_at INTEGER NOT NULL,  -- unix ms
    expires_at INTEGER NOT NULL,  -- unix ms
    decided_at INTEGER,           -- unix ms
    decided_by TEXT,              -- name of the passkey that approved or denied
    -- Proof of an approval, checkable offline against the pinned key: the
    -- signed record bytes, the other challenge inputs, and the assertion.
    record     BLOB,
    nonce      BLOB,
    issued_at  INTEGER,           -- unix ns; the challenge hashes it at full precision
    assertion  TEXT,              -- or, in an unlock window, {"delegation", "signature"}
    result     TEXT               -- JSON connector.Result
) STRICT;

CREATE INDEX IF NOT EXISTS proposals_by_status ON proposals (status, expires_at);
CREATE INDEX IF NOT EXISTS proposals_by_decided ON proposals (decided_at);

-- Outstanding approval challenges. A proposal may have several; using one
-- spends them all.
CREATE TABLE IF NOT EXISTS challenges (
    nonce     BLOB PRIMARY KEY,
    proposal  TEXT NOT NULL REFERENCES proposals (id),
    record    BLOB NOT NULL,
    issued_at INTEGER NOT NULL,   -- unix ns
    session   BLOB NOT NULL       -- the session that asked
) STRICT;

CREATE INDEX IF NOT EXISTS challenges_by_proposal ON challenges (proposal);

CREATE TABLE IF NOT EXISTS sessions (
    hash          BLOB PRIMARY KEY, -- sha256 of the cookie's token
    credential_id TEXT NOT NULL,     -- '' for a password session
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL
) STRICT;

DROP TABLE IF EXISTS passkeys;
DROP TABLE IF EXISTS key_changes;

-- Key changes waiting for a trusted key to approve them.
CREATE TABLE IF NOT EXISTS key_requests (
    id            TEXT PRIMARY KEY,
    op            TEXT NOT NULL,     -- add | remove
    name          TEXT NOT NULL,
    credential_id TEXT NOT NULL UNIQUE,
    public_key    TEXT NOT NULL,
    synced        INTEGER NOT NULL,
    aaguid        BLOB,              -- the authenticator's model, for additions
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL
) STRICT;

-- Agents and grants are stored as the signed records the gateway verifies.
-- Deleting or revoking drops the signature and keeps the row, so a
-- compromised console has nothing to serve again.
CREATE TABLE IF NOT EXISTS agents (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    token_sha256 TEXT NOT NULL,
    signed       TEXT,            -- JSON approval.Signed; NULL once deleted
    created_at   INTEGER NOT NULL,
    deleted_at   INTEGER
) STRICT;

CREATE TABLE IF NOT EXISTS grants (
    id         TEXT PRIMARY KEY,
    agent      TEXT NOT NULL REFERENCES agents (id),
    connector  TEXT NOT NULL,
    signed     TEXT,              -- JSON approval.Signed; NULL once revoked
    created_at INTEGER NOT NULL,
    revoked_at INTEGER
) STRICT;

CREATE TABLE IF NOT EXISTS access_requests (
    id         TEXT PRIMARY KEY,
    agent_id   TEXT NOT NULL,
    agent_name TEXT NOT NULL,
    connector  TEXT NOT NULL,
    chats      TEXT NOT NULL,     -- JSON []proposal.Chat
    unmatched  TEXT NOT NULL,     -- JSON []string, terms no chat matched
    for_ns     INTEGER NOT NULL,
    horizon_ns INTEGER NOT NULL,
    reason     TEXT NOT NULL,
    status     TEXT NOT NULL,     -- pending | granted | denied | expired
    grant_id   TEXT NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    decided_at INTEGER
) STRICT;

CREATE INDEX IF NOT EXISTS access_requests_by_status ON access_requests (status, expires_at);
CREATE INDEX IF NOT EXISTS access_requests_by_agent ON access_requests (agent_id, created_at);

-- Outstanding access record challenges; request is the access request a
-- grant answers, or ''.
CREATE TABLE IF NOT EXISTS access_challenges (
    nonce     BLOB PRIMARY KEY,
    kind      TEXT NOT NULL,
    record    BLOB NOT NULL,
    issued_at INTEGER NOT NULL,   -- unix ns
    session   BLOB NOT NULL,
    request   TEXT NOT NULL
) STRICT;

CREATE TABLE IF NOT EXISTS reads (
    at        INTEGER NOT NULL,
    agent_id  TEXT NOT NULL,
    connector TEXT NOT NULL,
    verb      TEXT NOT NULL,
    chat      TEXT NOT NULL,
    grants    TEXT NOT NULL,      -- JSON []string
    count     INTEGER NOT NULL,
    dropped   INTEGER NOT NULL    -- earlier events the gateway couldn't log
) STRICT;

CREATE INDEX IF NOT EXISTS reads_by_at ON reads (at);
