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
