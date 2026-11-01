-- Source schema shared by every ingester. Also loaded by the NixOS VM test.
CREATE TABLE IF NOT EXISTS containers (
    id                TEXT PRIMARY KEY,
    kind              TEXT NOT NULL,   -- dm, group, channel, thread, ...
    parent_id         TEXT,            -- the enclosing container
    name              TEXT,
    is_group          INTEGER NOT NULL DEFAULT 0,
    participants_json TEXT NOT NULL DEFAULT '[]',
    updated_at        TEXT NOT NULL,
    last_message_at   TEXT             -- last activity the service reported
);

CREATE TABLE IF NOT EXISTS messages (
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

CREATE INDEX IF NOT EXISTS messages_container_sent ON messages(container_id, sent_at);
CREATE INDEX IF NOT EXISTS messages_sent ON messages(sent_at);
CREATE INDEX IF NOT EXISTS messages_sender ON messages(sender_id);
