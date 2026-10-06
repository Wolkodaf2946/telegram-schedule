package telegram

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"telegram-schedule/internal/schedule"
)

func d(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, time.UTC) }

func TestCallbackRoundTrip(t *testing.T) {
	day := d(2026, time.October, 6)
	cb, err := parseCallback(dayData(7, day))
	if err != nil || cb.action != actionDay || cb.groupID != 7 || !cb.date.Equal(day) {
		t.Fatalf("day: %+v, %v", cb, err)
	}

	month := d(2026, time.December, 1)
	cb, err = parseCallback(calData(7, month))
	if err != nil || cb.action != actionCalendar || cb.groupID != 7 || !cb.date.Equal(month) {
		t.Fatalf("cal: %+v, %v", cb, err)
	}

	cb, err = parseCallback(groupData(123456))
	if err != nil || cb.action != actionGroup || cb.groupID != 123456 {
		t.Fatalf("grp: %+v, %v", cb, err)
	}

	if cb, err := parseCallback(noopData()); err != nil || cb.action != actionNoop {
		t.Fatalf("noop: %+v, %v", cb, err)
	}

	// Мусор и данные от старой версии бота (форматы из прошлого проекта).
	for _, bad := range []string{"", "ignore", "day:", "day:1:2026-13-01", "day:2026-10-06", "cal:1:2026", "grp:", "grp:0", "grp:-1", "grp:x", "grp:1:2",
		"day:1:2026-10-06:x", "prev_night_2026_10", "2026-10-06", "x:1"} {
		if _, err := parseCallback(bad); !errors.Is(err, errBadCallback) {
			t.Errorf("parseCallback(%q) err = %v, want errBadCallback", bad, err)
		}
	}
}

func TestCallbackDataFitsTelegramLimit(t *testing.T) {
	const maxID = int64(1<<63 - 1)
	for _, s := range []string{dayData(maxID, d(2026, 12, 31)), calData(maxID, d(2026, 12, 1)), groupData(maxID), noopData()} {
		if len(s) > 64 {
			t.Errorf("callback %q is %d bytes, limit is 64", s, len(s))
		}
	}
}

func TestBuildCalendar(t *testing.T) {
	// Октябрь 2026 начинается в четверг.
	month, today := d(2026, time.October, 1), d(2026, time.October, 6)
	kb := buildCalendar(5, month, today, map[int]bool{1: true, 6: true, 7: true})
	rows := kb.InlineKeyboard

	if got := rows[0][1].Text; got != "Октябрь 2026" {
		t.Errorf("title = %q", got)
	}
	if rows[0][0].CallbackData != "cal:5:2026-09" || rows[0][2].CallbackData != "cal:5:2026-11" {
		t.Errorf("nav = %q / %q", rows[0][0].CallbackData, rows[0][2].CallbackData)
	}
	if rows[1][0].Text != "Пн" || rows[1][6].Text != "Вс" {
		t.Errorf("weekday header = %q..%q", rows[1][0].Text, rows[1][6].Text)
	}

	firstWeek := rows[2]
	for i := range 3 { // Пн, Вт, Ср — пустые
		if firstWeek[i].CallbackData != noopData() {
			t.Errorf("cell %d of first week should be blank, got %+v", i, firstWeek[i])
		}
	}
	if firstWeek[3].Text != "1" || firstWeek[3].CallbackData != "day:5:2026-10-01" {
		t.Errorf("1 Oct cell = %+v", firstWeek[3])
	}
	if firstWeek[4].Text != "·" { // 2 октября — пар нет
		t.Errorf("2 Oct cell = %q, want dot", firstWeek[4].Text)
	}

	secondWeek := rows[3]
	if secondWeek[1].Text != "[6]" { // вторник 6 октября — сегодня
		t.Errorf("today cell = %q", secondWeek[1].Text)
	}
	if secondWeek[2].Text != "7" {
		t.Errorf("7 Oct cell = %q", secondWeek[2].Text)
	}

	// Все строки по 7 кнопок, кроме заголовка и кнопки «Сегодня».
	for i, r := range rows[1 : len(rows)-1] {
		if len(r) != 7 {
			t.Errorf("row %d has %d buttons", i+1, len(r))
		}
	}
	last := rows[len(rows)-1]
	if len(last) != 1 || last[0].CallbackData != "day:5:2026-10-06" {
		t.Errorf("today button = %+v", last)
	}
}

func TestBuildCalendar_YearBoundary(t *testing.T) {
	kb := buildCalendar(1, d(2026, time.December, 1), d(2026, time.December, 15), nil)
	nav := kb.InlineKeyboard[0]
	if nav[0].CallbackData != "cal:1:2026-11" || nav[2].CallbackData != "cal:1:2027-01" {
		t.Errorf("nav = %q / %q", nav[0].CallbackData, nav[2].CallbackData)
	}
	kb = buildCalendar(1, d(2027, time.January, 1), d(2026, time.December, 15), nil)
	if got := kb.InlineKeyboard[0][0].CallbackData; got != "cal:1:2026-12" {
		t.Errorf("prev from January = %q", got)
	}
}

func TestBuildCalendar_MonthStartingOnMonday(t *testing.T) {
	// Июнь 2026 начинается в понедельник — пустых клеток в начале нет.
	kb := buildCalendar(1, d(2026, time.June, 1), d(2026, time.June, 1), nil)
	if got := kb.InlineKeyboard[2][0].CallbackData; got != "day:1:2026-06-01" {
		t.Errorf("first cell = %q", got)
	}
}

func lesson(start, end, discipline, typ string) schedule.Lesson {
	s, _ := schedule.ParseClock(start)
	e, _ := schedule.ParseClock(end)
	return schedule.Lesson{
		Group: "ИОП-ИТ-24/2", Date: d(2026, time.October, 6), Start: s, End: e,
		Discipline: discipline, TypeTitle: typ, Address: "Основной", Room: "Альфа 5.7",
		Teachers: []string{"Иванов И. И."},
	}
}

func TestFormatDay(t *testing.T) {
	msk, _ := time.LoadLocation("Europe/Moscow")
	day := schedule.Day{
		Group: schedule.Group{ID: 1, Name: "ИОП-ИТ-24/2"},
		Date:  d(2026, time.October, 6),
		Known: true,
		Lessons: []schedule.Lesson{
			lesson("08:45", "10:05", "Теория <систем> & Co", "Лекции"),
			lesson("13:30", "14:50", "Базы данных", "Практические занятия"),
		},
		UpdatedAt: time.Date(2026, time.October, 6, 3, 0, 0, 0, time.UTC),
	}
	got := formatDay(day, d(2026, time.October, 6), msk)

	for _, want := range []string{
		"📅 <b>Вторник, 6 октября</b> · сегодня",
		"<b>1 пара · 08:45–10:05</b>",
		"🟩 <b>Теория &lt;систем&gt; &amp; Co</b>", // HTML экранирован
		"<b>4 пара · 13:30–14:50</b>",
		"🟦 <b>Базы данных</b>",
		"☕ <i>окно 3 ч 25 мин</i>",
		"📍 Альфа 5.7 · Основной",
		"👤 Иванов И. И.",
		"Обновлено 06.10 в 06:00", // 03:00 UTC = 06:00 МСК
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestFormatDay_EmptyStates(t *testing.T) {
	msk, _ := time.LoadLocation("Europe/Moscow")
	g := schedule.Group{ID: 1, Name: "G"}
	base := schedule.Day{Group: g, Date: d(2026, time.October, 10)}
	updated := time.Date(2026, time.October, 6, 3, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		day  schedule.Day
		want string
	}{
		{"never synced", base, "ещё ни разу не загружалось"},
		{"outside synced range", schedule.Day{Group: g, Date: base.Date, UpdatedAt: updated}, "пока нет"},
		{"free day", schedule.Day{Group: g, Date: base.Date, Known: true, UpdatedAt: updated}, "Пар нет"},
	}
	for _, c := range cases {
		if got := formatDay(c.day, d(2026, time.October, 6), msk); !strings.Contains(got, c.want) {
			t.Errorf("%s: missing %q in:\n%s", c.name, c.want, got)
		}
	}
}

func TestPlaceLine_OnlineLink(t *testing.T) {
	l := schedule.Lesson{Room: "https://example.org/?a=1&b=2"}
	if got := placeLine(l); got != `<a href="https://example.org/?a=1&amp;b=2">онлайн</a>` {
		t.Errorf("placeLine = %q", got)
	}
}

func TestDayKeyboard(t *testing.T) {
	kb := dayKeyboard(3, d(2026, time.November, 1), d(2026, time.October, 6))
	row := kb.InlineKeyboard[0]
	want := []models.InlineKeyboardButton{
		{Text: "◀ 31 окт", CallbackData: "day:3:2026-10-31"},
		{Text: "Сегодня", CallbackData: "day:3:2026-10-06"},
		{Text: "2 ноя ▶", CallbackData: "day:3:2026-11-02"},
	}
	for i := range want {
		if row[i].Text != want[i].Text || row[i].CallbackData != want[i].CallbackData {
			t.Errorf("button %d = %+v, want %+v", i, row[i], want[i])
		}
	}
	if got := kb.InlineKeyboard[1][0].CallbackData; got != "cal:3:2026-11" {
		t.Errorf("calendar button = %q", got)
	}
}

func TestRateLimiter(t *testing.T) {
	r := newRateLimiter(3 * time.Second)
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	if !r.Allow(1, now) {
		t.Fatal("first request must pass")
	}
	if r.Allow(1, now.Add(time.Second)) {
		t.Error("second request within interval must be limited")
	}
	if !r.Allow(2, now.Add(time.Second)) {
		t.Error("other users are not affected")
	}
	if !r.Allow(1, now.Add(3*time.Second)) {
		t.Error("request after interval must pass")
	}
}

func TestUpdateAttrs(t *testing.T) {
	attrs := func(u *models.Update) map[string]any {
		a := updateAttrs(u)
		m := map[string]any{}
		for i := 0; i+1 < len(a); i += 2 {
			m[a[i].(string)] = a[i+1]
		}
		return m
	}

	msg := attrs(&models.Update{ID: 10, Message: &models.Message{
		Chat: models.Chat{ID: 100},
		From: &models.User{ID: 7, Username: "student"},
		Text: strings.Repeat("я", 250),
	}})
	if msg["update_id"] != int64(10) || msg["chat_id"] != int64(100) || msg["user_id"] != int64(7) ||
		msg["username"] != "student" || msg["kind"] != "message" {
		t.Errorf("message attrs = %v", msg)
	}
	if text := msg["text"].(string); len([]rune(text)) != 201 || !strings.HasSuffix(text, "…") {
		t.Errorf("text must be truncated to 200 runes + ellipsis, got %d runes", len([]rune(text)))
	}

	cb := attrs(&models.Update{ID: 11, CallbackQuery: &models.CallbackQuery{
		From:    models.User{ID: 8},
		Data:    "day:1:2026-10-06",
		Message: models.MaybeInaccessibleMessage{Message: &models.Message{Chat: models.Chat{ID: 200}}},
	}})
	if cb["kind"] != "callback" || cb["data"] != "day:1:2026-10-06" || cb["chat_id"] != int64(200) || cb["user_id"] != int64(8) {
		t.Errorf("callback attrs = %v", cb)
	}
}
