-- +goose Up
CREATE TABLE IF NOT EXISTS scheduled_commands (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    schedule TEXT NOT NULL,
    command TEXT NOT NULL,
    group_jid TEXT NOT NULL,
    enabled BOOLEAN DEFAULT 1,
    created_date DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_date DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_scheduled_commands_enabled ON scheduled_commands (enabled);

-- +goose Down
DROP TABLE IF EXISTS scheduled_commands;
