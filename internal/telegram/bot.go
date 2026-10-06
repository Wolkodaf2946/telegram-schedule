// Package telegram — транспортный слой: команды, кнопки, тексты сообщений.
// Всё, что касается Telegram API, живёт только здесь; сервисы получают и
// возвращают доменные типы.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"sync"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"telegram-schedule/internal/logging"
	"telegram-schedule/internal/schedule"
	"telegram-schedule/internal/syncer"
	"telegram-schedule/internal/users"
)

const (
	btnToday    = "📅 Сегодня"
	btnTomorrow = "➡️ Завтра"
	btnCalendar = "📆 Календарь"
	btnGroup    = "👥 Группа"

	textError = "😔 Что-то пошло не так. Попробуйте ещё раз чуть позже."
)

const (
	// pollTimeout — long polling getUpdates (Telegram держит запрос до pollTimeout-1с).
	pollTimeout = 50 * time.Second
	// httpTimeout с запасом больше pollTimeout: у библиотеки по умолчанию запас всего
	// в секунду, и на медленной сети пустой long poll превращается в ложную ошибку.
	httpTimeout = pollTimeout + 20*time.Second
	// initTimeout — на проверку токена (getMe) при старте; по умолчанию у библиотеки 5 с.
	initTimeout = 30 * time.Second
)

// ErrAPIUnavailable — не удалось связаться с Telegram Bot API при старте
// (сеть, блокировка, прокси).
var ErrAPIUnavailable = errors.New("telegram bot api is unavailable")

// ErrInvalidToken — Telegram отклонил TELEGRAM_TOKEN.
var ErrInvalidToken = errors.New("telegram rejected the bot token (check TELEGRAM_TOKEN)")

// newHTTPClient — клиент для Telegram. Прокси из TELEGRAM_PROXY действует только здесь,
// поэтому сайт университета можно продолжать открывать напрямую.
//
// Каждый запрос к Telegram через медленный прокси стоит сотни миллисекунд, а новое
// TLS-соединение — ещё несколько обменов сверху. Поэтому соединения держатся открытыми
// долго, а HTTP/2 позволяет отправлять ответы по тому же соединению, на котором
// висит long polling, — без нового рукопожатия на каждое сообщение.
func newHTTPClient(proxy *url.URL) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxy != nil {
		tr.Proxy = http.ProxyURL(proxy)
	}
	tr.ForceAttemptHTTP2 = true
	tr.MaxIdleConnsPerHost = 16
	tr.IdleConnTimeout = 15 * time.Minute
	return &http.Client{Timeout: httpTimeout, Transport: tr}
}

// ScheduleService — то, что боту нужно от доменного сервиса расписания.
type ScheduleService interface {
	Today() time.Time

	UserGroup(ctx context.Context, userID int64) (g schedule.Group, ok bool, err error)
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

// UsersService — учёт пользователей и доступ к боту.
type UsersService interface {
	Mode() users.Mode
	IsAdmin(id int64) bool
	Touch(ctx context.Context, p users.Profile) (u users.User, created bool, err error)
	SetStatus(ctx context.Context, adminID, userID int64, status users.Status) (users.User, error)
	List(ctx context.Context, limit int) ([]users.User, error)
}

type Options struct {
	Token  string
	APIURL string   // адрес Bot API; пусто — https://api.telegram.org
	Proxy  *url.URL // прокси только для Telegram; nil — HTTPS_PROXY из окружения или напрямую

	Location *time.Location
	AdminIDs map[int64]bool // кому слать заявки, «Бот запущен» и уведомления о новых пользователях

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
	users    UsersService
	opts     Options
	log      *slog.Logger
	inflight sync.WaitGroup
	search   *rateLimiter
}

func New(svc ScheduleService, syn Syncer, usr UsersService, opts Options, log *slog.Logger) (*Bot, error) {
	b := &Bot{
		svc:    svc,
		sync:   syn,
		users:  usr,
		opts:   opts,
		log:    log.With("component", "telegram"),
		search: newRateLimiter(3 * time.Second), // каждый поиск — запрос к сайту университета
	}

	tgOpts := []tg.Option{
		tg.WithMiddlewares(b.trackInflight, b.logUpdate, b.recoverPanic, b.checkAccess),
		tg.WithDefaultHandler(b.handleText),
		tg.WithErrorsHandler(func(err error) { b.log.Error("telegram polling", "err", err) }),
		tg.WithAllowedUpdates(tg.AllowedUpdates{"message", "callback_query"}),
		tg.WithHTTPClient(pollTimeout, newHTTPClient(opts.Proxy)),
		tg.WithCheckInitTimeout(initTimeout),
	}
	if opts.APIURL != "" {
		tgOpts = append(tgOpts, tg.WithServerURL(opts.APIURL))
	}
	api, err := tg.New(opts.Token, tgOpts...)
	switch {
	case errors.Is(err, tg.ErrorUnauthorized), errors.Is(err, tg.ErrorNotFound):
		// 401/404 на getMe — неверный токен: повторять бессмысленно.
		return nil, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	case err != nil:
		return nil, fmt.Errorf("%w: %w", ErrAPIUnavailable, err)
	}
	b.api = api

	for cmd, h := range map[string]tg.HandlerFunc{
		"start":    b.handleStart,
		"help":     b.handleHelp,
		"today":    b.handleToday,
		"tomorrow": b.handleTomorrow,
		"calendar": b.handleCalendar,
		"group":    b.handleGroup,
		"refresh":  b.adminOnly(b.handleRefresh),
		"users":    b.adminOnly(b.handleUsers),
		"allow":    b.adminOnly(b.handleAllow),
		"revoke":   b.adminOnly(b.handleRevoke),
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
		{Command: "group", Description: "Выбрать группу"},
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

// checkAccess учитывает пользователя (новых — в журнал и администраторам) и пропускает
// к обработчикам только тех, у кого есть доступ. В открытом режиме доступ есть у всех.
func (b *Bot) checkAccess(next tg.HandlerFunc) tg.HandlerFunc {
	return func(ctx context.Context, api *tg.Bot, u *models.Update) {
		p, ok := profileOf(u)
		if !ok {
			return // апдейт без отправителя (например, от канала) — не наш случай
		}
		user, created, err := b.users.Touch(ctx, p)
		if err != nil {
			b.logger(ctx).Error("touch user", "err", err)
			if b.users.Mode() == users.ModeOpen {
				next(ctx, api, u) // открытый бот не должен ломаться из-за журнала
			} else {
				b.reply(ctx, u, textError)
			}
			return
		}
		if created {
			b.goTracked(func() { b.announceNewUser(context.WithoutCancel(ctx), user) })
		}

		switch user.Status {
		case users.StatusActive:
			next(ctx, api, u)
		case users.StatusPending:
			b.logger(ctx).Info("access pending")
			b.reply(ctx, u, "⏳ Заявка на доступ отправлена администратору. Как только её одобрят, я напишу.")
		default:
			b.logger(ctx).Info("access denied", "status", string(user.Status))
			b.reply(ctx, u, "⛔ Доступ к боту закрыт.")
		}
	}
}

// adminOnly пропускает к обработчику только администраторов.
func (b *Bot) adminOnly(h tg.HandlerFunc) tg.HandlerFunc {
	return func(ctx context.Context, api *tg.Bot, u *models.Update) {
		if !b.users.IsAdmin(senderID(u)) {
			b.reply(ctx, u, "Эта команда доступна только администратору.")
			return
		}
		h(ctx, api, u)
	}
}

// goTracked запускает фоновую работу так, чтобы остановка бота её дождалась.
func (b *Bot) goTracked(f func()) {
	b.inflight.Add(1)
	go func() {
		defer b.inflight.Done()
		f()
	}()
}

// reply — короткий ответ на апдейт: всплывашка для кнопки, сообщение для текста.
func (b *Bot) reply(ctx context.Context, u *models.Update, text string) {
	switch {
	case u.CallbackQuery != nil:
		b.answerCallback(ctx, u.CallbackQuery.ID, text)
	case u.Message != nil:
		b.send(ctx, u.Message.Chat.ID, text, nil)
	}
}

func profileOf(u *models.Update) (users.Profile, bool) {
	var from *models.User
	switch {
	case u.Message != nil:
		from = u.Message.From
	case u.CallbackQuery != nil:
		from = &u.CallbackQuery.From
	}
	if from == nil || from.ID == 0 {
		return users.Profile{}, false
	}
	return users.Profile{
		ID: from.ID, Username: from.Username, FirstName: from.FirstName,
		LastName: from.LastName, LanguageCode: from.LanguageCode,
	}, true
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
