-- +goose Up
-- Справочник групп. Наполняется из поиска по сайту; в callback-кнопки кладётся id,
-- потому что названия групп не влезают в лимит callback_data (64 байта).
CREATE TABLE groups (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT        NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Выбранная пользователем группа. Строки нет — используется группа по умолчанию.
CREATE TABLE users (
    user_id    BIGINT      PRIMARY KEY, -- Telegram user id (From.ID), не chat id
    group_id   BIGINT      NOT NULL REFERENCES groups (id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX users_group_id_idx ON users (group_id);

-- +goose Down
DROP TABLE users;
DROP TABLE groups;
