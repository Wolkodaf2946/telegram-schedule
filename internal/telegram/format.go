package telegram

import (
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"telegram-schedule/internal/schedule"
)

var (
	monthGenitive = [...]string{"", "января", "февраля", "марта", "апреля", "мая", "июня",
		"июля", "августа", "сентября", "октября", "ноября", "декабря"}
	monthShort = [...]string{"", "янв", "фев", "мар", "апр", "мая", "июн",
		"июл", "авг", "сен", "окт", "ноя", "дек"}
	weekdayNames = [...]string{"Воскресенье", "Понедельник", "Вторник", "Среда",
		"Четверг", "Пятница", "Суббота"}
)

// Цвета совпадают с легендой на сайте расписания.
var kindIcon = map[schedule.Kind]string{
	schedule.KindLecture:  "🟩",
	schedule.KindSeminar:  "🟨",
	schedule.KindPractice: "🟦",
	schedule.KindLab:      "🟪",
	schedule.KindExam:     "🟥",
	schedule.KindOther:    "🟧",
}

// Перерыв длиннее обычной перемены показываем отдельной строкой.
const minWindow = 30 * time.Minute

// formatDay рендерит расписание на день в HTML для parse_mode=HTML.
// Всё, что пришло с сайта, экранируется.
func formatDay(day schedule.Day, today time.Time, loc *time.Location) string {
	var b strings.Builder

	fmt.Fprintf(&b, "📅 <b>%s</b>", dayTitle(day.Date))
	if rel := relativeDay(day.Date, today); rel != "" {
		fmt.Fprintf(&b, " · %s", rel)
	}
	fmt.Fprintf(&b, "\n<i>%s</i>\n", html.EscapeString(day.Group.Name))

	switch {
	case len(day.Lessons) > 0:
		for i, l := range day.Lessons {
			if i > 0 {
				if gap := time.Duration(l.Start-day.Lessons[i-1].End) * time.Minute; gap >= minWindow {
					fmt.Fprintf(&b, "\n☕ <i>окно %s</i>\n", humanDuration(gap))
				}
			}
			b.WriteString("\n")
			writeLesson(&b, l)
		}
	case day.UpdatedAt.IsZero():
		b.WriteString("\n⏳ Расписание ещё ни разу не загружалось. Попробуйте позже.\n")
	case !day.Known:
		b.WriteString("\n🤷 На эту дату расписания пока нет: сайт его ещё не опубликовал или оно не загружено.\n")
	default:
		b.WriteString("\n🎉 Пар нет — можно отдохнуть.\n")
	}

	if !day.UpdatedAt.IsZero() {
		fmt.Fprintf(&b, "\n<i>Обновлено %s</i>", day.UpdatedAt.In(loc).Format("02.01 в 15:04"))
	}
	return b.String()
}

func writeLesson(b *strings.Builder, l schedule.Lesson) {
	e := html.EscapeString

	pair := ""
	if n := l.Pair(); n > 0 {
		pair = fmt.Sprintf("%d пара · ", n)
	}
	fmt.Fprintf(b, "<b>%s%s–%s</b>\n", pair, l.Start, l.End)
	fmt.Fprintf(b, "%s <b>%s</b>\n", kindIcon[l.Kind()], e(l.Discipline))
	if l.TypeTitle != "" {
		fmt.Fprintf(b, "<i>%s</i>\n", e(l.TypeTitle))
	}

	if place := placeLine(l); place != "" {
		fmt.Fprintf(b, "📍 %s\n", place)
	}
	if len(l.Teachers) > 0 {
		fmt.Fprintf(b, "👤 %s\n", e(strings.Join(l.Teachers, ", ")))
	}
}

// placeLine — аудитория и корпус; для онлайн-занятий — кликабельная ссылка.
func placeLine(l schedule.Lesson) string {
	e := html.EscapeString
	if strings.HasPrefix(l.Room, "http://") || strings.HasPrefix(l.Room, "https://") {
		return fmt.Sprintf(`<a href="%s">онлайн</a>`, e(l.Room))
	}
	parts := make([]string, 0, 2)
	if l.Room != "" {
		parts = append(parts, e(l.Room))
	}
	if l.Address != "" {
		parts = append(parts, e(l.Address))
	}
	return strings.Join(parts, " · ")
}

// dayKeyboard — навигация под расписанием дня.
func dayKeyboard(groupID int64, date, today time.Time) models.InlineKeyboardMarkup {
	prev, next := date.AddDate(0, 0, -1), date.AddDate(0, 0, 1)
	return models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{
		{
			{Text: "◀ " + shortDate(prev), CallbackData: dayData(groupID, prev)},
			{Text: "Сегодня", CallbackData: dayData(groupID, today)},
			{Text: shortDate(next) + " ▶", CallbackData: dayData(groupID, next)},
		},
		{
			{Text: "📆 Календарь", CallbackData: calData(groupID, schedule.MonthOf(date))},
		},
	}}
}

func dayTitle(d time.Time) string {
	return fmt.Sprintf("%s, %d %s", weekdayNames[d.Weekday()], d.Day(), monthGenitive[d.Month()])
}

func shortDate(d time.Time) string {
	return fmt.Sprintf("%d %s", d.Day(), monthShort[d.Month()])
}

func relativeDay(date, today time.Time) string {
	switch int(date.Sub(today).Hours() / 24) {
	case 0:
		return "сегодня"
	case 1:
		return "завтра"
	case -1:
		return "вчера"
	}
	return ""
}

func humanDuration(d time.Duration) string {
	h, m := int(d.Hours()), int(d.Minutes())%60
	switch {
	case h == 0:
		return fmt.Sprintf("%d мин", m)
	case m == 0:
		return fmt.Sprintf("%d ч", h)
	default:
		return fmt.Sprintf("%d ч %d мин", h, m)
	}
}
