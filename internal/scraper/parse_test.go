package scraper

import (
	"errors"
	"os"
	"slices"
	"testing"
	"time"

	"telegram-schedule/internal/schedule"
)

// Фикстуры сняты с реального сайта (HAR от 2026-10-06), октябрь 2026:
//   - list_page.html   — GET /list, группа ИОП-ИТ-24/2, 93 события;
//   - other_group.html — effects.html после set("С06ББ-25/2"), 93 события.
func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func date(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func mustClock(t *testing.T, s string) schedule.Clock {
	t.Helper()
	c, err := schedule.ParseClock(s)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestParseLessons_MyGroup(t *testing.T) {
	lessons, err := ParseLessons(readFixture(t, "list_page.html"), "ИОП-ИТ-24/2")
	if err != nil {
		t.Fatal(err)
	}
	if len(lessons) != 93 {
		t.Fatalf("got %d lessons, want 93", len(lessons))
	}

	first := lessons[0]
	want := schedule.Lesson{
		Group:      "ИОП-ИТ-24/2",
		Date:       date(2026, time.October, 1),
		Start:      mustClock(t, "08:45"),
		End:        mustClock(t, "10:05"),
		Discipline: "Элективные дисциплины по физической культуре и спорту (Физическая культура)",
		TypeTitle:  "Практические занятия",
		Address:    "",
		Room:       `ЛД "Айсберг"`,
		Teachers:   []string{"Басков Е. П."}, // на сайте «Басков Е.  П. » с двойным пробелом
		Department: "Ресурсный центр междисциплинарных исследований спорта",
	}
	if !lessonsEqual(first, want) {
		t.Errorf("first lesson:\n got %+v\nwant %+v", first, want)
	}
	if first.Kind() != schedule.KindPractice || first.Pair() != 1 {
		t.Errorf("kind/pair = %s/%d, want practice/1", first.Kind(), first.Pair())
	}

	second := lessons[1]
	if second.Discipline != "Теории систем" || second.Kind() != schedule.KindLecture ||
		second.Address != "Основной" || second.Pair() != 4 {
		t.Errorf("second lesson unexpected: %+v", second)
	}

	for i, l := range lessons {
		if l.Date.Month() != time.October || l.End <= l.Start || l.Discipline == "" {
			t.Errorf("lesson #%d looks broken: %+v", i, l)
		}
	}
}

func TestParseLessons_OtherGroup(t *testing.T) {
	lessons, err := ParseLessons(readFixture(t, "other_group.html"), "С06ББ-25/2")
	if err != nil {
		t.Fatal(err)
	}
	if len(lessons) != 93 {
		t.Fatalf("got %d lessons, want 93", len(lessons))
	}

	kinds := map[schedule.Kind]int{}
	for _, l := range lessons {
		kinds[l.Kind()]++
		if l.Room == "Альфа 5.7Альфа 5.7" {
			t.Fatalf("room was not de-duplicated: %q", l.Room)
		}
	}
	// Счётчики сверены с легендой на странице.
	wantKinds := map[schedule.Kind]int{
		schedule.KindLecture: 34, schedule.KindPractice: 28, schedule.KindLab: 11,
		schedule.KindExam: 2, schedule.KindOther: 18,
	}
	for k, n := range wantKinds {
		if kinds[k] != n {
			t.Errorf("kind %s: got %d, want %d", k, kinds[k], n)
		}
	}

	// Занятие с двумя преподавателями.
	idx := slices.IndexFunc(lessons, func(l schedule.Lesson) bool { return len(l.Teachers) == 2 })
	if idx < 0 {
		t.Fatal("no lesson with two teachers")
	}
	if got := lessons[idx].Teachers; got[0] != "Гребеников Д. А." || got[1] != "Кучумов А. Г." {
		t.Errorf("teachers = %q", got)
	}

	// Аудитория «Альфа 5.7» после схлопывания дубля.
	if !slices.ContainsFunc(lessons, func(l schedule.Lesson) bool { return l.Room == "Альфа 5.7" }) {
		t.Error("no lesson in room «Альфа 5.7»")
	}
}

func TestParseLessons_BrokenRowFails(t *testing.T) {
	html := `<table class="table-list"><tbody><tr>
		<td x-show="getColumn('date').opened">31.02.2026</td>
		<td x-show="getColumn('start').opened">08:45</td>
		<td x-show="getColumn('end').opened">10:05</td>
		<td x-show="getColumn('discipline').opened">Матан</td>
	</tr></tbody></table>`
	_, err := ParseLessons(html, "G")
	if !errors.Is(err, ErrBadRow) {
		t.Fatalf("err = %v, want ErrBadRow", err)
	}
}

func TestParseLessons_EmptyTable(t *testing.T) {
	lessons, err := ParseLessons(`<table class="table-list"><tbody></tbody></table>`, "G")
	if err != nil || len(lessons) != 0 {
		t.Fatalf("got %v, %v; want no lessons and no error", lessons, err)
	}
}

func TestParseLessons_OnlineRoomLink(t *testing.T) {
	html := `<table class="table-list"><tbody><tr>
		<td x-show="getColumn('date').opened">06.10.2026</td>
		<td x-show="getColumn('start').opened">10:20</td>
		<td x-show="getColumn('end').opened">11:40</td>
		<td x-show="getColumn('discipline').opened">Онлайн-курс</td>
		<td x-show="getColumn('room').opened"><a href=" https://example.org/meet ">ссылка</a></td>
	</tr></tbody></table>`
	lessons, err := ParseLessons(html, "G")
	if err != nil {
		t.Fatal(err)
	}
	if lessons[0].Room != "https://example.org/meet" {
		t.Errorf("room = %q", lessons[0].Room)
	}
}

func TestUndouble(t *testing.T) {
	for in, want := range map[string]string{
		"Альфа 5.7Альфа 5.7": "Альфа 5.7",
		"1.32К_3":            "1.32К_3",
		"abab":               "ab",
		"":                   "",
		"ЛД Айсберг":         "ЛД Айсберг",
	} {
		if got := undouble(in); got != want {
			t.Errorf("undouble(%q) = %q, want %q", in, got, want)
		}
	}
}

func lessonsEqual(a, b schedule.Lesson) bool {
	return a.Group == b.Group && a.Date.Equal(b.Date) && a.Start == b.Start && a.End == b.End &&
		a.Discipline == b.Discipline && a.TypeTitle == b.TypeTitle && a.Address == b.Address &&
		a.Room == b.Room && slices.Equal(a.Teachers, b.Teachers) && a.Department == b.Department
}
