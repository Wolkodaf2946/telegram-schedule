package syncer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"telegram-schedule/internal/schedule"
	"telegram-schedule/internal/scraper"
)

func clocks(t *testing.T, ss ...string) []schedule.Clock {
	t.Helper()
	var out []schedule.Clock
	for _, s := range ss {
		c, err := schedule.ParseClock(s)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c)
	}
	return out
}

func TestNextRun(t *testing.T) {
	msk, _ := time.LoadLocation("Europe/Moscow")
	kamchatka, _ := time.LoadLocation("Asia/Kamchatka")

	cases := []struct {
		name  string
		now   time.Time
		times []string
		loc   *time.Location
		want  time.Time
	}{
		{
			name: "before 06:00 MSK — today", times: []string{"06:00"}, loc: msk,
			now:  time.Date(2026, 10, 6, 2, 0, 0, 0, time.UTC), // 05:00 МСК
			want: time.Date(2026, 10, 6, 6, 0, 0, 0, msk),
		},
		{
			name: "exactly 06:00 MSK — tomorrow", times: []string{"06:00"}, loc: msk,
			now:  time.Date(2026, 10, 6, 6, 0, 0, 0, msk),
			want: time.Date(2026, 10, 7, 6, 0, 0, 0, msk),
		},
		{
			name: "server in UTC, evening — next MSK morning", times: []string{"06:00"}, loc: msk,
			now:  time.Date(2026, 10, 6, 22, 30, 0, 0, time.UTC), // уже 01:30 7-го по МСК
			want: time.Date(2026, 10, 7, 6, 0, 0, 0, msk),
		},
		{
			// Баг прошлого проекта: hour - tz - 3 давал отрицательный час для восточных поясов.
			name: "far east timezone", times: []string{"07:00"}, loc: kamchatka,
			now:  time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC),
			want: time.Date(2026, 10, 7, 7, 0, 0, 0, kamchatka),
		},
		{
			name: "several times — nearest", times: []string{"06:00", "14:30", "20:00"}, loc: msk,
			now:  time.Date(2026, 10, 6, 10, 0, 0, 0, msk),
			want: time.Date(2026, 10, 6, 14, 30, 0, 0, msk),
		},
		{
			name: "several times — after the last one", times: []string{"06:00", "14:30"}, loc: msk,
			now:  time.Date(2026, 10, 6, 23, 0, 0, 0, msk),
			want: time.Date(2026, 10, 7, 6, 0, 0, 0, msk),
		},
		{
			name: "month boundary", times: []string{"06:00"}, loc: msk,
			now:  time.Date(2026, 10, 31, 12, 0, 0, 0, msk),
			want: time.Date(2026, 11, 1, 6, 0, 0, 0, msk),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := NextRun(c.now, clocks(t, c.times...), c.loc)
			if !ok || !got.Equal(c.want) {
				t.Errorf("NextRun = %s (ok=%v), want %s", got, ok, c.want)
			}
		})
	}

	if _, ok := NextRun(time.Now(), nil, msk); ok {
		t.Error("NextRun with no times should return ok=false")
	}
}

func TestRetryAt(t *testing.T) {
	now := time.Date(2026, 10, 6, 6, 0, 0, 0, time.UTC)
	retries := []time.Duration{5 * time.Minute, time.Hour}
	planned := now.Add(24 * time.Hour)

	if got, _ := retryAt(now, 0, retries, planned, true); !got.Equal(now.Add(5 * time.Minute)) {
		t.Errorf("first retry = %s", got)
	}
	if got, _ := retryAt(now, 7, retries, planned, true); !got.Equal(now.Add(time.Hour)) {
		t.Errorf("later retries should use the last delay, got %s", got)
	}
	soon := now.Add(10 * time.Minute)
	if got, _ := retryAt(now, 1, retries, soon, true); !got.Equal(soon) {
		t.Errorf("retry must not go past the planned run, got %s", got)
	}
	if got, ok := retryAt(now, 0, retries, time.Time{}, false); !ok || !got.Equal(now.Add(5*time.Minute)) {
		t.Errorf("without planned runs retry should still happen, got %s, %v", got, ok)
	}
	if _, ok := retryAt(now, 0, nil, time.Time{}, false); ok {
		t.Error("no retries and no planned runs — should stop")
	}
}

type fakeFetcher struct {
	months  []scraper.Month
	errFor  map[string]error
	block   chan struct{}
	fetched []string
}

func (f *fakeFetcher) Fetch(_ context.Context, group string, _ int) ([]scraper.Month, error) {
	if f.block != nil {
		<-f.block
	}
	f.fetched = append(f.fetched, group)
	if err := f.errFor[group]; err != nil {
		return nil, err
	}
	return f.months, nil
}

type fakeStore struct {
	replaced map[string]bool
	from, to time.Time
	runs     []schedule.SyncRun
	tracked  []string
}

func newFakeStore() *fakeStore { return &fakeStore{replaced: map[string]bool{}} }

func (s *fakeStore) ReplaceLessons(_ context.Context, group string, from, to time.Time, _ []schedule.Lesson) error {
	s.replaced[group], s.from, s.to = true, from, to
	return nil
}

func (s *fakeStore) RecordSync(_ context.Context, r schedule.SyncRun) error {
	s.runs = append(s.runs, r)
	return nil
}

func (s *fakeStore) LastSuccessfulSync(context.Context, string) (time.Time, error) {
	return time.Time{}, schedule.ErrNotSynced
}

func (s *fakeStore) TrackedGroups(context.Context) ([]string, error) { return s.tracked, nil }

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

var october = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func TestRunGroup_ReplacesWholeRange(t *testing.T) {
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	f := &fakeFetcher{months: []scraper.Month{
		{First: october, Lessons: []schedule.Lesson{{Group: "G", Date: october}}},
		{First: nov}, // пустой месяц тоже должен очистить старые данные
	}}
	st := newFakeStore()
	res := New(f, st, "G", 1, discard()).RunGroup(context.Background(), "G")
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if !st.from.Equal(october) || !st.to.Equal(time.Date(2026, 11, 30, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("replaced range %s..%s", st.from, st.to)
	}
	if res.Lessons != 1 || len(st.runs) != 1 || st.runs[0].Err != nil {
		t.Errorf("result %+v, runs %+v", res, st.runs)
	}
}

func TestRunGroup_FetchErrorKeepsOldData(t *testing.T) {
	st := newFakeStore()
	f := &fakeFetcher{errFor: map[string]error{"G": scraper.ErrCountMismatch}}
	res := New(f, st, "G", 0, discard()).RunGroup(context.Background(), "G")
	if !errors.Is(res.Err, scraper.ErrCountMismatch) {
		t.Fatalf("err = %v", res.Err)
	}
	if st.replaced["G"] {
		t.Error("data must not be replaced when fetch fails")
	}
	if len(st.runs) != 1 || st.runs[0].Err == nil {
		t.Errorf("failed run must be recorded: %+v", st.runs)
	}
}

func TestRunAll_OneGroupFailureDoesNotStopOthers(t *testing.T) {
	st := newFakeStore()
	st.tracked = []string{"A", "DEF", "B"} // группа по умолчанию тоже выбрана кем-то
	f := &fakeFetcher{
		months: []scraper.Month{{First: october}},
		errFor: map[string]error{"A": errors.New("site is down")},
	}
	s := New(f, st, "DEF", 0, discard())
	s.pause = 0

	results, err := s.RunAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.fetched; len(got) != 3 || got[0] != "DEF" || got[1] != "A" || got[2] != "B" {
		t.Errorf("fetched %v, want [DEF A B] without duplicates", got)
	}
	if results[1].Err == nil || results[0].Err != nil || results[2].Err != nil {
		t.Errorf("results = %+v", results)
	}
	if !st.replaced["DEF"] || !st.replaced["B"] || st.replaced["A"] {
		t.Errorf("replaced = %v", st.replaced)
	}
	if !anyFailed(results) {
		t.Error("anyFailed should report the failed group")
	}
}

func TestRunGroup_NoConcurrentRunsOfSameGroup(t *testing.T) {
	f := &fakeFetcher{block: make(chan struct{}), months: []scraper.Month{{First: october}}}
	s := New(f, newFakeStore(), "G", 0, discard())

	done := make(chan Result)
	go func() { done <- s.RunGroup(context.Background(), "G") }()
	// Дожидаемся, пока первый запуск займёт группу.
	for !s.isActive("G") {
		time.Sleep(time.Millisecond)
	}

	if res := s.RunGroup(context.Background(), "G"); !errors.Is(res.Err, ErrInProgress) {
		t.Errorf("second run err = %v, want ErrInProgress", res.Err)
	}
	close(f.block)
	if res := <-done; res.Err != nil {
		t.Errorf("first run: %v", res.Err)
	}
}

func (s *Syncer) isActive(group string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active[group]
}
