package schedule

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
type Service struct {
	repo         Repository
	searcher     GroupSearcher
	defaultGroup Group
	loc          *time.Location
	now          func() time.Time
}

// NewService регистрирует группу по умолчанию в справочнике (ей нужен id для кнопок).
func NewService(ctx context.Context, repo Repository, searcher GroupSearcher, defaultGroup string, loc *time.Location) (*Service, error) {
	groups, err := repo.UpsertGroups(ctx, []string{defaultGroup})
	if err != nil {
		return nil, fmt.Errorf("register default group: %w", err)
	}
	return &Service{repo: repo, searcher: searcher, defaultGroup: groups[0], loc: loc, now: time.Now}, nil
}

func (s *Service) DefaultGroup() Group { return s.defaultGroup }

// Today — сегодняшняя дата в часовом поясе университета.
func (s *Service) Today() time.Time { return DateOf(s.now().In(s.loc)) }

// UserGroup — группа, выбранная пользователем, или группа по умолчанию.
func (s *Service) UserGroup(ctx context.Context, userID int64) (Group, error) {
	g, ok, err := s.repo.UserGroup(ctx, userID)
	if err != nil {
		return Group{}, fmt.Errorf("get user group: %w", err)
	}
	if !ok {
		return s.defaultGroup, nil
	}
	return g, nil
}

// SelectGroup запоминает выбор пользователя.
func (s *Service) SelectGroup(ctx context.Context, userID, groupID int64) (Group, error) {
	g, err := s.repo.GroupByID(ctx, groupID)
	if err != nil {
		return Group{}, err
	}
	if err := s.repo.SetUserGroup(ctx, userID, g.ID); err != nil {
		return Group{}, fmt.Errorf("set user group: %w", err)
	}
	return g, nil
}

func (s *Service) GroupByID(ctx context.Context, id int64) (Group, error) {
	return s.repo.GroupByID(ctx, id)
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
