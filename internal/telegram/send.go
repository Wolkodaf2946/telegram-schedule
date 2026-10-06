package telegram

import (
	"context"
	"errors"
	"strings"
	"time"

	tg "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// maxRetryAfter — дольше этого на 429 не ждём: пользователю уже всё равно,
// а обработчик не должен висеть минутами.
const maxRetryAfter = 30 * time.Second

// withRetry выполняет запрос к Telegram и один раз повторяет его, если
// Telegram ответил 429 Too Many Requests, выждав retry_after.
func withRetry(ctx context.Context, call func() error) error {
	err := call()
	var tooMany *tg.TooManyRequestsError
	if !errors.As(err, &tooMany) {
		return err
	}
	wait := time.Duration(tooMany.RetryAfter) * time.Second
	if wait > maxRetryAfter {
		return err
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return err
	case <-t.C:
		return call()
	}
}

// send отправляет HTML-сообщение. markup может быть nil.
func (b *Bot) send(ctx context.Context, chatID int64, text string, markup models.ReplyMarkup) {
	err := withRetry(ctx, func() error {
		_, err := b.api.SendMessage(ctx, &tg.SendMessageParams{
			ChatID:             chatID,
			Text:               text,
			ParseMode:          models.ParseModeHTML,
			ReplyMarkup:        markup,
			LinkPreviewOptions: noPreview(),
		})
		return err
	})
	b.logSendError(ctx, "send message", chatID, err)
}

// editOrSend заменяет текст и клавиатуру сообщения с кнопкой. Если сообщение
// недоступно (старше 48 часов или удалено), отправляет новое.
func (b *Bot) editOrSend(ctx context.Context, msg models.MaybeInaccessibleMessage, text string, kb models.InlineKeyboardMarkup) {
	var (
		chatID    int64
		messageID int
	)
	switch {
	case msg.Message != nil:
		chatID, messageID = msg.Message.Chat.ID, msg.Message.ID
	case msg.InaccessibleMessage != nil:
		b.send(ctx, msg.InaccessibleMessage.Chat.ID, text, kb)
		return
	default:
		return
	}

	err := withRetry(ctx, func() error {
		_, err := b.api.EditMessageText(ctx, &tg.EditMessageTextParams{
			ChatID:             chatID,
			MessageID:          messageID,
			Text:               text,
			ParseMode:          models.ParseModeHTML,
			ReplyMarkup:        kb,
			LinkPreviewOptions: noPreview(),
		})
		return err
	})
	switch {
	case err == nil, isNotModified(err):
		// «Сегодня», нажатое на сегодняшнем расписании, — не ошибка.
	case errors.Is(err, tg.ErrorBadRequest):
		// Сообщение нельзя отредактировать (слишком старое и т. п.) — шлём новое.
		b.send(ctx, chatID, text, kb)
	default:
		b.logSendError(ctx, "edit message", chatID, err)
	}
}

func (b *Bot) answerCallback(ctx context.Context, id, text string) {
	_, err := b.api.AnswerCallbackQuery(ctx, &tg.AnswerCallbackQueryParams{CallbackQueryID: id, Text: text})
	if err != nil {
		b.logger(ctx).Warn("answer callback", "err", err)
	}
}

// fail логирует внутреннюю ошибку и сообщает пользователю общий текст:
// детали ошибок БД пользователю не показываем.
func (b *Bot) fail(ctx context.Context, chatID int64, op string, err error) {
	b.logger(ctx).Error(op, "err", err, "chat_id", chatID)
	b.send(ctx, chatID, textError, nil)
}

func (b *Bot) logSendError(ctx context.Context, op string, chatID int64, err error) {
	switch {
	case err == nil:
	case errors.Is(err, tg.ErrorForbidden):
		b.logger(ctx).Info(op+": bot is blocked by user", "chat_id", chatID)
	default:
		b.logger(ctx).Error(op, "err", err, "chat_id", chatID)
	}
}

// isNotModified — Telegram не различает эту ситуацию кодом ошибки, только текстом.
// Единственное место в проекте, где ошибка сравнивается по строке, и это строка
// внешнего API, а не наша.
func isNotModified(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message is not modified")
}

func noPreview() *models.LinkPreviewOptions {
	disabled := true
	return &models.LinkPreviewOptions{IsDisabled: &disabled}
}
