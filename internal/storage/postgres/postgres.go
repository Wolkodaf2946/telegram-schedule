// Package postgres — хранилище расписания в PostgreSQL (pgx/v5).
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"telegram-schedule/internal/schedule"
	"telegram-schedule/internal/users"
	"telegram-schedule/migrations"
)

type Store struct {
	pool *pgxpool.Pool
}

// Open подключается к БД, проверяет соединение и применяет миграции.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	if err := migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func migrate(ctx context.Context, pool *pgxpool.Pool) error {
	db := stdlib.OpenDBFromPool(pool)
	defer db.Close()

	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		return fmt.Errorf("init migrations: %w", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	return nil
}

// ReplaceLessons атомарно заменяет все занятия группы в диапазоне [from, to]:
// либо в БД целиком новое расписание, либо целиком старое. Повторный запуск
// синхронизации с теми же данными даёт тот же результат, дублей не бывает.
func (s *Store) ReplaceLessons(ctx context.Context, group string, from, to time.Time, lessons []schedule.Lesson) error {
	for _, l := range lessons {
		if l.Group != group || l.Date.Before(from) || l.Date.After(to) {
			return fmt.Errorf("lesson %s %s of group %q is outside of replaced range",
				l.Date.Format(time.DateOnly), l.Start, l.Group)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // после Commit это no-op

	if _, err := tx.Exec(ctx,
		`DELETE FROM lessons WHERE group_name = $1 AND lesson_date BETWEEN $2 AND $3`,
		group, from, to,
	); err != nil {
		return fmt.Errorf("delete old lessons: %w", err)
	}

	rows := make([][]any, len(lessons))
	for i, l := range lessons {
		teachers := l.Teachers
		if teachers == nil {
			teachers = []string{}
		}
		rows[i] = []any{
			l.Group, l.Date, clockToPG(l.Start), clockToPG(l.End), l.Discipline,
			l.TypeTitle, l.Address, l.Room, teachers, l.Department,
		}
	}
	if _, err := tx.CopyFrom(ctx,
		pgx.Identifier{"lessons"},
		[]string{"group_name", "lesson_date", "starts_at", "ends_at", "discipline",
			"lesson_type", "address", "room", "teachers", "department"},
		pgx.CopyFromRows(rows),
	); err != nil {
		return fmt.Errorf("insert lessons: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

func (s *Store) LessonsOn(ctx context.Context, group string, date time.Time) ([]schedule.Lesson, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT group_name, lesson_date, starts_at, ends_at, discipline,
		       lesson_type, address, room, teachers, department
		FROM lessons
		WHERE group_name = $1 AND lesson_date = $2
		ORDER BY starts_at, ends_at, id`,
		group, date,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (schedule.Lesson, error) {
		var (
			l          schedule.Lesson
			start, end pgtype.Time
		)
		err := row.Scan(&l.Group, &l.Date, &start, &end, &l.Discipline,
			&l.TypeTitle, &l.Address, &l.Room, &l.Teachers, &l.Department)
		l.Start, l.End = clockFromPG(start), clockFromPG(end)
		return l, err
	})
}

func (s *Store) DatesWithLessons(ctx context.Context, group string, from, to time.Time) ([]time.Time, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT lesson_date
		FROM lessons
		WHERE group_name = $1 AND lesson_date BETWEEN $2 AND $3
		ORDER BY lesson_date`,
		group, from, to,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[time.Time])
}

// Coverage — объединение диапазонов всех успешных синхронизаций и время последней из них.
func (s *Store) Coverage(ctx context.Context, group string) (schedule.Coverage, error) {
	var (
		from, to pgtype.Date
		updated  pgtype.Timestamptz
	)
	err := s.pool.QueryRow(ctx, `
		SELECT min(range_from), max(range_to), max(finished_at)
		FROM sync_runs
		WHERE group_name = $1 AND status = 'ok'`,
		group,
	).Scan(&from, &to, &updated)
	if err != nil {
		return schedule.Coverage{}, err
	}
	if !from.Valid || !to.Valid || !updated.Valid {
		return schedule.Coverage{}, schedule.ErrNotSynced
	}
	return schedule.Coverage{From: from.Time, To: to.Time, UpdatedAt: updated.Time}, nil
}

func (s *Store) RecordSync(ctx context.Context, r schedule.SyncRun) error {
	status, errText := "ok", ""
	var from, to pgtype.Date
	if r.Err != nil {
		status, errText = "failed", r.Err.Error()
	} else {
		from = pgtype.Date{Time: r.From, Valid: true}
		to = pgtype.Date{Time: r.To, Valid: true}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sync_runs (group_name, started_at, finished_at, status, range_from, range_to, lessons, error)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.Group, r.StartedAt, r.FinishedAt, status, from, to, r.Lessons, errText,
	)
	return err
}

// LastSuccessfulSync — время последней успешной синхронизации группы.
func (s *Store) LastSuccessfulSync(ctx context.Context, group string) (time.Time, error) {
	cov, err := s.Coverage(ctx, group)
	if err != nil {
		return time.Time{}, err
	}
	return cov.UpdatedAt, nil
}

func clockToPG(c schedule.Clock) pgtype.Time {
	return pgtype.Time{Microseconds: int64(c) * int64(time.Minute/time.Microsecond), Valid: true}
}

func clockFromPG(t pgtype.Time) schedule.Clock {
	return schedule.Clock(t.Microseconds / int64(time.Minute/time.Microsecond))
}

// UpsertGroups добавляет группы в справочник (существующие не трогает)
// и возвращает их с id в том же порядке, что и names.
func (s *Store) UpsertGroups(ctx context.Context, names []string) ([]schedule.Group, error) {
	rows, err := s.pool.Query(ctx, `
		INSERT INTO groups (name)
		SELECT DISTINCT unnest($1::text[])
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name -- чтобы RETURNING вернул и существующие
		RETURNING id, name`,
		names,
	)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowToStructByPos[schedule.Group])
	if err != nil {
		return nil, err
	}
	byName := make(map[string]schedule.Group, len(found))
	for _, g := range found {
		byName[g.Name] = g
	}
	groups := make([]schedule.Group, len(names))
	for i, n := range names {
		groups[i] = byName[n]
	}
	return groups, nil
}

func (s *Store) GroupByID(ctx context.Context, id int64) (schedule.Group, error) {
	g := schedule.Group{ID: id}
	err := s.pool.QueryRow(ctx, `SELECT name FROM groups WHERE id = $1`, id).Scan(&g.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return schedule.Group{}, schedule.ErrGroupNotFound
	}
	return g, err
}

func (s *Store) UserGroup(ctx context.Context, userID int64) (schedule.Group, bool, error) {
	var g schedule.Group
	err := s.pool.QueryRow(ctx, `
		SELECT g.id, g.name
		FROM users u JOIN groups g ON g.id = u.group_id
		WHERE u.user_id = $1`,
		userID,
	).Scan(&g.ID, &g.Name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return schedule.Group{}, false, nil
	case err != nil:
		return schedule.Group{}, false, err
	}
	return g, true, nil
}

func (s *Store) SetUserGroup(ctx context.Context, userID, groupID int64) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO users (user_id, group_id) VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE SET group_id = EXCLUDED.group_id, updated_at = now()`,
		userID, groupID,
	)
	return err
}

// TrackedGroups — группы, выбранные хотя бы одним пользователем с доступом: их и обновляем.
func (s *Store) TrackedGroups(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT g.name
		FROM users u JOIN groups g ON g.id = u.group_id
		WHERE u.status = 'active'
		ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

const userColumns = `u.user_id, u.username, u.first_name, u.last_name, u.language_code,
	u.status, coalesce(g.name, ''), u.created_at, u.last_seen_at`

// scanUser читает колонки userColumns и, после них, extra.
func scanUser(row pgx.Row, extra ...any) (users.User, error) {
	var (
		u      users.User
		status string
	)
	dest := append([]any{&u.ID, &u.Username, &u.FirstName, &u.LastName, &u.LanguageCode,
		&status, &u.GroupName, &u.FirstSeen, &u.LastSeen}, extra...)
	err := row.Scan(dest...)
	u.Status = users.Status(status)
	return u, err
}

// TouchUser создаёт пользователя или обновляет профиль и last_seen существующего.
// Статус существующего пользователя не меняется.
func (s *Store) TouchUser(ctx context.Context, p users.Profile, status users.Status) (users.User, bool, error) {
	var created bool
	row := s.pool.QueryRow(ctx, `
		WITH u AS (
			INSERT INTO users (user_id, username, first_name, last_name, language_code, status)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (user_id) DO UPDATE SET
				username = EXCLUDED.username, first_name = EXCLUDED.first_name,
				last_name = EXCLUDED.last_name, language_code = EXCLUDED.language_code,
				last_seen_at = now()
			RETURNING *, (xmax = 0) AS inserted -- xmax = 0 только у вставленной строки
		)
		SELECT `+userColumns+`, u.inserted
		FROM u LEFT JOIN groups g ON g.id = u.group_id`,
		p.ID, p.Username, p.FirstName, p.LastName, p.LanguageCode, string(status),
	)
	u, err := scanUser(row, &created)
	return u, created, err
}

func (s *Store) SetUserStatus(ctx context.Context, id int64, status users.Status) (users.User, error) {
	row := s.pool.QueryRow(ctx, `
		WITH u AS (
			UPDATE users SET status = $2, updated_at = now() WHERE user_id = $1 RETURNING *
		)
		SELECT `+userColumns+`
		FROM u LEFT JOIN groups g ON g.id = u.group_id`,
		id, string(status),
	)
	u, err := scanUser(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return users.User{}, users.ErrUserNotFound
	}
	return u, err
}

// UsersPage — страница пользователей с данным статусом, недавно активные сверху.
// Порядок совпадает с индексом users_status_seen_idx, поэтому страница читается
// по индексу без сортировки.
func (s *Store) UsersPage(ctx context.Context, status users.Status, offset, limit int) ([]users.User, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+userColumns+`
		FROM users u LEFT JOIN groups g ON g.id = u.group_id
		WHERE u.status = $1
		ORDER BY u.last_seen_at DESC, u.user_id DESC
		OFFSET $2 LIMIT $3`,
		string(status), offset, limit,
	)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (users.User, error) { return scanUser(row) })
}

// CountUsers — число пользователей по статусам (по индексу, без чтения строк).
func (s *Store) CountUsers(ctx context.Context) (map[users.Status]int, error) {
	rows, err := s.pool.Query(ctx, `SELECT status, count(*) FROM users GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[users.Status]int, 3)
	for rows.Next() {
		var (
			status string
			n      int
		)
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		counts[users.Status(status)] = n
	}
	return counts, rows.Err()
}

func (s *Store) GetUser(ctx context.Context, id int64) (users.User, error) {
	u, err := scanUser(s.pool.QueryRow(ctx, `
		SELECT `+userColumns+`
		FROM users u LEFT JOIN groups g ON g.id = u.group_id
		WHERE u.user_id = $1`,
		id,
	))
	if errors.Is(err, pgx.ErrNoRows) {
		return users.User{}, users.ErrUserNotFound
	}
	return u, err
}

// Проверка на этапе компиляции, что Store реализует нужные сервисам интерфейсы.
var (
	_ schedule.Repository = (*Store)(nil)
	_ users.Repository    = (*Store)(nil)
)
