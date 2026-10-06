package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	// ErrNotSynced — расписание группы ещё ни разу успешно не загружалось.
	ErrNotSynced = errors.New("schedule has never been synced")
	// ErrGroupNotFound — группы с таким id нет в справочнике.
	ErrGroupNotFound = errors.New("group not found")
	// ErrQueryTooShort — слишком короткий поисковый запрос.
	ErrQueryTooShort = errors.New("search query is too short")
)

// MinQueryLen — минимальная длина запроса поиска группы (в символах).
const MinQueryLen = 2

// Group — учебная группа из справочника.
type Group struct {
	ID   int64
	Name string
}

// Coverage — диапазон дат, который хотя бы раз успешно загружался с сайта,
// и время последней успешной загрузки.
type Coverage struct {
	From, To  time.Time
	UpdatedAt time.Time
}

func (c Coverage) Contains(date time.Time) bool {
	return !date.Before(c.From) && !date.After(c.To)
}

// SyncRun — запись журнала синхронизаций.
type SyncRun struct {
	Group      string
	StartedAt  time.Time
	FinishedAt time.Time
	From, To   time.Time // нулевые для неуспешной синхронизации
	Lessons    int
	Err        error
}

// Repository — то, что сервису нужно от хранилища.
type Repository interface {
	LessonsOn(ctx context.Context, group string, date time.Time) ([]Lesson, error)
	DatesWithLessons(ctx context.Context, group string, from, to time.Time) ([]time.Time, error)
	Coverage(ctx context.Context, group string) (Coverage, error) // ErrNotSynced, если загрузок не было

	UpsertGroups(ctx context.Context, names []string) ([]Group, error) // в порядке names
	GroupByID(ctx context.Context, id int64) (Group, error)            // ErrGroupNotFound
	UserGroup(ctx context.Context, userID int64) (Group, bool, error)  // false — пользователь не выбирал группу
	SetUserGroup(ctx context.Context, userID, groupID int64) error
}

// GroupSearcher ищет группы на сайте расписания.
type GroupSearcher interface {
	SearchGroups(ctx context.Context, query string) ([]string, error)
}

// Day — расписание на один день.
type Day struct {
	Group   Group
	Date    time.Time
	Lessons []Lesson // отсортированы по времени начала
	// Known — дата входит в загруженный диапазон. Если false, пустой Lessons означает
	// «данных нет», а не «пар нет».
	Known     bool
	UpdatedAt time.Time // нулевое, если загрузок не было
}

// Service — расписание и выбор групп. Принимает и возвращает доменные типы:
// построение сообщений и клавиатур живёт в транспортном слое.
//
// Группы и выбор пользователей кешируются в памяти: это справочные данные, которые
// меняются только через этот же сервис, а запрашиваются на каждое нажатие кнопки.
type Service struct {
	repo     Repository
	searcher GroupSearcher
	loc      *time.Location
	now      func() time.Time

	mu         sync.RWMutex
	groups     map[int64]Group     // id -> группа; группы неизменяемы
	userGroups map[int64]userGroup // выбор пользователя, включая «не выбрано»
}

type userGroup struct {
	group Group
	ok    bool
}

func NewService(repo Repository, searcher GroupSearcher, loc *time.Location) *Service {
	return &Service{
		repo: repo, searcher: searcher, loc: loc, now: time.Now,
		groups:     make(map[int64]Group),
		userGroups: make(map[int64]userGroup),
	}
}

// Today — сегодняшняя дата в часовом поясе университета.
func (s *Service) Today() time.Time { return DateOf(s.now().In(s.loc)) }

// UserGroup — группа, выбранная пользователем; ok=false, если он ещё не выбирал.
func (s *Service) UserGroup(ctx context.Context, userID int64) (g Group, ok bool, err error) {
	s.mu.RLock()
	cached, hit := s.userGroups[userID]
	s.mu.RUnlock()
	if hit {
		return cached.group, cached.ok, nil
	}

	g, ok, err = s.repo.UserGroup(ctx, userID)
	if err != nil {
		return Group{}, false, fmt.Errorf("get user group: %w", err)
	}
	s.mu.Lock()
	s.userGroups[userID] = userGroup{group: g, ok: ok}
	s.mu.Unlock()
	return g, ok, nil
}

// SelectGroup запоминает выбор пользователя.
func (s *Service) SelectGroup(ctx context.Context, userID, groupID int64) (Group, error) {
	g, err := s.GroupByID(ctx, groupID)
	if err != nil {
		return Group{}, err
	}
	if err := s.repo.SetUserGroup(ctx, userID, g.ID); err != nil {
		return Group{}, fmt.Errorf("set user group: %w", err)
	}
	s.mu.Lock()
	s.userGroups[userID] = userGroup{group: g, ok: true}
	s.mu.Unlock()
	return g, nil
}

func (s *Service) GroupByID(ctx context.Context, id int64) (Group, error) {
	s.mu.RLock()
	g, ok := s.groups[id]
	s.mu.RUnlock()
	if ok {
		return g, nil
	}

	g, err := s.repo.GroupByID(ctx, id)
	if err != nil {
		return Group{}, err
	}
	s.rememberGroups(g)
	return g, nil
}

func (s *Service) rememberGroups(groups ...Group) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, g := range groups {
		if g.ID != 0 {
			s.groups[g.ID] = g
		}
	}
}

// SearchGroups ищет группы на сайте и заносит найденные в справочник.
func (s *Service) SearchGroups(ctx context.Context, query string) ([]Group, error) {
	query = strings.Join(strings.Fields(query), " ")
	if utf8.RuneCountInString(query) < MinQueryLen {
		return nil, ErrQueryTooShort
	}
	names, err := s.searcher.SearchGroups(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("search on site: %w", err)
	}
	if len(names) == 0 {
		return nil, nil
	}
	groups, err := s.repo.UpsertGroups(ctx, names)
	if err != nil {
		return nil, fmt.Errorf("save groups: %w", err)
	}
	s.rememberGroups(groups...)
	return groups, nil
}

func (s *Service) Day(ctx context.Context, g Group, date time.Time) (Day, error) {
	date = DateOf(date)
	day := Day{Group: g, Date: date}

	cov, err := s.repo.Coverage(ctx, g.Name)
	switch {
	case errors.Is(err, ErrNotSynced):
		return day, nil
	case err != nil:
		return Day{}, fmt.Errorf("get coverage: %w", err)
	}
	day.Known = cov.Contains(date)
	day.UpdatedAt = cov.UpdatedAt

	day.Lessons, err = s.repo.LessonsOn(ctx, g.Name, date)
	if err != nil {
		return Day{}, fmt.Errorf("get lessons on %s: %w", date.Format(time.DateOnly), err)
	}
	return day, nil
}

// Status — какой диапазон дат загружен и когда было последнее обновление.
// ErrNotSynced, если загрузок ещё не было.
func (s *Service) Status(ctx context.Context, g Group) (Coverage, error) {
	return s.repo.Coverage(ctx, g.Name)
}

// MonthDays возвращает множество дней месяца (1..31), в которые есть занятия.
func (s *Service) MonthDays(ctx context.Context, g Group, month time.Time) (map[int]bool, error) {
	from, to := MonthRange(month)
	dates, err := s.repo.DatesWithLessons(ctx, g.Name, from, to)
	if err != nil {
		return nil, fmt.Errorf("get dates with lessons: %w", err)
	}
	days := make(map[int]bool, len(dates))
	for _, d := range dates {
		days[d.Day()] = true
	}
	return days, nil
}
