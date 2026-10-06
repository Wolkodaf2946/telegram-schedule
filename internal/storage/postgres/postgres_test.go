package postgres

import (
	"context"
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"telegram-schedule/internal/schedule"
	"telegram-schedule/internal/users"
	"telegram-schedule/migrations"
)

// Интеграционные тесты идут на настоящем Postgres и запускаются, только если задан
// TEST_DATABASE_URL (make test-db поднимает базу в Docker). База очищается перед тестом.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.pool.Exec(ctx, `TRUNCATE lessons, sync_runs, users, groups RESTART IDENTITY CASCADE`); err != nil {
		t.Fatal(err)
	}
	return s
}

func day(m time.Month, d int) time.Time { return time.Date(2026, m, d, 0, 0, 0, 0, time.UTC) }

func lesson(group string, date time.Time, start string, teachers ...string) schedule.Lesson {
	s, _ := schedule.ParseClock(start)
	return schedule.Lesson{
		Group: group, Date: date, Start: s, End: s + 80, Discipline: "Предмет " + start,
		TypeTitle: "Лекции", Address: "Основной", Room: "Альфа 5.7", Teachers: teachers,
	}
}

func TestReplaceAndRead(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	from, to := day(10, 1), day(10, 31)

	// Две группы в одном диапазоне: данные одной не должны задевать другую.
	a := []schedule.Lesson{
		lesson("A", day(10, 6), "10:20", "Петров П. П.", "Иванов И. И."),
		lesson("A", day(10, 6), "08:45"),
		lesson("A", day(10, 7), "08:45"),
	}
	b := []schedule.Lesson{lesson("B", day(10, 6), "13:30")}
	if err := s.ReplaceLessons(ctx, "A", from, to, a); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceLessons(ctx, "B", from, to, b); err != nil {
		t.Fatal(err)
	}

	got, err := s.LessonsOn(ctx, "A", day(10, 6))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Start.String() != "08:45" || got[1].Start.String() != "10:20" {
		t.Fatalf("lessons of A on 6 Oct (sorted by time): %+v", got)
	}
	if !slices.Equal(got[1].Teachers, []string{"Петров П. П.", "Иванов И. И."}) || got[1].End.String() != "11:40" {
		t.Errorf("round trip mismatch: %+v", got[1])
	}
	if !got[0].Date.Equal(day(10, 6)) || got[0].Teachers == nil {
		t.Errorf("date/teachers: %+v", got[0])
	}

	dates, err := s.DatesWithLessons(ctx, "A", from, to)
	if err != nil || len(dates) != 2 || !dates[0].Equal(day(10, 6)) || !dates[1].Equal(day(10, 7)) {
		t.Errorf("dates with lessons: %v, %v", dates, err)
	}

	// Повторная загрузка того же диапазона заменяет данные, а не дублирует их.
	if err := s.ReplaceLessons(ctx, "A", from, to, a[:1]); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.LessonsOn(ctx, "A", day(10, 6)); len(got) != 1 {
		t.Errorf("after re-sync: %d lessons, want 1", len(got))
	}
	if got, _ := s.LessonsOn(ctx, "B", day(10, 6)); len(got) != 1 {
		t.Errorf("group B must be untouched, got %d lessons", len(got))
	}

	// Занятие вне заменяемого диапазона — ошибка, транзакция не применяется.
	if err := s.ReplaceLessons(ctx, "A", from, to, []schedule.Lesson{lesson("A", day(11, 1), "08:45")}); err == nil {
		t.Error("lesson outside range must be rejected")
	}
	if got, _ := s.LessonsOn(ctx, "A", day(10, 6)); len(got) != 1 {
		t.Errorf("failed replace must not change data, got %d lessons", len(got))
	}
}

func TestCoverage(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if _, err := s.Coverage(ctx, "A"); !errors.Is(err, schedule.ErrNotSynced) {
		t.Fatalf("err = %v, want ErrNotSynced", err)
	}

	t0 := time.Date(2026, 10, 1, 3, 0, 0, 0, time.UTC)
	runs := []schedule.SyncRun{
		{Group: "A", StartedAt: t0, FinishedAt: t0.Add(time.Second), From: day(10, 1), To: day(11, 30)},
		{Group: "A", StartedAt: t0.Add(24 * time.Hour), FinishedAt: t0.Add(25 * time.Hour), Err: errors.New("site down")},
		{Group: "B", StartedAt: t0.Add(48 * time.Hour), FinishedAt: t0.Add(49 * time.Hour), From: day(12, 1), To: day(12, 31)},
	}
	for _, r := range runs {
		if err := s.RecordSync(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	cov, err := s.Coverage(ctx, "A")
	if err != nil {
		t.Fatal(err)
	}
	// Неуспешная синхронизация и чужая группа не влияют на покрытие.
	if !cov.From.Equal(day(10, 1)) || !cov.To.Equal(day(11, 30)) || !cov.UpdatedAt.Equal(t0.Add(time.Second)) {
		t.Errorf("coverage = %+v", cov)
	}
}

func TestGroupsAndUsers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	first, err := s.UpsertGroups(ctx, []string{"ИОП-ИТ-24/2", "С06ББ-25/2"})
	if err != nil {
		t.Fatal(err)
	}
	// Повторный upsert возвращает те же id в порядке запроса, включая дубли во входе.
	again, err := s.UpsertGroups(ctx, []string{"С06ББ-25/2", "Новая", "ИОП-ИТ-24/2", "Новая"})
	if err != nil {
		t.Fatal(err)
	}
	if again[0] != first[1] || again[2] != first[0] || again[1].ID == 0 || again[3] != again[1] {
		t.Fatalf("upsert ids: first %+v, again %+v", first, again)
	}

	if g, err := s.GroupByID(ctx, first[1].ID); err != nil || g.Name != "С06ББ-25/2" {
		t.Errorf("GroupByID = %+v, %v", g, err)
	}
	if _, err := s.GroupByID(ctx, 999999); !errors.Is(err, schedule.ErrGroupNotFound) {
		t.Errorf("unknown group err = %v", err)
	}

	if _, ok, err := s.UserGroup(ctx, 1); ok || err != nil {
		t.Errorf("new user: ok=%v err=%v", ok, err)
	}
	for _, step := range []struct{ user, group int64 }{{1, first[0].ID}, {2, first[1].ID}, {1, first[1].ID}} {
		if err := s.SetUserGroup(ctx, step.user, step.group); err != nil {
			t.Fatal(err)
		}
	}
	if g, ok, err := s.UserGroup(ctx, 1); !ok || err != nil || g.Name != "С06ББ-25/2" {
		t.Errorf("user 1 group = %+v ok=%v err=%v", g, ok, err)
	}

	tracked, err := s.TrackedGroups(ctx)
	if err != nil || !slices.Equal(tracked, []string{"С06ББ-25/2"}) {
		t.Errorf("tracked groups = %v, %v", tracked, err)
	}
}

func TestUsersAccess(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	p := users.Profile{ID: 100, Username: "student", FirstName: "Иван", LastName: "Петров", LanguageCode: "ru"}
	u, created, err := s.TouchUser(ctx, p, users.StatusPending)
	if err != nil || !created || u.Status != users.StatusPending || u.Profile != p || u.GroupName != "" {
		t.Fatalf("first touch: %+v created=%v err=%v", u, created, err)
	}

	// Повторное касание обновляет профиль, но не меняет статус и не считается новым.
	p.Username = "renamed"
	u, created, err = s.TouchUser(ctx, p, users.StatusActive)
	if err != nil || created || u.Status != users.StatusPending || u.Username != "renamed" {
		t.Fatalf("second touch: %+v created=%v err=%v", u, created, err)
	}

	// Выбор группы до одобрения не делает группу отслеживаемой.
	groups, _ := s.UpsertGroups(ctx, []string{"ИОП-ИТ-24/2"})
	if err := s.SetUserGroup(ctx, p.ID, groups[0].ID); err != nil {
		t.Fatal(err)
	}
	if tracked, _ := s.TrackedGroups(ctx); len(tracked) != 0 {
		t.Errorf("groups of pending users must not be synced, got %v", tracked)
	}

	u, err = s.SetUserStatus(ctx, p.ID, users.StatusActive)
	if err != nil || u.Status != users.StatusActive || u.GroupName != "ИОП-ИТ-24/2" {
		t.Fatalf("approve: %+v, %v", u, err)
	}
	if tracked, _ := s.TrackedGroups(ctx); !slices.Equal(tracked, []string{"ИОП-ИТ-24/2"}) {
		t.Errorf("tracked after approval = %v", tracked)
	}
	if _, err := s.SetUserStatus(ctx, 999, users.StatusActive); !errors.Is(err, users.ErrUserNotFound) {
		t.Errorf("unknown user err = %v", err)
	}

	// В списке ожидающие идут первыми.
	if _, _, err := s.TouchUser(ctx, users.Profile{ID: 200}, users.StatusPending); err != nil {
		t.Fatal(err)
	}
	list, err := s.ListUsers(ctx, 10)
	if err != nil || len(list) != 2 || list[0].ID != 200 || list[0].Status != users.StatusPending {
		t.Errorf("list = %+v, %v", list, err)
	}
}

// Пользователи, выбравшие группу до появления статусов доступа (миграция 3),
// после обновления должны остаться с доступом и своей группой.
func TestMigration3KeepsExistingUsers(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	db := stdlib.OpenDBFromPool(s.pool)
	defer db.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 2); err != nil {
		t.Fatalf("down to v2: %v", err)
	}
	t.Cleanup(func() { _, _ = provider.Up(ctx) }) // вернуть схему для остальных тестов

	var groupID int64
	if err := s.pool.QueryRow(ctx, `INSERT INTO groups (name) VALUES ('ИОП-ИТ-24/2') RETURNING id`).Scan(&groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `INSERT INTO users (user_id, group_id) VALUES (1246713334, $1)`, groupID); err != nil {
		t.Fatal(err)
	}

	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("up to latest: %v", err)
	}
	u, created, err := s.TouchUser(ctx, users.Profile{ID: 1246713334, Username: "me"}, users.StatusPending)
	if err != nil || created || u.Status != users.StatusActive || u.GroupName != "ИОП-ИТ-24/2" {
		t.Errorf("existing user after upgrade: %+v created=%v err=%v", u, created, err)
	}
}
