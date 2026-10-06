// Package telegram — транспортный слой: команды, кнопки, тексты сообщений.
// Всё, что касается Telegram API, живёт только здесь; сервисы получают и
// возвращают доменные типы.
package telegram

import (
	"context"
	"fmt"
	"html"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"telegram-schedule/internal/logging"
	"telegram-schedule/internal/schedule"
	"telegram-schedule/internal/syncer"
)

const (
	btnToday    = "📅 Сегодня"
	btnTomorrow = "➡️ Завтра"
	btnCalendar = "📆 Календарь"
	btnGroup    = "👥 Группа"

	textError = "😔 Что-то пошло не так. Попробуйте ещё раз чуть позже."
)

// ScheduleService — то, что боту нужно от доменного сервиса.
type ScheduleService interface {
	DefaultGroup() schedule.Group
	Today() time.Time

	UserGroup(ctx context.Context, userID int64) (schedule.Group, error)
	SelectGroup(ctx context.Context, userID, groupID int64) (schedule.Group, error)
	GroupByID(ctx context.Context, id int64) (schedule.Group, error)
	SearchGroups(ctx context.Context, query string) ([]schedule.Group, error)

	Day(ctx context.Context, g schedule.Group, date time.Time) (schedule.Day, error)
	MonthDays(ctx context.Context, g schedule.Group, month time.Time) (map[int]bool, error)
	Status(ctx context.Context, g schedule.Group) (schedule.Coverage, error)
}

// Syncer запускает загрузку расписания с сайта.
type Syncer interface {
	Groups(ctx context.Context) ([]string, error)
	RunAll(ctx context.Context) ([]syncer.Result, error)
	RunGroup(ctx context.Context, group string) syncer.Result
}

type Options struct {
	Token      string
	Location   *time.Location
	AdminIDs   map[int64]bool
	AllowedIDs map[int64]bool // пусто — бот открыт для всех

	// StartupMessage — при запуске написать администраторам, что бот работает.
	// Только им: рассылка всем пользователям на каждый рестарт — это спам
	// (и при краш-лупе — блокировка от Telegram).
	StartupMessage bool
	SyncTimes      []schedule.Clock // для текста стартового сообщения
}

type Bot struct {
	api      *tg.Bot
	svc      ScheduleService
	sync     Syncer
	opts     Options
	log      *slog.Logger
	inflight sync.WaitGroup
	search   *rateLimiter
}

func New(svc ScheduleService, syn Syncer, opts Options, log *slog.Logger) (*Bot, error) {
	b := &Bot{
		svc:    svc,
		sync:   syn,
		opts:   opts,
		log:    log.With("component", "telegram"),
		search: newRateLimiter(3 * time.Second), // каждый поиск — запрос к сайту университета
	}

	api, err := tg.New(opts.Token,
		tg.WithMiddlewares(b.trackInflight, b.logUpdate, b.recoverPanic, b.restrictAccess),
		tg.WithDefaultHandler(b.handleText),
		tg.WithErrorsHandler(func(err error) { b.log.Error("telegram polling", "err", err) }),
		tg.WithAllowedUpdates(tg.AllowedUpdates{"message", "callback_query"}),
	)
	if err != nil {
		return nil, fmt.Errorf("init telegram bot: %w", err)
	}
	b.api = api

	for cmd, h := range map[string]tg.HandlerFunc{
		"start":    b.handleStart,
		"help":     b.handleHelp,
		"today":    b.handleToday,
		"tomorrow": b.handleTomorrow,
		"calendar": b.handleCalendar,
		"group":    b.handleGroup,
		"refresh":  b.handleRefresh,
	} {
		// CommandStartOnly, а не Command: последний режет текст по UTF-16-смещениям
		// из entities как по байтам и ошибается, если перед командой есть кириллица.
		api.RegisterHandler(tg.HandlerTypeMessageText, cmd, tg.MatchTypeCommandStartOnly, h)
	}
	api.RegisterHandler(tg.HandlerTypeMessageText, btnToday, tg.MatchTypeExact, b.handleToday)
	api.RegisterHandler(tg.HandlerTypeMessageText, btnTomorrow, tg.MatchTypeExact, b.handleTomorrow)
	api.RegisterHandler(tg.HandlerTypeMessageText, btnCalendar, tg.MatchTypeExact, b.handleCalendar)
	api.RegisterHandler(tg.HandlerTypeMessageText, btnGroup, tg.MatchTypeExact, b.handleGroup)
	api.RegisterHandler(tg.HandlerTypeCallbackQueryData, "", tg.MatchTypePrefix, b.handleCallback)

	return b, nil
}

// Run принимает апдейты до отмены ctx, затем дожидается уже начатых обработчиков
// (не дольше shutdownTimeout), чтобы не оборвать ответ на полуслове.
func (b *Bot) Run(ctx context.Context, shutdownTimeout time.Duration) {
	b.setCommands(ctx)
	if b.opts.StartupMessage {
		b.notifyAdmins(ctx, b.startupText(ctx))
	}
	b.log.Info("bot started")
	b.api.Start(ctx) // блокируется до отмены ctx

	done := make(chan struct{})
	go func() { b.inflight.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		b.log.Warn("shutdown timeout: some handlers are still running")
	}
	b.log.Info("bot stopped")
}

func (b *Bot) setCommands(ctx context.Context) {
	_, err := b.api.SetMyCommands(ctx, &tg.SetMyCommandsParams{Commands: []models.BotCommand{
		{Command: "today", Description: "Расписание на сегодня"},
		{Command: "tomorrow", Description: "Расписание на завтра"},
		{Command: "calendar", Description: "Выбрать день в календаре"},
		{Command: "group", Description: "Сменить группу"},
		{Command: "help", Description: "Что умеет бот"},
	}})
	if err != nil {
		b.log.Warn("set bot commands", "err", err)
	}
}

func (b *Bot) notifyAdmins(ctx context.Context, text string) {
	for id := range b.opts.AdminIDs {
		b.send(ctx, id, text, nil)
	}
}

// --- middleware ---

// trackInflight учитывает работающие обработчики для graceful shutdown.
// go-telegram/bot запускает каждый обработчик в своей горутине, поэтому
// медленный запрос одного пользователя не блокирует остальных.
func (b *Bot) trackInflight(next tg.HandlerFunc) tg.HandlerFunc {
	return func(ctx context.Context, api *tg.Bot, u *models.Update) {
		b.inflight.Add(1)
		defer b.inflight.Done()
		next(ctx, api, u)
	}
}

// logUpdate пишет в лог каждый апдейт: кто, что прислал, сколько обрабатывалось.
// Логгер с этими полями кладётся в ctx, поэтому любая ошибка внутри обработчика
// попадает в лог вместе с update_id, user_id и тем, что пользователь отправил.
func (b *Bot) logUpdate(next tg.HandlerFunc) tg.HandlerFunc {
	return func(ctx context.Context, api *tg.Bot, u *models.Update) {
		l := b.log.With(updateAttrs(u)...)
		ctx = logging.WithLogger(ctx, l)

		started := time.Now()
		next(ctx, api, u)
		l.Info("update handled", "duration_ms", time.Since(started).Milliseconds())
	}
}

// updateAttrs — поля лога, описывающие апдейт.
func updateAttrs(u *models.Update) []any {
	attrs := []any{"update_id", u.ID}
	var from *models.User
	switch {
	case u.Message != nil:
		from = u.Message.From
		attrs = append(attrs, "chat_id", u.Message.Chat.ID, "kind", "message", "text", truncate(u.Message.Text, 200))
	case u.CallbackQuery != nil:
		from = &u.CallbackQuery.From
		attrs = append(attrs, "chat_id", chatOf(u.CallbackQuery.Message), "kind", "callback", "data", u.CallbackQuery.Data)
	default:
		attrs = append(attrs, "kind", "other")
	}
	if from != nil {
		attrs = append(attrs, "user_id", from.ID)
		if from.Username != "" {
			attrs = append(attrs, "username", from.Username)
		}
	}
	return attrs
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// logger — логгер текущего апдейта (с его полями) или общий логгер бота.
func (b *Bot) logger(ctx context.Context) *slog.Logger {
	return logging.From(ctx, b.log)
}

// recoverPanic — паника в обработчике одного апдейта не роняет весь процесс.
func (b *Bot) recoverPanic(next tg.HandlerFunc) tg.HandlerFunc {
	return func(ctx context.Context, api *tg.Bot, u *models.Update) {
		defer func() {
			if r := recover(); r != nil {
				b.logger(ctx).Error("panic in handler", "panic", r, "stack", string(debug.Stack()))
			}
		}()
		next(ctx, api, u)
	}
}

// restrictAccess пропускает только пользователей из ALLOWED_USER_IDS, если он задан.
func (b *Bot) restrictAccess(next tg.HandlerFunc) tg.HandlerFunc {
	return func(ctx context.Context, api *tg.Bot, u *models.Update) {
		if len(b.opts.AllowedIDs) == 0 {
			next(ctx, api, u)
			return
		}
		userID := senderID(u)
		if b.opts.AllowedIDs[userID] {
			next(ctx, api, u)
			return
		}
		b.logger(ctx).Warn("access denied")
		if u.CallbackQuery != nil {
			b.answerCallback(ctx, u.CallbackQuery.ID, "Нет доступа")
		}
	}
}

// --- helpers ---

func mainKeyboard() *models.ReplyKeyboardMarkup {
	return &models.ReplyKeyboardMarkup{
		Keyboard: [][]models.KeyboardButton{
			{{Text: btnToday}, {Text: btnTomorrow}},
			{{Text: btnCalendar}, {Text: btnGroup}},
		},
		ResizeKeyboard: true,
		IsPersistent:   true,
	}
}

// senderID — пользователь, от которого пришёл апдейт. Данные пользователя
// (выбранная группа) привязаны к нему, а не к чату: в группе у каждого своя.
func senderID(u *models.Update) int64 {
	switch {
	case u.Message != nil && u.Message.From != nil:
		return u.Message.From.ID
	case u.CallbackQuery != nil:
		return u.CallbackQuery.From.ID
	}
	return 0
}

func escape(s string) string { return html.EscapeString(s) }

// rateLimiter — не чаще одного действия в interval на пользователя.
type rateLimiter struct {
	interval time.Duration
	mu       sync.Mutex
	last     map[int64]time.Time
}

func newRateLimiter(interval time.Duration) *rateLimiter {
	return &rateLimiter{interval: interval, last: make(map[int64]time.Time)}
}

func (r *rateLimiter) Allow(userID int64, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if now.Sub(r.last[userID]) < r.interval {
		return false
	}
	r.last[userID] = now
	if len(r.last) > 10_000 { // не даём карте расти бесконечно
		for id, t := range r.last {
			if now.Sub(t) >= r.interval {
				delete(r.last, id)
			}
		}
	}
	return true
}
