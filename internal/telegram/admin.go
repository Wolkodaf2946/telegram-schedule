package telegram

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"telegram-schedule/internal/syncer"
	"telegram-schedule/internal/users"
)

// announceNewUser сообщает администраторам о новом пользователе. В режиме по заявкам
// к сообщению прикреплены кнопки «Разрешить / Отклонить».
func (b *Bot) announceNewUser(ctx context.Context, u users.User) {
	if b.users.IsAdmin(u.ID) {
		return
	}
	text := "👤 <b>Новый пользователь</b>\n" + userCard(u)
	var kb models.ReplyMarkup
	if u.Status == users.StatusPending {
		text += "\n\nПросит доступ к боту."
		kb = models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{{
			{Text: "✅ Разрешить", CallbackData: accessData(u.ID, true)},
			{Text: "❌ Отклонить", CallbackData: accessData(u.ID, false)},
		}}}
	}
	for id := range b.opts.AdminIDs {
		b.send(ctx, id, text, kb)
	}
}

// handleAccessCallback — администратор нажал «Разрешить» или «Отклонить».
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

	verdict := "❌ Отклонено"
	if cb.approve {
		verdict = "✅ Доступ выдан"
	}
	b.answerAsync(ctx, q.ID, verdict)
	b.editOrSend(ctx, q.Message, verdict+"\n"+userCard(u), emptyKeyboard())
	b.notifyAccessChange(ctx, u)
}

func (b *Bot) handleUsers(ctx context.Context, _ *tg.Bot, upd *models.Update) {
	const limit = 50
	list, err := b.users.List(ctx, limit)
	if err != nil {
		b.fail(ctx, upd.Message.Chat.ID, "list users", err)
		return
	}
	var sb strings.Builder
	mode := "открыт для всех"
	if b.users.Mode() == users.ModeApproval {
		mode = "по заявкам"
	}
	fmt.Fprintf(&sb, "<b>Пользователи</b> (последние %d) · доступ: %s\n\n", len(list), mode)
	for _, u := range list {
		sb.WriteString(statusIcon(u.Status) + " " + userLine(u) + "\n")
	}
	sb.WriteString("\n/allow &lt;id&gt; — открыть доступ, /revoke &lt;id&gt; — закрыть.")
	b.send(ctx, upd.Message.Chat.ID, sb.String(), nil)
}

func (b *Bot) handleAllow(ctx context.Context, _ *tg.Bot, upd *models.Update) {
	b.changeAccess(ctx, upd, users.StatusActive)
}

func (b *Bot) handleRevoke(ctx context.Context, _ *tg.Bot, upd *models.Update) {
	b.changeAccess(ctx, upd, users.StatusBlocked)
}

func (b *Bot) changeAccess(ctx context.Context, upd *models.Update, status users.Status) {
	chatID := upd.Message.Chat.ID
	fields := strings.Fields(upd.Message.Text)
	var id int64
	if len(fields) == 2 {
		id, _ = strconv.ParseInt(fields[1], 10, 64)
	}
	if id <= 0 {
		b.send(ctx, chatID, "Укажите Telegram ID: <code>/allow 123456789</code>. Список — /users.", nil)
		return
	}
	if status == users.StatusBlocked && b.users.IsAdmin(id) {
		b.send(ctx, chatID, "Администратора нельзя заблокировать — уберите его из ADMIN_IDS.", nil)
		return
	}

	u, err := b.users.SetStatus(ctx, senderID(upd), id, status)
	switch {
	case errors.Is(err, users.ErrUserNotFound):
		b.send(ctx, chatID, "Такого пользователя нет: он должен сначала написать боту /start.", nil)
		return
	case err != nil:
		b.fail(ctx, chatID, "set user status", err)
		return
	}
	b.send(ctx, chatID, statusIcon(u.Status)+" Готово\n"+userCard(u), nil)
	b.notifyAccessChange(ctx, u)
}

// notifyAccessChange сообщает пользователю о решении администратора.
func (b *Bot) notifyAccessChange(ctx context.Context, u users.User) {
	switch u.Status {
	case users.StatusActive:
		b.send(ctx, u.ID, "✅ Доступ к боту открыт!\n\n"+textChooseGroup, mainKeyboard())
	case users.StatusBlocked:
		b.send(ctx, u.ID, "⛔ Доступ к боту закрыт.", nil)
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

// userLine — пользователь одной строкой для списка /users.
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

func statusIcon(s users.Status) string {
	switch s {
	case users.StatusActive:
		return "✅"
	case users.StatusPending:
		return "⏳"
	default:
		return "⛔"
	}
}
