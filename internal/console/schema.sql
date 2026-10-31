CREATE TABLE IF NOT EXISTS proposals (
    id         TEXT PRIMARY KEY,
    connector  TEXT NOT NULL,
    op         TEXT NOT NULL,
    args       TEXT NOT NULL,     -- JSON; the prepared args, replaced by the approved ones
    display    TEXT NOT NULL,     -- JSON []approval.Field
    note       TEXT NOT NULL DEFAULT '',
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
    assertion  TEXT,
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

-- Key changes waiting for a trusted key to approve them.
CREATE TABLE IF NOT EXISTS key_changes (
    id            TEXT PRIMARY KEY,
    op            TEXT NOT NULL,     -- add | remove
    name          TEXT NOT NULL,
    credential_id TEXT NOT NULL UNIQUE,
    public_key    TEXT NOT NULL,
    synced        INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    expires_at    INTEGER NOT NULL
) STRICT;
