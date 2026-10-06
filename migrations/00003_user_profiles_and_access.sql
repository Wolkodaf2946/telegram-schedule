-- +goose Up
-- users теперь хранит всех, кто писал боту, а не только выбравших группу:
-- профиль для журнала, статус доступа и время последней активности.
ALTER TABLE users ALTER COLUMN group_id DROP NOT NULL;

ALTER TABLE users
    ADD COLUMN username      TEXT        NOT NULL DEFAULT '',
    ADD COLUMN first_name    TEXT        NOT NULL DEFAULT '',
    ADD COLUMN last_name     TEXT        NOT NULL DEFAULT '',
    ADD COLUMN language_code TEXT        NOT NULL DEFAULT '',
    -- active — доступ есть; pending — ждёт одобрения админа; blocked — отклонён или отозван.
    ADD COLUMN status        TEXT        NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'pending', 'blocked')),
    ADD COLUMN last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now();

CREATE INDEX users_status_idx ON users (status);

-- +goose Down
DROP INDEX users_status_idx;
ALTER TABLE users
    DROP COLUMN last_seen_at,
    DROP COLUMN status,
    DROP COLUMN language_code,
    DROP COLUMN last_name,
    DROP COLUMN first_name,
    DROP COLUMN username;
DELETE FROM users WHERE group_id IS NULL;
ALTER TABLE users ALTER COLUMN group_id SET NOT NULL;
