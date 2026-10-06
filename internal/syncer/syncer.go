// Package syncer переносит расписание с сайта в БД: разовый запуск (RunAll, RunGroup)
// и цикл по расписанию (Loop).
package syncer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"telegram-schedule/internal/schedule"
	"telegram-schedule/internal/scraper"
)

var ErrInProgress = errors.New("sync is already in progress")

type Fetcher interface {
	Fetch(ctx context.Context, group string, monthsAhead int) ([]scraper.Month, error)
}

type Store interface {
	ReplaceLessons(ctx context.Context, group string, from, to time.Time, lessons []schedule.Lesson) error
	RecordSync(ctx context.Context, r schedule.SyncRun) error
	LastSuccessfulSync(ctx context.Context, group string) (time.Time, error)
	TrackedGroups(ctx context.Context) ([]string, error)
}

// Result — итог синхронизации одной группы.
type Result struct {
	Group    string
	From, To time.Time
	Lessons  int
	Err      error
}

type Syncer struct {
	fetcher     Fetcher
	store       Store
	monthsAhead int
	timeout     time.Duration // на одну группу
	pause       time.Duration // между группами, чтобы не долбить сайт
	log         *slog.Logger
	now         func() time.Time

	mu     sync.Mutex
	active map[string]bool // группы, которые синхронизируются прямо сейчас
}

func New(f Fetcher, s Store, monthsAhead int, log *slog.Logger) *Syncer {
	return &Syncer{
		fetcher:     f,
		store:       s,
		monthsAhead: monthsAhead,
		timeout:     5 * time.Minute,
		pause:       2 * time.Second,
		log:         log.With("component", "syncer"),
		now:         time.Now,
		active:      make(map[string]bool),
	}
}

// Groups — что обновлять: группы, выбранные пользователями с доступом.
func (s *Syncer) Groups(ctx context.Context) ([]string, error) {
	groups, err := s.store.TrackedGroups(ctx)
	if err != nil {
		return nil, fmt.Errorf("get tracked groups: %w", err)
	}
	return groups, nil
}

// RunAll обновляет все отслеживаемые группы по очереди. Ошибка одной группы
// не останавливает остальные; она попадает в Result.Err и в журнал.
func (s *Syncer) RunAll(ctx context.Context) ([]Result, error) {
	groups, err := s.Groups(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(groups))
	for i, g := range groups {
		if i > 0 && !sleep(ctx, s.pause) {
			return results, ctx.Err()
		}
		results = append(results, s.RunGroup(ctx, g))
	}
	return results, nil
}

// RunGroup обновляет одну группу. Если она уже обновляется (например, ежедневный
// цикл и /refresh одновременно), сразу возвращает ErrInProgress, а не встаёт в очередь.
func (s *Syncer) RunGroup(ctx context.Context, group string) Result {
	if !s.lock(group) {
		return Result{Group: group, Err: ErrInProgress}
	}
	defer s.unlock(group)

	log := s.log.With("group", group)
	started := s.now()
	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	res := s.run(runCtx, group)

	// Журнал пишем даже при отменённом ctx (например, на остановке бота).
	recCtx, recCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer recCancel()
	run := schedule.SyncRun{
		Group: group, StartedAt: started, FinishedAt: s.now(),
		From: res.From, To: res.To, Lessons: res.Lessons, Err: res.Err,
	}
	if err := s.store.RecordSync(recCtx, run); err != nil {
		log.Error("record sync run", "err", err)
	}

	if res.Err != nil {
		log.Error("sync failed", "err", res.Err, "duration_ms", s.now().Sub(started).Milliseconds())
		return res
	}
	log.Info("sync finished",
		"from", res.From.Format(time.DateOnly), "to", res.To.Format(time.DateOnly),
		"lessons", res.Lessons, "duration_ms", s.now().Sub(started).Milliseconds())
	return res
}

func (s *Syncer) run(ctx context.Context, group string) Result {
	fail := func(err error) Result { return Result{Group: group, Err: err} }

	months, err := s.fetcher.Fetch(ctx, group, s.monthsAhead)
	if err != nil {
		return fail(fmt.Errorf("fetch: %w", err))
	}
	if len(months) == 0 {
		return fail(errors.New("fetch: no months returned"))
	}

	from, _ := schedule.MonthRange(months[0].First)
	_, to := schedule.MonthRange(months[len(months)-1].First)
	var lessons []schedule.Lesson
	for _, m := range months {
		lessons = append(lessons, m.Lessons...)
	}

	if err := s.store.ReplaceLessons(ctx, group, from, to, lessons); err != nil {
		return fail(fmt.Errorf("save: %w", err))
	}
	return Result{Group: group, From: from, To: to, Lessons: len(lessons)}
}

func (s *Syncer) lock(group string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[group] {
		return false
	}
	s.active[group] = true
	return true
}

func (s *Syncer) unlock(group string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.active, group)
}

// Schedule — когда запускать синхронизацию.
type Schedule struct {
	// Times — время суток плановых запусков в часовом поясе Location.
	// Пусто — автоматических обновлений нет, только /refresh и -sync-once.
	Times    []schedule.Clock
	Location *time.Location
	// OnStart — синхронизировать при старте, если данных нет или они старше суток:
	// рестарт или долгий простой не оставляют бота с устаревшим расписанием.
	OnStart bool
	// Retries — паузы между повторами после неудачи. Последняя повторяется,
	// пока не наступит следующий плановый запуск.
	Retries []time.Duration
}

// Loop запускает синхронизацию по расписанию sch, пока не отменён ctx.
func (s *Syncer) Loop(ctx context.Context, sch Schedule) {
	next, ok := NextRun(s.now(), sch.Times, sch.Location)
	if sch.OnStart && s.stale(ctx) {
		next, ok = s.now(), true
	}

	failures := 0
	for ok {
		s.log.Info("next sync scheduled", "at", next.In(sch.Location).Format(time.RFC3339))
		if !sleepUntil(ctx, next, s.now) {
			return
		}

		results, err := s.RunAll(ctx)
		if ctx.Err() != nil {
			return
		}
		planned, hasPlanned := NextRun(s.now(), sch.Times, sch.Location)
		if err == nil && !anyFailed(results) {
			failures = 0
			next, ok = planned, hasPlanned
			continue
		}
		next, ok = retryAt(s.now(), failures, sch.Retries, planned, hasPlanned)
		failures++
	}
	s.log.Info("automatic sync is disabled (SYNC_TIMES=off)")
}

// stale — у какой-то из групп нет данных или они старше суток.
func (s *Syncer) stale(ctx context.Context) bool {
	groups, err := s.Groups(ctx)
	if err != nil {
		s.log.Error("check data freshness", "err", err)
		return true
	}
	for _, g := range groups {
		last, err := s.store.LastSuccessfulSync(ctx, g)
		switch {
		case errors.Is(err, schedule.ErrNotSynced):
			return true
		case err != nil:
			s.log.Error("get last sync time", "group", g, "err", err)
			return true
		case s.now().Sub(last) > 24*time.Hour:
			return true
		}
	}
	return false
}

// anyFailed — была ли настоящая ошибка (ErrInProgress ошибкой не считается:
// группу в этот момент уже обновляет кто-то другой).
func anyFailed(results []Result) bool {
	for _, r := range results {
		if r.Err != nil && !errors.Is(r.Err, ErrInProgress) {
			return true
		}
	}
	return false
}

// NextRun — ближайший момент строго после now, когда в часовом поясе loc наступит
// одно из времён times. Считается через календарную дату в loc, поэтому корректен
// для любого пояса и не зависит от часового пояса сервера. ok=false, если times пуст.
func NextRun(now time.Time, times []schedule.Clock, loc *time.Location) (next time.Time, ok bool) {
	local := now.In(loc)
	for _, c := range times {
		h, m := int(c)/60, int(c)%60
		run := time.Date(local.Year(), local.Month(), local.Day(), h, m, 0, 0, loc)
		if !run.After(local) {
			run = time.Date(local.Year(), local.Month(), local.Day()+1, h, m, 0, 0, loc)
		}
		if !ok || run.Before(next) {
			next, ok = run, true
		}
	}
	return next, ok
}

// retryAt — когда повторить после failures предыдущих неудач подряд;
// не позже следующего планового запуска, если он есть.
func retryAt(now time.Time, failures int, retries []time.Duration, planned time.Time, hasPlanned bool) (time.Time, bool) {
	if len(retries) == 0 {
		return planned, hasPlanned
	}
	at := now.Add(retries[min(failures, len(retries)-1)])
	if hasPlanned && planned.Before(at) {
		return planned, true
	}
	return at, true
}

func sleepUntil(ctx context.Context, at time.Time, now func() time.Time) bool {
	return sleep(ctx, at.Sub(now()))
}

// sleep ждёт d или отмены ctx; false — ctx отменён.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
