package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"telegram-schedule/internal/schedule"
	"telegram-schedule/internal/scraper"
	"telegram-schedule/internal/syncer"
	"telegram-schedule/internal/users"
)

const textChooseGroup = "👥 <b>Выберите свою группу</b>: напишите мне часть её названия, " +
	"например <code>ИОП-ИТ</code> или <code>ББ-25</code>, — я найду её на сайте университета."

// Про скорость. Каждый запрос к Telegram (особенно через прокси) — заметная задержка,
// поэтому обработчики делают их как можно меньше и не последовательно:
//   - на /start, «Сегодня», «Завтра», «Календарь» — ровно одно сообщение;
//   - на кнопку — ответ на callback уходит параллельно с редактированием сообщения;
//   - выбор группы превращает сообщение со списком групп сразу в расписание.

// handleStart — приветствие. Новому пользователю — просьба выбрать группу,
// вернувшемуся — напоминание, какая группа выбрана. Всегда одно сообщение.
func (b *Bot) handleStart(ctx context.Context, _ *tg.Bot, u *models.Update) {
	chatID := u.Message.Chat.ID
	g, ok, err := b.svc.UserGroup(ctx, senderID(u))
	if err != nil {
		b.fail(ctx, chatID, "get user group", err)
		return
	}

	name := ""
	if u.Message.From != nil && u.Message.From.FirstName != "" {
		name = ", " + escape(u.Message.From.FirstName)
	}
	intro := fmt.Sprintf("👋 Привет%s! Я показываю расписание Университета «Сириус»: "+
		"на сегодня, на завтра и на любой день через календарь.\n\n", name)

	if !ok {
		b.send(ctx, chatID, intro+textChooseGroup, mainKeyboard())
		return
	}
	b.send(ctx, chatID, intro+fmt.Sprintf(
		"Ваша группа: <b>%s</b>.\n\n"+
			"• %s и %s — пары на день, под расписанием кнопки ◀ ▶\n"+
			"• %s — любой день месяца\n"+
			"• %s — сменить группу",
		escape(g.Name), btnToday, btnTomorrow, btnCalendar, btnGroup), mainKeyboard())
}

func (b *Bot) handleHelp(ctx context.Context, _ *tg.Bot, u *models.Update) {
	text := "<b>Команды</b>\n" +
		"/today — расписание на сегодня\n" +
		"/tomorrow — на завтра\n" +
		"/calendar — календарь на месяц\n" +
		"/group — выбрать группу\n\n" +
		"Чтобы найти группу, просто напишите часть её названия, например <code>ББ-25</code>.\n\n" +
		"В календаре цифрой отмечены дни с парами, точкой — свободные, [в скобках] — сегодня."
	if b.users.IsAdmin(senderID(u)) {
		text += "\n\n<b>Администратору</b>\n" +
			"/users — пользователи и заявки\n" +
			"/allow &lt;id&gt; — открыть доступ, /revoke &lt;id&gt; — закрыть\n" +
			"/refresh — обновить расписание сейчас"
	}
	b.send(ctx, u.Message.Chat.ID, text, mainKeyboard())
}

func (b *Bot) handleToday(ctx context.Context, _ *tg.Bot, u *models.Update) {
	b.withUserGroup(ctx, u, func(g schedule.Group) {
		b.sendDay(ctx, u.Message.Chat.ID, g, b.svc.Today())
	})
}

func (b *Bot) handleTomorrow(ctx context.Context, _ *tg.Bot, u *models.Update) {
	b.withUserGroup(ctx, u, func(g schedule.Group) {
		b.sendDay(ctx, u.Message.Chat.ID, g, b.svc.Today().AddDate(0, 0, 1))
	})
}

func (b *Bot) handleCalendar(ctx context.Context, _ *tg.Bot, u *models.Update) {
	b.withUserGroup(ctx, u, func(g schedule.Group) {
		text, kb, err := b.renderCalendar(ctx, g, schedule.MonthOf(b.svc.Today()))
		if err != nil {
			b.fail(ctx, u.Message.Chat.ID, "render calendar", err)
			return
		}
		b.send(ctx, u.Message.Chat.ID, text, kb)
	})
}

func (b *Bot) handleGroup(ctx context.Context, _ *tg.Bot, u *models.Update) {
	g, ok, err := b.svc.UserGroup(ctx, senderID(u))
	if err != nil {
		b.fail(ctx, u.Message.Chat.ID, "get user group", err)
		return
	}
	text := textChooseGroup
	if ok {
		text = fmt.Sprintf("Сейчас выбрана группа <b>%s</b>.\n\n", escape(g.Name)) + textChooseGroup
	}
	b.send(ctx, u.Message.Chat.ID, text, nil)
}

// handleText — любой текст, не являющийся командой или кнопкой, считается
// поисковым запросом группы. Так не нужно хранить состояние «ждём название группы».
func (b *Bot) handleText(ctx context.Context, _ *tg.Bot, u *models.Update) {
	if u.Message == nil || u.Message.Text == "" {
		return
	}
	chatID, text := u.Message.Chat.ID, strings.TrimSpace(u.Message.Text)
	if strings.HasPrefix(text, "/") {
		b.send(ctx, chatID, "Не знаю такой команды 🙂 Список команд — /help.", mainKeyboard())
		return
	}
	if !b.search.Allow(senderID(u), time.Now()) {
		b.send(ctx, chatID, "⏳ Не так быстро — подождите пару секунд и повторите поиск.", nil)
		return
	}

	current, _, err := b.svc.UserGroup(ctx, senderID(u))
	if err != nil {
		b.fail(ctx, chatID, "get user group", err)
		return
	}
	groups, err := b.svc.SearchGroups(ctx, text)
	switch {
	case errors.Is(err, schedule.ErrQueryTooShort):
		b.send(ctx, chatID, fmt.Sprintf("Для поиска группы нужно хотя бы %d символа. Список команд — /help.", schedule.MinQueryLen), mainKeyboard())
		return
	case err != nil:
		b.logger(ctx).Error("search groups", "err", err, "query", text)
		b.send(ctx, chatID, "😔 Не получилось связаться с сайтом расписания. Попробуйте позже.", nil)
		return
	case len(groups) == 0:
		b.send(ctx, chatID, fmt.Sprintf("🔍 По запросу «%s» ничего не нашлось. Попробуйте написать иначе, например <code>ИОП</code>.", escape(text)), nil)
		return
	}

	rows := make([][]models.InlineKeyboardButton, 0, len(groups))
	for _, g := range groups {
		label := g.Name
		if g.ID == current.ID {
			label = "✅ " + label
		}
		rows = append(rows, []models.InlineKeyboardButton{{Text: label, CallbackData: groupData(g.ID)}})
	}
	msg := fmt.Sprintf("🔍 Нашёл по запросу «%s» — выберите группу:", escape(text))
	if len(groups) >= scraper.MaxSearchResults {
		msg += "\n<i>Показаны первые совпадения — уточните запрос, если нужной группы нет.</i>"
	}
	b.send(ctx, chatID, msg, models.InlineKeyboardMarkup{InlineKeyboard: rows})
}

// handleCallback обрабатывает все inline-кнопки. Навигация редактирует то же
// сообщение, а не шлёт новые — чат не засоряется.
func (b *Bot) handleCallback(ctx context.Context, _ *tg.Bot, u *models.Update) {
	q := u.CallbackQuery
	cb, err := parseCallback(q.Data)
	if err != nil {
		b.logger(ctx).Warn("bad callback", "err", err)
		b.answerCallback(ctx, q.ID, "Кнопка устарела — откройте расписание заново")
		return
	}

	switch cb.action {
	case actionNoop:
		b.answerCallback(ctx, q.ID, "")
		return
	case actionAccess:
		b.handleAccessCallback(ctx, q, cb)
		return
	case actionGroup:
		b.selectGroup(ctx, q, cb.groupID)
		return
	}

	// «Часики» на кнопке снимаем параллельно с подготовкой и отправкой ответа:
	// это отдельный запрос к Telegram, ждать его последовательно незачем.
	b.answerAsync(ctx, q.ID, "")

	g, err := b.svc.GroupByID(ctx, cb.groupID)
	if err != nil {
		b.logger(ctx).Error("get group", "err", err, "group_id", cb.groupID)
		return
	}
	var (
		text string
		kb   models.InlineKeyboardMarkup
	)
	switch cb.action {
	case actionDay:
		text, kb, err = b.renderDay(ctx, g, cb.date)
	case actionCalendar:
		text, kb, err = b.renderCalendar(ctx, g, cb.date)
	}
	if err != nil {
		b.logger(ctx).Error("render", "err", err, "action", string(cb.action))
		return
	}
	b.editOrSend(ctx, q.Message, text, kb)
}

// selectGroup запоминает выбранную группу и превращает сообщение со списком групп
// в расписание на сегодня. Если группу ещё ни разу не загружали, загружает её сразу.
func (b *Bot) selectGroup(ctx context.Context, q *models.CallbackQuery, groupID int64) {
	g, err := b.svc.SelectGroup(ctx, q.From.ID, groupID)
	if err != nil {
		b.logger(ctx).Error("select group", "err", err, "group_id", groupID)
		b.answerCallback(ctx, q.ID, "Не удалось выбрать группу")
		return
	}
	b.answerAsync(ctx, q.ID, "Группа выбрана: "+g.Name)

	_, err = b.svc.Status(ctx, g)
	switch {
	case errors.Is(err, schedule.ErrNotSynced):
		b.editOrSend(ctx, q.Message, fmt.Sprintf("✅ Ваша группа: <b>%s</b>\n\n⏳ Загружаю её расписание с сайта, это займёт несколько секунд…",
			escape(g.Name)), emptyKeyboard())
		if res := b.sync.RunGroup(ctx, g.Name); res.Err != nil && !errors.Is(res.Err, syncer.ErrInProgress) {
			b.logger(ctx).Error("initial group sync", "err", res.Err, "group", g.Name)
			b.editOrSend(ctx, q.Message, fmt.Sprintf("✅ Ваша группа: <b>%s</b>\n\n"+
				"😔 Не получилось загрузить расписание с сайта. Попробую снова при следующем плановом обновлении.",
				escape(g.Name)), emptyKeyboard())
			return
		}
	case err != nil:
		b.logger(ctx).Error("get group status", "err", err)
	}

	text, kb, err := b.renderDay(ctx, g, b.svc.Today())
	if err != nil {
		b.fail(ctx, chatOf(q.Message), "render day", err)
		return
	}
	b.editOrSend(ctx, q.Message, text, kb)
}

// --- rendering helpers ---

// withUserGroup достаёт группу пользователя и вызывает f. Если группа не выбрана —
// просит выбрать; при ошибке отвечает пользователю.
func (b *Bot) withUserGroup(ctx context.Context, u *models.Update, f func(schedule.Group)) {
	g, ok, err := b.svc.UserGroup(ctx, senderID(u))
	switch {
	case err != nil:
		b.fail(ctx, u.Message.Chat.ID, "get user group", err)
	case !ok:
		b.send(ctx, u.Message.Chat.ID, "Сначала выберите группу.\n\n"+textChooseGroup, mainKeyboard())
	default:
		f(g)
	}
}

func (b *Bot) sendDay(ctx context.Context, chatID int64, g schedule.Group, date time.Time) {
	text, kb, err := b.renderDay(ctx, g, date)
	if err != nil {
		b.fail(ctx, chatID, "render day", err)
		return
	}
	b.send(ctx, chatID, text, kb)
}

func (b *Bot) renderDay(ctx context.Context, g schedule.Group, date time.Time) (string, models.InlineKeyboardMarkup, error) {
	day, err := b.svc.Day(ctx, g, date)
	if err != nil {
		return "", models.InlineKeyboardMarkup{}, err
	}
	today := b.svc.Today()
	return formatDay(day, today, b.opts.Location), dayKeyboard(g.ID, day.Date, today), nil
}

func (b *Bot) renderCalendar(ctx context.Context, g schedule.Group, month time.Time) (string, models.InlineKeyboardMarkup, error) {
	busy, err := b.svc.MonthDays(ctx, g, month)
	if err != nil {
		return "", models.InlineKeyboardMarkup{}, err
	}
	text := fmt.Sprintf("📆 Выберите день\n<i>%s · цифрой отмечены дни с парами</i>", escape(g.Name))
	return text, buildCalendar(g.ID, month, b.svc.Today(), busy), nil
}

func (b *Bot) startupText(ctx context.Context) string {
	var sb strings.Builder
	sb.WriteString("🤖 <b>Бот запущен</b>\n")

	if b.users.Mode() == users.ModeApproval {
		sb.WriteString("Доступ: по заявкам (одобряете вы)\n")
	} else {
		sb.WriteString("Доступ: открыт для всех\n")
	}
	if len(b.opts.SyncTimes) == 0 {
		sb.WriteString("Автообновление выключено — используйте /refresh.\n")
	} else {
		times := make([]string, len(b.opts.SyncTimes))
		for i, t := range b.opts.SyncTimes {
			times[i] = t.String()
		}
		fmt.Fprintf(&sb, "Обновление расписания: %s (%s)\n", strings.Join(times, ", "), b.opts.Location)
	}

	if list, err := b.users.List(ctx, 1000); err == nil {
		pending := 0
		for _, u := range list {
			if u.Status == users.StatusPending {
				pending++
			}
		}
		if pending > 0 {
			fmt.Fprintf(&sb, "⏳ Заявок на доступ: %d — /users\n", pending)
		}
	}

	groups, err := b.sync.Groups(ctx)
	if err != nil {
		b.logger(ctx).Error("get tracked groups", "err", err)
		sb.WriteString("⚠️ Не удалось прочитать состояние базы данных.")
		return sb.String()
	}
	if len(groups) == 0 {
		sb.WriteString("\nГрупп пока нет: их выбирают пользователи.")
		return sb.String()
	}
	fmt.Fprintf(&sb, "\nОтслеживаемые группы (%d):\n", len(groups))
	for _, name := range groups {
		cov, err := b.svc.Status(ctx, schedule.Group{Name: name})
		switch {
		case errors.Is(err, schedule.ErrNotSynced):
			fmt.Fprintf(&sb, "• %s — ещё не загружалась\n", escape(name))
		case err != nil:
			fmt.Fprintf(&sb, "• %s — ⚠️ ошибка чтения\n", escape(name))
		default:
			fmt.Fprintf(&sb, "• %s — обновлено %s\n", escape(name), cov.UpdatedAt.In(b.opts.Location).Format("02.01 15:04"))
		}
	}
	return sb.String()
}

func chatOf(msg models.MaybeInaccessibleMessage) int64 {
	switch {
	case msg.Message != nil:
		return msg.Message.Chat.ID
	case msg.InaccessibleMessage != nil:
		return msg.InaccessibleMessage.Chat.ID
	}
	return 0
}

// emptyKeyboard убирает inline-кнопки у сообщения (nil Telegram отвергает).
func emptyKeyboard() models.InlineKeyboardMarkup {
	return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
}
