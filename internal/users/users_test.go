package users

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type fakeRepo struct {
	users   map[int64]User
	touches int
}

func newFakeRepo() *fakeRepo { return &fakeRepo{users: map[int64]User{}} }

func (f *fakeRepo) TouchUser(_ context.Context, p Profile, status Status) (User, bool, error) {
	f.touches++
	u, ok := f.users[p.ID]
	if !ok {
		u = User{Status: status}
	}
	u.Profile = p
	f.users[p.ID] = u
	return u, !ok, nil
}

func (f *fakeRepo) SetUserStatus(_ context.Context, id int64, s Status) (User, error) {
	u, ok := f.users[id]
	if !ok {
		return User{}, ErrUserNotFound
	}
	u.Status = s
	f.users[id] = u
	return u, nil
}

func (f *fakeRepo) ListUsers(context.Context, int) ([]User, error) { return nil, nil }

func newTestService(repo Repository, mode Mode, journal *bytes.Buffer) *Service {
	return NewService(repo, Options{
		Mode:        mode,
		Admins:      map[int64]bool{1: true},
		Preapproved: map[int64]bool{2: true},
		Journal:     slog.New(slog.NewJSONHandler(journal, nil)),
	})
}

func TestTouch_OpenMode(t *testing.T) {
	var journal bytes.Buffer
	s := newTestService(newFakeRepo(), ModeOpen, &journal)

	u, created, err := s.Touch(context.Background(), Profile{ID: 10, Username: "student", FirstName: "Иван"})
	if err != nil || !created || u.Status != StatusActive {
		t.Fatalf("open mode: %+v created=%v err=%v", u, created, err)
	}

	var rec map[string]any
	if err := json.Unmarshal(journal.Bytes(), &rec); err != nil {
		t.Fatalf("journal is not JSON: %q", journal.String())
	}
	if rec["event"] != "new user" || rec["user_id"] != float64(10) || rec["username"] != "student" || rec["first_name"] != "Иван" {
		t.Errorf("journal record = %v", rec)
	}
}

func TestTouch_ApprovalMode(t *testing.T) {
	var journal bytes.Buffer
	repo := newFakeRepo()
	s := newTestService(repo, ModeApproval, &journal)
	ctx := context.Background()

	if u, _, _ := s.Touch(ctx, Profile{ID: 10}); u.Status != StatusPending {
		t.Errorf("stranger must wait for approval, got %s", u.Status)
	}
	if u, _, _ := s.Touch(ctx, Profile{ID: 1}); u.Status != StatusActive {
		t.Errorf("admin must have access, got %s", u.Status)
	}
	if u, _, _ := s.Touch(ctx, Profile{ID: 2}); u.Status != StatusActive {
		t.Errorf("preapproved user must have access, got %s", u.Status)
	}

	// Одобрение видно сразу, хотя пользователь закеширован как ожидающий.
	if _, err := s.SetStatus(ctx, 1, 10, StatusActive); err != nil {
		t.Fatal(err)
	}
	if u, created, _ := s.Touch(ctx, Profile{ID: 10}); u.Status != StatusActive || created {
		t.Errorf("after approval: %+v created=%v", u, created)
	}
	if !strings.Contains(journal.String(), `"event":"access changed"`) || !strings.Contains(journal.String(), `"by_admin":1`) {
		t.Errorf("access change must be journaled:\n%s", journal.String())
	}
}

func TestTouch_PromotesPreapprovedPending(t *testing.T) {
	// Пользователь подал заявку, а потом его добавили в ALLOWED_USER_IDS.
	repo := newFakeRepo()
	repo.users[2] = User{Profile: Profile{ID: 2}, Status: StatusPending}
	s := newTestService(repo, ModeApproval, &bytes.Buffer{})
	if u, _, _ := s.Touch(context.Background(), Profile{ID: 2}); u.Status != StatusActive {
		t.Errorf("preapproved pending user must be activated, got %s", u.Status)
	}
}

func TestTouch_CachesRecentUsers(t *testing.T) {
	repo := newFakeRepo()
	s := newTestService(repo, ModeOpen, &bytes.Buffer{})
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	ctx := context.Background()
	p := Profile{ID: 10, Username: "a"}

	for range 5 {
		_, _, _ = s.Touch(ctx, p)
	}
	if repo.touches != 1 {
		t.Errorf("repeated touches within interval hit DB %d times, want 1", repo.touches)
	}

	p.Username = "renamed" // сменил ник — профиль нужно обновить
	_, _, _ = s.Touch(ctx, p)
	now = now.Add(touchInterval + time.Second)
	_, _, _ = s.Touch(ctx, p)
	if repo.touches != 3 {
		t.Errorf("profile change and expired interval must hit DB, got %d touches", repo.touches)
	}
}

func TestDisplayName(t *testing.T) {
	for p, want := range map[Profile]string{
		{ID: 1, FirstName: "Иван", LastName: "Петров"}: "Иван Петров",
		{ID: 1, FirstName: "Иван"}:                     "Иван",
		{ID: 42}:                                       "id42",
	} {
		if got := p.DisplayName(); got != want {
			t.Errorf("DisplayName(%+v) = %q, want %q", p, got, want)
		}
	}
}
