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
)

// handleStart — вступительное сообщение: что умеет бот, клавиатура и сразу
// расписание на сегодня, чтобы первое касание было полезным.
func (b *Bot) handleStart(ctx context.Context, _ *tg.Bot, u *models.Update) {
	chatID := u.Message.Chat.ID
	g, err := b.svc.UserGroup(ctx, senderID(u))
	if err != nil {
		b.fail(ctx, chatID, "get user group", err)
		return
	}

	name := ""
	if u.Message.From != nil && u.Message.From.FirstName != "" {
		name = ", " + escape(u.Message.From.FirstName)
	}
	text := fmt.Sprintf(
		"👋 Привет%s! Я показываю расписание Университета «Сириус».\n\n"+
			"Сейчас выбрана группа <b>%s</b>.\n\n"+
			"Что я умею:\n"+
			"• %s — пары на сегодня\n"+
			"• %s — пары на завтра\n"+
			"• %s — выбрать любой день месяца\n"+
			"• %s — сменить группу. Можно и просто написать мне часть названия, например <code>ИОП-ИТ</code>\n\n"+
			"Под расписанием есть кнопки ◀ ▶ для перехода по дням. "+
			"Данные я беру с schedule.siriusuniversity.ru и регулярно обновляю.\n\n"+
			"А вот что сегодня 👇",
		name, escape(g.Name), btnToday, btnTomorrow, btnCalendar, btnGroup)
	b.send(ctx, chatID, text, mainKeyboard())
	b.sendDay(ctx, chatID, g, b.svc.Today())
}

func (b *Bot) handleHelp(ctx context.Context, _ *tg.Bot, u *models.Update) {
	text := "<b>Команды</b>\n" +
		"/today — расписание на сегодня\n" +
		"/tomorrow — на завтра\n" +
		"/calendar — календарь на месяц\n" +
		"/group — сменить группу\n\n" +
		"Чтобы найти группу, просто напишите часть её названия, например <code>ББ-25</code>.\n\n" +
		"В календаре цифрой отмечены дни с парами, точкой — свободные, [в скобках] — сегодня."
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
	b.withUserGroup(ctx, u, func(g schedule.Group) {
		text := fmt.Sprintf("Сейчас выбрана группа <b>%s</b>.\n\n"+
			"✏️ Напишите часть названия группы, и я найду её на сайте. Например: <code>ИОП-ИТ</code>, <code>ББ-25</code>.",
			escape(g.Name))
		var kb models.ReplyMarkup
		if def := b.svc.DefaultGroup(); def.ID != g.ID {
			kb = models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
				{{Text: "↩️ " + def.Name, CallbackData: groupData(def.ID)}},
			}}
		}
		b.send(ctx, u.Message.Chat.ID, text, kb)
	})
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

	current, err := b.svc.UserGroup(ctx, senderID(u))
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

func (b *Bot) handleRefresh(ctx context.Context, _ *tg.Bot, u *models.Update) {
	chatID := u.Message.Chat.ID
	if !b.opts.AdminIDs[senderID(u)] {
		b.send(ctx, chatID, "Эта команда доступна только администратору.", nil)
		return
	}
	b.send(ctx, chatID, "⏳ Загружаю расписание с сайта…", nil)

	results, err := b.sync.RunAll(ctx)
	if err != nil {
		b.fail(ctx, chatID, "manual refresh", err)
		return
	}
	var sb strings.Builder
	sb.WriteString("<b>Обновление расписания</b>\n")
	for _, r := range results {
		switch {
		case errors.Is(r.Err, syncer.ErrInProgress):
			fmt.Fprintf(&sb, "⏳ %s — уже обновляется\n", escape(r.Group))
		case r.Err != nil:
			fmt.Fprintf(&sb, "❌ %s — <code>%s</code>\n", escape(r.Group), escape(r.Err.Error()))
		default:
			fmt.Fprintf(&sb, "✅ %s — %d занятий, %s–%s\n", escape(r.Group), r.Lessons,
				r.From.Format("02.01"), r.To.Format("02.01"))
		}
	}
	b.send(ctx, chatID, sb.String(), nil)
}

// handleCallback обрабатывает все inline-кнопки. Навигация редактирует то же
// сообщение, а не шлёт новые — чат не засоряется.
func (b *Bot) handleCallback(ctx context.Context, _ *tg.Bot, u *models.Update) {
	q := u.CallbackQuery
	cb, err := parseCallback(q.Data)
	if err != nil {
		b.logger(ctx).Warn("bad callback", "err", err, "user_id", q.From.ID)
		b.answerCallback(ctx, q.ID, "Кнопка устарела — откройте расписание заново")
		return
	}
	if cb.action == actionNoop {
		b.answerCallback(ctx, q.ID, "")
		return
	}

	if cb.action == actionGroup {
		b.selectGroup(ctx, q, cb.groupID)
		return
	}

	g, err := b.svc.GroupByID(ctx, cb.groupID)
	if err != nil {
		b.logger(ctx).Error("get group", "err", err, "group_id", cb.groupID)
		b.answerCallback(ctx, q.ID, "Группа не найдена")
		return
	}

	var (
		text string
		kb   models.InlineKeyboardMarkup
	)
	switch cb.action {
	case actionDay:
		day, err := b.svc.Day(ctx, g, cb.date)
		if err != nil {
			b.logger(ctx).Error("get day", "err", err, "date", cb.date.Format(time.DateOnly))
			b.answerCallback(ctx, q.ID, "Не удалось загрузить расписание")
			return
		}
		today := b.svc.Today()
		text, kb = formatDay(day, today, b.opts.Location), dayKeyboard(g.ID, day.Date, today)
	case actionCalendar:
		text, kb, err = b.renderCalendar(ctx, g, cb.date)
		if err != nil {
			b.logger(ctx).Error("render calendar", "err", err)
			b.answerCallback(ctx, q.ID, "Не удалось загрузить календарь")
			return
		}
	}

	b.answerCallback(ctx, q.ID, "")
	b.editOrSend(ctx, q.Message, text, kb)
}

// selectGroup запоминает выбранную группу и показывает её расписание на сегодня.
// Если группу ещё ни разу не загружали, загружает её сразу, не дожидаясь утра.
func (b *Bot) selectGroup(ctx context.Context, q *models.CallbackQuery, groupID int64) {
	g, err := b.svc.SelectGroup(ctx, q.From.ID, groupID)
	if err != nil {
		b.logger(ctx).Error("select group", "err", err, "group_id", groupID)
		b.answerCallback(ctx, q.ID, "Не удалось выбрать группу")
		return
	}
	b.answerCallback(ctx, q.ID, "Группа выбрана")

	chatID := chatOf(q.Message)
	b.editOrSend(ctx, q.Message, fmt.Sprintf("✅ Ваша группа: <b>%s</b>", escape(g.Name)),
		models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}) // пустой массив убирает кнопки; nil Telegram отвергает

	_, err = b.svc.Status(ctx, g)
	switch {
	case errors.Is(err, schedule.ErrNotSynced):
		b.send(ctx, chatID, "⏳ Загружаю расписание этой группы с сайта, это займёт несколько секунд…", nil)
		res := b.sync.RunGroup(ctx, g.Name)
		if res.Err != nil && !errors.Is(res.Err, syncer.ErrInProgress) {
			b.logger(ctx).Error("initial group sync", "err", res.Err, "group", g.Name)
			b.send(ctx, chatID, "😔 Не получилось загрузить расписание. Попробую снова при следующем плановом обновлении.", nil)
			return
		}
	case err != nil:
		b.fail(ctx, chatID, "get group status", err)
		return
	}
	b.sendDay(ctx, chatID, g, b.svc.Today())
}

// --- rendering helpers ---

// withUserGroup достаёт группу пользователя и вызывает f; при ошибке отвечает пользователю.
func (b *Bot) withUserGroup(ctx context.Context, u *models.Update, f func(schedule.Group)) {
	g, err := b.svc.UserGroup(ctx, senderID(u))
	if err != nil {
		b.fail(ctx, u.Message.Chat.ID, "get user group", err)
		return
	}
	f(g)
}

func (b *Bot) sendDay(ctx context.Context, chatID int64, g schedule.Group, date time.Time) {
	day, err := b.svc.Day(ctx, g, date)
	if err != nil {
		b.fail(ctx, chatID, "get day", err)
		return
	}
	today := b.svc.Today()
	b.send(ctx, chatID, formatDay(day, today, b.opts.Location), dayKeyboard(g.ID, day.Date, today))
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

	if len(b.opts.SyncTimes) == 0 {
		sb.WriteString("Автообновление выключено — используйте /refresh.\n")
	} else {
		times := make([]string, len(b.opts.SyncTimes))
		for i, t := range b.opts.SyncTimes {
			times[i] = t.String()
		}
		fmt.Fprintf(&sb, "Обновление расписания: %s (%s)\n", strings.Join(times, ", "), b.opts.Location)
	}

	groups, err := b.sync.Groups(ctx)
	if err != nil {
		b.logger(ctx).Error("get tracked groups", "err", err)
		sb.WriteString("⚠️ Не удалось прочитать состояние базы данных.")
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
