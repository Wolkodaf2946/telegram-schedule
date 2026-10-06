// Package users — пользователи бота и доступ к нему: учёт новых пользователей,
// заявки на доступ и их одобрение администратором. Не знает про Telegram API.
package users

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

var ErrUserNotFound = errors.New("user not found")

// Status — статус доступа пользователя.
type Status string

const (
	StatusActive  Status = "active"  // доступ есть
	StatusPending Status = "pending" // ждёт одобрения администратора
	StatusBlocked Status = "blocked" // отклонён или отозван
)

// Mode — режим доступа к боту.
type Mode string

const (
	ModeOpen     Mode = "open"     // бот открыт для всех
	ModeApproval Mode = "approval" // новых пользователей одобряет администратор
)

// Profile — данные пользователя из Telegram.
type Profile struct {
	ID           int64
	Username     string
	FirstName    string
	LastName     string
	LanguageCode string
}

func (p Profile) DisplayName() string {
	name := p.FirstName
	if p.LastName != "" {
		name += " " + p.LastName
	}
	if name == "" {
		name = fmt.Sprintf("id%d", p.ID)
	}
	return name
}

// User — пользователь бота.
type User struct {
	Profile
	Status    Status
	GroupName string // пусто — группа не выбрана
	FirstSeen time.Time
	LastSeen  time.Time
}

type Repository interface {
	// TouchUser создаёт пользователя со статусом status или обновляет профиль и время
	// активности существующего (его статус не меняется). created — пользователь новый.
	TouchUser(ctx context.Context, p Profile, status Status) (u User, created bool, err error)
	SetUserStatus(ctx context.Context, id int64, status Status) (User, error) // ErrUserNotFound
	ListUsers(ctx context.Context, limit int) ([]User, error)
}

type Options struct {
	Mode        Mode
	Admins      map[int64]bool
	Preapproved map[int64]bool // доступ без заявки
	// Journal — отдельный журнал пользователей (новые, заявки, решения администратора).
	Journal *slog.Logger
}

// touchInterval — как часто обновлять профиль и last_seen в БД для активного
// пользователя. Чаще не нужно: это лишняя запись в БД на каждое нажатие кнопки.
const touchInterval = 10 * time.Minute

type Service struct {
	repo Repository
	opts Options
	now  func() time.Time

	mu    sync.Mutex
	cache map[int64]cachedUser
}

type cachedUser struct {
	user    User
	touched time.Time
}

func NewService(repo Repository, opts Options) *Service {
	if opts.Journal == nil {
		opts.Journal = slog.New(slog.DiscardHandler)
	}
	return &Service{repo: repo, opts: opts, now: time.Now, cache: make(map[int64]cachedUser)}
}

func (s *Service) Mode() Mode            { return s.opts.Mode }
func (s *Service) IsAdmin(id int64) bool { return s.opts.Admins[id] }

// Touch отмечает активность пользователя и возвращает его вместе со статусом доступа.
// created — пользователь написал боту впервые (уже записан в журнал).
func (s *Service) Touch(ctx context.Context, p Profile) (User, bool, error) {
	if u, ok := s.cached(p); ok {
		return u, false, nil
	}

	u, created, err := s.repo.TouchUser(ctx, p, s.initialStatus(p.ID))
	if err != nil {
		return User{}, false, fmt.Errorf("touch user: %w", err)
	}
	// Админов и пользователей из списка предодобренных пускаем, даже если раньше
	// они были в ожидании (например, список в конфиге пополнили позже).
	if u.Status != StatusActive && s.privileged(p.ID) {
		if u, err = s.repo.SetUserStatus(ctx, p.ID, StatusActive); err != nil {
			return User{}, false, fmt.Errorf("activate privileged user: %w", err)
		}
	}
	if created {
		s.journal("new user", u)
	}
	s.store(u)
	return u, created, nil
}

// SetStatus меняет доступ пользователя (решение администратора).
func (s *Service) SetStatus(ctx context.Context, adminID, userID int64, status Status) (User, error) {
	u, err := s.repo.SetUserStatus(ctx, userID, status)
	if err != nil {
		return User{}, err
	}
	s.store(u)
	s.journal("access changed", u, "by_admin", adminID)
	return u, nil
}

func (s *Service) List(ctx context.Context, limit int) ([]User, error) {
	return s.repo.ListUsers(ctx, limit)
}

func (s *Service) initialStatus(id int64) Status {
	if s.opts.Mode == ModeOpen || s.privileged(id) {
		return StatusActive
	}
	return StatusPending
}

func (s *Service) privileged(id int64) bool {
	return s.opts.Admins[id] || s.opts.Preapproved[id]
}

// cached возвращает пользователя без похода в БД, если его недавно видели
// и профиль в Telegram не менялся.
func (s *Service) cached(p Profile) (User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.cache[p.ID]
	if !ok || c.user.Profile != p || s.now().Sub(c.touched) > touchInterval {
		return User{}, false
	}
	return c.user, true
}

func (s *Service) store(u User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.cache) > 10_000 {
		clear(s.cache) // простая защита от неограниченного роста
	}
	s.cache[u.ID] = cachedUser{user: u, touched: s.now()}
}

func (s *Service) journal(event string, u User, extra ...any) {
	attrs := append([]any{
		"event", event,
		"user_id", u.ID,
		"username", u.Username,
		"first_name", u.FirstName,
		"last_name", u.LastName,
		"language_code", u.LanguageCode,
		"status", string(u.Status),
		"access_mode", string(s.opts.Mode),
	}, extra...)
	s.opts.Journal.Info("user", attrs...)
}
