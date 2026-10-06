-- +goose Up
CREATE TABLE lessons (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_name  TEXT   NOT NULL,
    lesson_date DATE   NOT NULL,
    starts_at   TIME   NOT NULL,
    ends_at     TIME   NOT NULL,
    discipline  TEXT   NOT NULL,
    lesson_type TEXT   NOT NULL DEFAULT '',
    address     TEXT   NOT NULL DEFAULT '',
    room        TEXT   NOT NULL DEFAULT '',
    teachers    TEXT[] NOT NULL DEFAULT '{}',
    department  TEXT   NOT NULL DEFAULT '',
    CONSTRAINT lessons_time_order CHECK (ends_at > starts_at)
);

-- Все чтения идут по (группа, дата) или (группа, диапазон дат).
CREATE INDEX lessons_group_date_idx ON lessons (group_name, lesson_date, starts_at);

-- Журнал синхронизаций: по нему бот понимает, насколько свежие данные
-- и какой диапазон дат вообще загружен.
CREATE TABLE sync_runs (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    group_name  TEXT        NOT NULL,
    started_at  TIMESTAMPTZ NOT NULL,
    finished_at TIMESTAMPTZ NOT NULL,
    status      TEXT        NOT NULL CHECK (status IN ('ok', 'failed')),
    range_from  DATE,
    range_to    DATE,
    lessons     INTEGER     NOT NULL DEFAULT 0,
    error       TEXT        NOT NULL DEFAULT '',
    CONSTRAINT sync_runs_ok_has_range CHECK (
        status <> 'ok' OR (range_from IS NOT NULL AND range_to IS NOT NULL AND range_from <= range_to)
    )
);

CREATE INDEX sync_runs_group_status_idx ON sync_runs (group_name, status, finished_at DESC);

-- +goose Down
DROP TABLE sync_runs;
DROP TABLE lessons;
