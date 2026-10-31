CREATE TABLE IF NOT EXISTS proposals (
    id         TEXT PRIMARY KEY,
    connector  TEXT NOT NULL,
    op         TEXT NOT NULL,
    args       TEXT NOT NULL,     -- JSON; the prepared args, replaced by the approved ones
    display    TEXT NOT NULL,     -- JSON []approval.Field
    note       TEXT NOT NULL DEFAULT '',
    status     TEXT NOT NULL,
    result     TEXT,              -- JSON connector.Result
    created_at INTEGER NOT NULL,  -- unix ms
    expires_at INTEGER NOT NULL,  -- unix ms
    -- The outstanding challenge: exact record bytes, nonce, and issue time.
    record     BLOB,
    nonce      BLOB,
    issued_at  INTEGER            -- unix ns; the challenge hashes it at full precision
) STRICT;

CREATE INDEX IF NOT EXISTS proposals_by_status ON proposals (status, expires_at);

-- Hash-chained audit log: hash = sha256(previous hash || event).
CREATE TABLE IF NOT EXISTS events (
    seq   INTEGER PRIMARY KEY,
    event TEXT NOT NULL,
    hash  BLOB NOT NULL
) STRICT;

CREATE TRIGGER IF NOT EXISTS events_no_update BEFORE UPDATE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;

CREATE TRIGGER IF NOT EXISTS events_no_delete BEFORE DELETE ON events
BEGIN
    SELECT RAISE(ABORT, 'events are append-only');
END;
