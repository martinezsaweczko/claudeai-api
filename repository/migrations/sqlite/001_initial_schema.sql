-- +goose Up
CREATE TABLE IF NOT EXISTS subscriptions (
    id TEXT PRIMARY KEY,
    subscription_text TEXT,
    user TEXT,
    created_date DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_date DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS jwt_used (
    id TEXT PRIMARY KEY,
    jwt TEXT,
    created_date DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_date DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS jwt_index ON jwt_used (jwt);

CREATE TABLE IF NOT EXISTS file_usage (
    id TEXT PRIMARY KEY,
    file TEXT,
    result TEXT,
    created_date DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_date DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS user_usage (
    id TEXT PRIMARY KEY,
    user TEXT,
    file TEXT,
    created_date DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_date DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- +goose Down
DROP TABLE IF EXISTS subscriptions;
DROP TABLE IF EXISTS jwt_used;
DROP TABLE IF EXISTS file_usage;
DROP TABLE IF EXISTS user_usage;
