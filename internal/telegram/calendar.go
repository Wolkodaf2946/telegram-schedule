package telegram

import (
	"fmt"
	"strconv"
	"time"

	"github.com/go-telegram/bot/models"
)

var (
	monthNames = [...]string{"", "Январь", "Февраль", "Март", "Апрель", "Май", "Июнь",
		"Июль", "Август", "Сентябрь", "Октябрь", "Ноябрь", "Декабрь"}
	weekdayHeader = [...]string{"Пн", "Вт", "Ср", "Чт", "Пт", "Сб", "Вс"}
)

// buildCalendar строит inline-календарь группы на месяц month.
//
//	‹   Октябрь 2026   ›
//	Пн Вт Ср Чт Пт Сб Вс
//	          1  2  ·  ·
//	[6] 7  8  9 10  ·  ·
//	...
//	     📅 Сегодня
//
// Дни с парами показаны числом, свободные — точкой, сегодняшний — в скобках.
// Нажать можно на любой день месяца: свободный покажет «пар нет».
// Навигация по месяцам не ограничена: перелистывание через год работает через AddDate.
func buildCalendar(groupID int64, month, today time.Time, busy map[int]bool) models.InlineKeyboardMarkup {
	first := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)

	rows := [][]models.InlineKeyboardButton{
		{
			{Text: "‹", CallbackData: calData(groupID, first.AddDate(0, -1, 0))},
			{Text: fmt.Sprintf("%s %d", monthNames[first.Month()], first.Year()), CallbackData: noopData()},
			{Text: "›", CallbackData: calData(groupID, first.AddDate(0, 1, 0))},
		},
	}

	header := make([]models.InlineKeyboardButton, 0, 7)
	for _, d := range weekdayHeader {
		header = append(header, models.InlineKeyboardButton{Text: d, CallbackData: noopData()})
	}
	rows = append(rows, header)

	week := make([]models.InlineKeyboardButton, 0, 7)
	for range mondayIndex(first) { // пустые клетки до первого числа
		week = append(week, blankButton())
	}
	for date := first; date.Month() == first.Month(); date = date.AddDate(0, 0, 1) {
		label := "·"
		if busy[date.Day()] {
			label = strconv.Itoa(date.Day())
		}
		if date.Equal(today) {
			label = "[" + strconv.Itoa(date.Day()) + "]"
		}
		week = append(week, models.InlineKeyboardButton{Text: label, CallbackData: dayData(groupID, date)})
		if len(week) == 7 {
			rows = append(rows, week)
			week = make([]models.InlineKeyboardButton, 0, 7)
		}
	}
	if len(week) > 0 {
		for len(week) < 7 {
			week = append(week, blankButton())
		}
		rows = append(rows, week)
	}

	rows = append(rows, []models.InlineKeyboardButton{
		{Text: "📅 Сегодня", CallbackData: dayData(groupID, today)},
	})
	return models.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// mondayIndex — номер дня недели, считая с понедельника (Пн=0 … Вс=6).
func mondayIndex(t time.Time) int {
	return (int(t.Weekday()) + 6) % 7
}

func blankButton() models.InlineKeyboardButton {
	return models.InlineKeyboardButton{Text: " ", CallbackData: noopData()}
}
