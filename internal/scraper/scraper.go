// Package scraper забирает расписание группы с schedule.siriusuniversity.ru.
package scraper

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"

	"telegram-schedule/internal/schedule"
)

var (
	ErrGroupNotFound = errors.New("group not found on the schedule site")
	ErrCountMismatch = errors.New("parsed lessons count differs from the site counter")
)

// Month — расписание группы за один календарный месяц.
type Month struct {
	First   time.Time // первое число месяца, полночь UTC
	Lessons []schedule.Lesson
}

type Scraper struct {
	baseURL *url.URL
	client  *http.Client
	ua      string
	log     *slog.Logger
	dumpDir string
}

type Options struct {
	Timeout time.Duration
	Logger  *slog.Logger
	// DumpDir — куда сохранять сырой ответ сайта, если его не удалось разобрать.
	// Пусто — не сохранять.
	DumpDir string
}

const defaultUserAgent = "Mozilla/5.0 (X11; Linux x86_64; rv:150.0) Gecko/20100101 Firefox/150.0"

// New создаёт скрейпер. Прокси, если нужен, берётся из стандартных переменных
// окружения HTTPS_PROXY / NO_PROXY (http.ProxyFromEnvironment).
func New(baseURL string, opts Options) (*Scraper, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid base url %q", baseURL)
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	return &Scraper{
		baseURL: u,
		client:  &http.Client{Timeout: opts.Timeout, Transport: http.DefaultTransport},
		ua:      defaultUserAgent,
		log:     opts.Logger.With("component", "scraper"),
		dumpDir: opts.DumpDir,
	}, nil
}

// Fetch загружает расписание группы за текущий месяц и ещё monthsAhead следующих.
//
// Каждый вызов открывает новую сессию (свежий cookie jar): состояние сайта
// (выбранная группа, месяц) хранится в сессии, и переиспользование старой
// сессии могло бы начать обход не с текущего месяца.
func (s *Scraper) Fetch(ctx context.Context, group string, monthsAhead int) (months []Month, err error) {
	sess, err := s.newSession(ctx)
	defer func() { s.dumpOnError(sess, "fetch "+group, err) }()
	if err != nil {
		return nil, err
	}
	if err := sess.call(ctx, "set", group); err != nil {
		return nil, fmt.Errorf("select group: %w", err)
	}

	months = make([]Month, 0, monthsAhead+1)
	for i := 0; i <= monthsAhead; i++ {
		if i > 0 {
			if err := sess.call(ctx, "addMonth"); err != nil {
				return nil, fmt.Errorf("switch to next month: %w", err)
			}
		}
		m, err := readMonth(sess.comp, group)
		if err != nil {
			return nil, err
		}
		if i > 0 && !m.First.Equal(months[i-1].First.AddDate(0, 1, 0)) {
			return nil, fmt.Errorf("%w: expected month after %s, got %s",
				ErrUnexpectedReply, months[i-1].First.Format("2006-01"), m.First.Format("2006-01"))
		}
		months = append(months, m)
	}
	return months, nil
}

// MaxSearchResults — столько групп сайт показывает в выпадающем списке.
// Если найдено ровно столько, запрос стоит уточнить.
const MaxSearchResults = 20

// SearchGroups ищет группы по части названия так же, как поле поиска на сайте.
func (s *Scraper) SearchGroups(ctx context.Context, query string) (groups []string, err error) {
	sess, err := s.newSession(ctx)
	defer func() { s.dumpOnError(sess, "search "+query, err) }()
	if err != nil {
		return nil, err
	}
	if err := sess.input(ctx, "search", query); err != nil {
		return nil, fmt.Errorf("search groups: %w", err)
	}
	return parseGroups(sess.comp.html)
}

// newSession открывает страницу расписания в свежей сессии.
func (s *Scraper) newSession(ctx context.Context) (*session, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	client := *s.client
	client.Jar = jar
	sess := &session{http: &client, baseURL: s.baseURL, ua: s.ua, log: s.log}
	if err := sess.open(ctx); err != nil {
		return sess, fmt.Errorf("open schedule page: %w", err)
	}
	return sess, nil
}

// readMonth извлекает из текущего состояния компонента месяц и его занятия
// и сверяет их с тем, что сайт сам о себе сообщает.
func readMonth(c *component, group string) (Month, error) {
	data, err := c.data()
	if err != nil {
		return Month{}, err
	}

	var shownGroup string
	if err := data.decodeKey("group", &shownGroup); err != nil {
		return Month{}, fmt.Errorf("%w: %v", ErrUnexpectedReply, err)
	}
	if shownGroup != group {
		return Month{}, fmt.Errorf("%w: %q (site shows %q)", ErrGroupNotFound, group, shownGroup)
	}

	var (
		year  stringOrNumber
		month struct {
			Number stringOrNumber `json:"number"`
		}
		count int
	)
	if err := data.decodeKey("year", &year); err != nil {
		return Month{}, fmt.Errorf("%w: %v", ErrUnexpectedReply, err)
	}
	if err := data.decodeKey("month", &month); err != nil {
		return Month{}, fmt.Errorf("%w: %v", ErrUnexpectedReply, err)
	}
	if err := data.decodeKey("count", &count); err != nil {
		return Month{}, fmt.Errorf("%w: %v", ErrUnexpectedReply, err)
	}
	y, errY := year.Int()
	m, errM := month.Number.Int()
	if errY != nil || errM != nil || m < 1 || m > 12 {
		return Month{}, fmt.Errorf("%w: bad month %q/%q", ErrUnexpectedReply, year, month.Number)
	}
	first := time.Date(y, time.Month(m), 1, 0, 0, 0, 0, time.UTC)

	lessons, err := ParseLessons(c.html, group)
	if err != nil {
		return Month{}, fmt.Errorf("parse %s: %w", first.Format("2006-01"), err)
	}
	if len(lessons) != count {
		return Month{}, fmt.Errorf("%w: %s: parsed %d, site says %d",
			ErrCountMismatch, first.Format("2006-01"), len(lessons), count)
	}
	from, to := schedule.MonthRange(first)
	for _, l := range lessons {
		if l.Date.Before(from) || l.Date.After(to) {
			return Month{}, fmt.Errorf("%w: lesson on %s outside of %s",
				ErrUnexpectedReply, l.Date.Format(time.DateOnly), first.Format("2006-01"))
		}
	}
	return Month{First: first, Lessons: lessons}, nil
}
