-- +goose Up
-- Админ-панель листает пользователей одного статуса, свежие сверху. Индекс отдаёт
-- страницу и счётчики по статусам без сортировки и полного чтения таблицы.
CREATE INDEX users_status_seen_idx ON users (status, last_seen_at DESC, user_id DESC);
DROP INDEX users_status_idx; -- покрывается новым индексом

-- +goose Down
CREATE INDEX users_status_idx ON users (status);
DROP INDEX users_status_seen_idx;
