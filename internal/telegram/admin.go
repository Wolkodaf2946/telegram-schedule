package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"telegram-schedule/internal/syncer"
	"telegram-schedule/internal/users"
)

// Админ-панель — одно сообщение, которое редактируется на месте: вкладки
// «Заявки / Активные / Баны», страницы по users.PageSize человек, карточка пользователя.
// Каждое нажатие — два запроса к БД по индексу (страница и счётчики), без выборки всех.

var tabTitles = map[users.Status]string{
	users.StatusPending: "⏳ Заявки",
	users.StatusActive:  "✅ Активные",
	users.StatusBlocked: "⛔ Баны",
}

// handleAdmin открывает панель: на заявках, если они есть, иначе на активных.
func (b *Bot) handleAdmin(ctx context.Context, _ *tg.Bot, upd *models.Update) {
	chatID := upd.Message.Chat.ID
	page, err := b.users.Page(ctx, users.StatusPending, 0)
	if err == nil && page.Counts[users.StatusPending] == 0 {
		page, err = b.users.Page(ctx, users.StatusActive, 0)
	}
	if err != nil {
		b.fail(ctx, chatID, "admin panel", err)
		return
	}
	text, kb := b.renderAdminPage(page)
	b.send(ctx, chatID, text, kb)
}

// handleAdminCallback — нажатия внутри панели.
func (b *Bot) handleAdminCallback(ctx context.Context, q *models.CallbackQuery, cb callback) {
	if !b.users.IsAdmin(q.From.ID) {
		b.answerCallback(ctx, q.ID, "Только для администратора")
		return
	}

	toast := ""
	switch cb.adminOp {
	case adminView:
		u, err := b.users.Get(ctx, cb.userID)
		if err != nil {
			b.logger(ctx).Error("admin view user", "err", err, "target_user_id", cb.userID)
			b.answerCallback(ctx, q.ID, "Пользователь не найден")
			return
		}
		b.answerAsync(ctx, q.ID, "")
		text, kb := b.renderUserCard(u, cb.tab, cb.page)
		b.editOrSend(ctx, q.Message, text, kb)
		return

	case adminSet:
		if cb.status == users.StatusBlocked && b.users.IsAdmin(cb.userID) {
			b.answerCallback(ctx, q.ID, "Администратора нельзя забанить — уберите его из ADMIN_IDS")
			return
		}
		u, err := b.users.SetStatus(ctx, q.From.ID, cb.userID, cb.status)
		if err != nil {
			b.logger(ctx).Error("admin set status", "err", err, "target_user_id", cb.userID)
			b.answerCallback(ctx, q.ID, "Не удалось изменить доступ")
			return
		}
		toast = verdictText(u.Status) + ": " + u.DisplayName()
		// Уведомление пользователю — в фоне: админу не нужно его ждать.
		b.goTracked(func() { b.notifyAccessChange(context.WithoutCancel(ctx), u) })
	}

	// adminList и возврат к списку после действия.
	page, err := b.users.Page(ctx, cb.tab, cb.page)
	if err != nil {
		b.logger(ctx).Error("admin page", "err", err)
		b.answerCallback(ctx, q.ID, "Не удалось загрузить список")
		return
	}
	b.answerAsync(ctx, q.ID, toast)
	text, kb := b.renderAdminPage(page)
	b.editOrSend(ctx, q.Message, text, kb)
}

func (b *Bot) renderAdminPage(p users.Page) (string, models.InlineKeyboardMarkup) {
	var sb strings.Builder
	sb.WriteString("🛠 <b>Админ-панель</b>\n")
	if b.users.Mode() == users.ModeApproval {
		sb.WriteString("Доступ: по заявкам\n")
	} else {
		sb.WriteString("Доступ: открыт для всех\n")
	}
	fmt.Fprintf(&sb, "\n<b>%s</b>", tabTitles[p.Status])
	if p.Total > 1 {
		fmt.Fprintf(&sb, " · стр. %d из %d", p.Number+1, p.Total)
	}
	sb.WriteString("\n")

	rows := [][]models.InlineKeyboardButton{tabsRow(p)}
	if len(p.Users) == 0 {
		sb.WriteString("\nПусто.")
	}
	for i, u := range p.Users {
		n := p.Number*users.PageSize + i + 1
		fmt.Fprintf(&sb, "\n%d. %s", n, userLine(u))
		rows = append(rows, b.userRow(n, u, p.Status, p.Number))
	}
	if p.Total > 1 {
		rows = append(rows, pagerRow(p))
	}
	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "🔄 Обновить", CallbackData: adminListData(p.Status, p.Number)},
	})
	return sb.String(), models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

func tabsRow(p users.Page) []models.InlineKeyboardButton {
	row := make([]models.InlineKeyboardButton, 0, 3)
	for _, s := range []users.Status{users.StatusPending, users.StatusActive, users.StatusBlocked} {
		label := fmt.Sprintf("%s %d", tabTitles[s], p.Counts[s])
		if s == p.Status {
			label = "• " + label
		}
		row = append(row, models.InlineKeyboardButton{Text: label, CallbackData: adminListData(s, 0)})
	}
	return row
}

func pagerRow(p users.Page) []models.InlineKeyboardButton {
	prev := models.InlineKeyboardButton{Text: " ", CallbackData: noopData()}
	next := prev
	if p.Number > 0 {
		prev = models.InlineKeyboardButton{Text: "‹ Назад", CallbackData: adminListData(p.Status, p.Number-1)}
	}
	if p.Number < p.Total-1 {
		next = models.InlineKeyboardButton{Text: "Вперёд ›", CallbackData: adminListData(p.Status, p.Number+1)}
	}
	return []models.InlineKeyboardButton{
		prev,
		{Text: fmt.Sprintf("%d / %d", p.Number+1, p.Total), CallbackData: noopData()},
		next,
	}
}

// userRow — строка списка: имя (открывает карточку) и быстрые действия.
func (b *Bot) userRow(n int, u users.User, tab users.Status, page int) []models.InlineKeyboardButton {
	row := []models.InlineKeyboardButton{
		{Text: fmt.Sprintf("%d. %s", n, truncate(u.DisplayName(), 24)), CallbackData: adminViewData(tab, page, u.ID)},
	}
	return append(row, b.actionButtons(u, tab, page, true)...)
}

// actionButtons — что можно сделать с пользователем в его текущем статусе.
func (b *Bot) actionButtons(u users.User, tab users.Status, page int, short bool) []models.InlineKeyboardButton {
	btn := func(full, icon string, status users.Status) models.InlineKeyboardButton {
		label := full
		if short {
			label = icon
		}
		return models.InlineKeyboardButton{Text: label, CallbackData: adminSetData(tab, page, u.ID, status)}
	}
	switch {
	case b.users.IsAdmin(u.ID):
		return []models.InlineKeyboardButton{{Text: "👑", CallbackData: noopData()}}
	case u.Status == users.StatusPending:
		return []models.InlineKeyboardButton{btn("✅ Принять", "✅", users.StatusActive), btn("❌ Отклонить", "❌", users.StatusBlocked)}
	case u.Status == users.StatusActive:
		return []models.InlineKeyboardButton{btn("⛔ Забанить", "⛔", users.StatusBlocked)}
	default:
		return []models.InlineKeyboardButton{btn("✅ Разбанить", "✅", users.StatusActive)}
	}
}

func (b *Bot) renderUserCard(u users.User, tab users.Status, page int) (string, models.InlineKeyboardMarkup) {
	loc := b.opts.Location
	text := fmt.Sprintf("👤 <b>Пользователь</b>\n%s\n\nСтатус: %s\nВпервые: %s\nБыл активен: %s",
		userCard(u), statusText(u.Status),
		u.FirstSeen.In(loc).Format("02.01.2006 15:04"), u.LastSeen.In(loc).Format("02.01.2006 15:04"))
	kb := [][]models.InlineKeyboardButton{
		b.actionButtons(u, tab, page, false),
		{{Text: "« К списку", CallbackData: adminListData(tab, page)}},
	}
	return text, models.InlineKeyboardMarkup{InlineKeyboard: kb}
}

// announceNewUser сообщает администраторам о новом пользователе. В режиме по заявкам
// к сообщению прикреплены кнопки «Разрешить / Отклонить».
func (b *Bot) announceNewUser(ctx context.Context, u users.User) {
	if b.users.IsAdmin(u.ID) {
		return
	}
	text := "👤 <b>Новый пользователь</b>\n" + userCard(u)
	var kb models.ReplyMarkup
	if u.Status == users.StatusPending {
		text += "\n\nПросит доступ к боту. Все заявки — в /admin."
		kb = models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "✅ Разрешить", CallbackData: accessData(u.ID, true)},
			{Text: "❌ Отклонить", CallbackData: accessData(u.ID, false)},
		}}}
	}
	for id := range b.opts.AdminIDs {
		b.send(ctx, id, text, kb)
	}
}

// handleAccessCallback — администратор нажал «Разрешить» или «Отклонить» в уведомлении.
func (b *Bot) handleAccessCallback(ctx context.Context, q *models.CallbackQuery, cb callback) {
	if !b.users.IsAdmin(q.From.ID) {
		b.answerCallback(ctx, q.ID, "Только для администратора")
		return
	}
	status := users.StatusBlocked
	if cb.approve {
		status = users.StatusActive
	}
	u, err := b.users.SetStatus(ctx, q.From.ID, cb.userID, status)
	if err != nil {
		b.logger(ctx).Error("set user status", "err", err, "target_user_id", cb.userID)
		b.answerCallback(ctx, q.ID, "Не удалось изменить доступ")
		return
	}

	verdict := verdictText(u.Status)
	b.answerAsync(ctx, q.ID, verdict)
	b.editOrSend(ctx, q.Message, verdict+"\n"+userCard(u), emptyKeyboard())
	b.notifyAccessChange(ctx, u)
}

// notifyAccessChange сообщает пользователю о решении администратора.
func (b *Bot) notifyAccessChange(ctx context.Context, u users.User) {
	switch u.Status {
	case users.StatusActive:
		b.send(ctx, u.ID, "✅ Доступ к боту открыт!\n\n"+textChooseGroup, b.mainKeyboard(u.ID))
	case users.StatusBlocked:
		b.send(ctx, u.ID, "⛔ Доступ к боту закрыт.", &models.ReplyKeyboardRemove{RemoveKeyboard: true})
	}
}

func (b *Bot) handleRefresh(ctx context.Context, _ *tg.Bot, upd *models.Update) {
	chatID := upd.Message.Chat.ID
	b.send(ctx, chatID, "⏳ Загружаю расписание с сайта…", nil)

	results, err := b.sync.RunAll(ctx)
	if err != nil {
		b.fail(ctx, chatID, "manual refresh", err)
		return
	}
	if len(results) == 0 {
		b.send(ctx, chatID, "Обновлять нечего: никто ещё не выбрал группу.", nil)
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

// userCard — подробная карточка пользователя для администратора.
func userCard(u users.User) string {
	var sb strings.Builder
	sb.WriteString(escape(u.DisplayName()))
	if u.Username != "" {
		sb.WriteString(" (@" + escape(u.Username) + ")")
	}
	fmt.Fprintf(&sb, "\nID: <code>%d</code>", u.ID)
	if u.LanguageCode != "" {
		sb.WriteString(" · язык: " + escape(u.LanguageCode))
	}
	if u.GroupName != "" {
		sb.WriteString("\nГруппа: " + escape(u.GroupName))
	}
	return sb.String()
}

// userLine — пользователь одной строкой для списка в панели.
func userLine(u users.User) string {
	s := escape(u.DisplayName())
	if u.Username != "" {
		s += " @" + escape(u.Username)
	}
	s += fmt.Sprintf(" · <code>%d</code>", u.ID)
	if u.GroupName != "" {
		s += " · " + escape(u.GroupName)
	}
	return s
}

func statusText(s users.Status) string {
	switch s {
	case users.StatusActive:
		return "✅ доступ есть"
	case users.StatusPending:
		return "⏳ ждёт одобрения"
	default:
		return "⛔ забанен"
	}
}

func verdictText(s users.Status) string {
	if s == users.StatusActive {
		return "✅ Доступ выдан"
	}
	return "⛔ Доступ закрыт"
}
