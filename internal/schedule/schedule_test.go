package schedule

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestParseKind(t *testing.T) {
	for title, want := range map[string]Kind{
		"Лекции":  KindLecture,
		"Семинар": KindSeminar,
		"Практические занятия": KindPractice,
		"Лабораторные занятия": KindLab,
		"Экзамен": KindExam,
		"Зачет":   KindExam,
		"Внеучебное мероприятие": KindOther,
		"": KindOther,
	} {
		if got := ParseKind(title); got != want {
			t.Errorf("ParseKind(%q) = %s, want %s", title, got, want)
		}
	}
}

func TestClock(t *testing.T) {
	c, err := ParseClock(" 08:45 ")
	if err != nil || c != 8*60+45 || c.String() != "08:45" {
		t.Fatalf("ParseClock = %d (%s), %v", c, c, err)
	}
	for _, bad := range []string{"", "8.45", "25:00", "08:60"} {
		if _, err := ParseClock(bad); !errors.Is(err, ErrInvalidClock) {
			t.Errorf("ParseClock(%q) err = %v", bad, err)
		}
	}
}

func TestPair(t *testing.T) {
	for start, want := range map[string]int{
		"08:45": 1, "10:20": 2, "11:55": 3, "13:30": 4, "15:05": 5, "16:40": 6, "18:15": 7,
		"09:00": 0, "07:00": 0,
	} {
		c, _ := ParseClock(start)
		if got := (Lesson{Start: c}).Pair(); got != want {
			t.Errorf("Pair(%s) = %d, want %d", start, got, want)
		}
	}
}

func TestDateOf(t *testing.T) {
	msk, _ := time.LoadLocation("Europe/Moscow")
	// 23:30 UTC 5 октября — это уже 6 октября по Москве.
	got := DateOf(time.Date(2026, 10, 5, 23, 30, 0, 0, time.UTC).In(msk))
	if want := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("DateOf = %s, want %s", got, want)
	}
}

type fakeRepo struct {
	cov      Coverage
	covErr   error
	lessons  []Lesson
	groups   map[string]Group
	users    map[int64]int64
	upserted [][]string
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{groups: map[string]Group{}, users: map[int64]int64{}}
}

func (f *fakeRepo) LessonsOn(context.Context, string, time.Time) ([]Lesson, error) {
	return f.lessons, nil
}
func (f *fakeRepo) DatesWithLessons(context.Context, string, time.Time, time.Time) ([]time.Time, error) {
	return []time.Time{time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)}, nil
}
func (f *fakeRepo) Coverage(context.Context, string) (Coverage, error) { return f.cov, f.covErr }

func (f *fakeRepo) UpsertGroups(_ context.Context, names []string) ([]Group, error) {
	f.upserted = append(f.upserted, names)
	out := make([]Group, len(names))
	for i, n := range names {
		g, ok := f.groups[n]
		if !ok {
			g = Group{ID: int64(len(f.groups) + 1), Name: n}
			f.groups[n] = g
		}
		out[i] = g
	}
	return out, nil
}

func (f *fakeRepo) GroupByID(_ context.Context, id int64) (Group, error) {
	for _, g := range f.groups {
		if g.ID == id {
			return g, nil
		}
	}
	return Group{}, ErrGroupNotFound
}

func (f *fakeRepo) UserGroup(ctx context.Context, userID int64) (Group, bool, error) {
	id, ok := f.users[userID]
	if !ok {
		return Group{}, false, nil
	}
	g, err := f.GroupByID(ctx, id)
	return g, true, err
}

func (f *fakeRepo) SetUserGroup(_ context.Context, userID, groupID int64) error {
	f.users[userID] = groupID
	return nil
}

type fakeSearcher struct {
	names   []string
	queries []string
}

func (f *fakeSearcher) SearchGroups(_ context.Context, q string) ([]string, error) {
	f.queries = append(f.queries, q)
	return f.names, nil
}

func newService(t *testing.T, repo *fakeRepo, s GroupSearcher) *Service {
	t.Helper()
	return NewService(repo, s, time.UTC)
}

var testGroup = Group{ID: 1, Name: "ИОП-ИТ-24/2"}

func TestServiceDay(t *testing.T) {
	repo := newFakeRepo()
	repo.cov = Coverage{
		From:      time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
		To:        time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC),
		UpdatedAt: time.Now(),
	}
	svc := newService(t, repo, &fakeSearcher{})
	g := testGroup

	day, err := svc.Day(context.Background(), g, time.Date(2026, 10, 6, 15, 0, 0, 0, time.UTC))
	if err != nil || !day.Known || !day.Date.Equal(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("in range: %+v, %v", day, err)
	}
	day, _ = svc.Day(context.Background(), g, time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC))
	if day.Known {
		t.Error("date outside coverage must not be Known")
	}

	never := newFakeRepo()
	never.covErr = ErrNotSynced
	day, err = newService(t, never, &fakeSearcher{}).Day(context.Background(), g, time.Now())
	if err != nil || day.Known || !day.UpdatedAt.IsZero() {
		t.Errorf("never synced: %+v, %v", day, err)
	}
}

func TestServiceMonthDays(t *testing.T) {
	svc := newService(t, newFakeRepo(), &fakeSearcher{})
	days, err := svc.MonthDays(context.Background(), testGroup, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if err != nil || len(days) != 2 || !days[1] || !days[15] {
		t.Errorf("MonthDays = %v, %v", days, err)
	}
}

func TestServiceUserGroups(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepo()
	searcher := &fakeSearcher{names: []string{"С06ББ-25/1", "С06ББ-25/2"}}
	svc := newService(t, repo, searcher)

	// У нового пользователя группы нет — её нужно выбрать.
	if _, ok, err := svc.UserGroup(ctx, 42); ok || err != nil {
		t.Fatalf("new user must have no group: ok=%v err=%v", ok, err)
	}

	found, err := svc.SearchGroups(ctx, "  ББ-25  ")
	if err != nil || len(found) != 2 || found[1].Name != "С06ББ-25/2" {
		t.Fatalf("search: %+v, %v", found, err)
	}
	if searcher.queries[0] != "ББ-25" {
		t.Errorf("query not normalized: %q", searcher.queries[0])
	}

	if _, err := svc.SelectGroup(ctx, 42, found[1].ID); err != nil {
		t.Fatal(err)
	}
	// Выбор виден сразу, несмотря на кеш «группа не выбрана» выше.
	if g, ok, _ := svc.UserGroup(ctx, 42); !ok || g.Name != "С06ББ-25/2" {
		t.Errorf("selected group = %+v, ok=%v", g, ok)
	}
	if _, ok, _ := svc.UserGroup(ctx, 7); ok {
		t.Error("other users are not affected")
	}

	if _, err := svc.SelectGroup(ctx, 42, 999); !errors.Is(err, ErrGroupNotFound) {
		t.Errorf("unknown group err = %v", err)
	}
	if _, err := svc.SearchGroups(ctx, "И"); !errors.Is(err, ErrQueryTooShort) {
		t.Errorf("short query err = %v", err)
	}
}

// countingRepo считает обращения к БД, чтобы проверить кеши сервиса.
type countingRepo struct {
	*fakeRepo
	groupByID, userGroup int
}

func (c *countingRepo) GroupByID(ctx context.Context, id int64) (Group, error) {
	c.groupByID++
	return c.fakeRepo.GroupByID(ctx, id)
}

func (c *countingRepo) UserGroup(ctx context.Context, id int64) (Group, bool, error) {
	c.userGroup++
	return c.fakeRepo.UserGroup(ctx, id)
}

func TestServiceCaches(t *testing.T) {
	ctx := context.Background()
	repo := &countingRepo{fakeRepo: newFakeRepo()}
	svc := NewService(repo, &fakeSearcher{names: []string{"A", "B"}}, time.UTC)

	found, _ := svc.SearchGroups(ctx, "AB")
	for range 3 {
		if g, err := svc.GroupByID(ctx, found[1].ID); err != nil || g.Name != "B" {
			t.Fatalf("GroupByID = %+v, %v", g, err)
		}
	}
	if repo.groupByID != 0 {
		t.Errorf("groups found by search must be served from cache, got %d DB calls", repo.groupByID)
	}

	for range 3 {
		_, _, _ = svc.UserGroup(ctx, 1)
	}
	if repo.userGroup != 1 {
		t.Errorf("UserGroup hit DB %d times, want 1", repo.userGroup)
	}
}
