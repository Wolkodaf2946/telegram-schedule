package scraper

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"

	"telegram-schedule/internal/schedule"
)

var ErrBadRow = errors.New("cannot parse schedule row")

// Колонки таблицы помечены атрибутом x-show="getColumn('<имя>').opened" — это
// стабильнее CSS-классов: у преподавателя и кафедры класс одинаковый (rasp-list-teachers).
var columnRe = regexp.MustCompile(`getColumn\('(\w+)'\)`)

// ParseLessons разбирает таблицу режима «список» (table.table-list) в занятия.
//
// Парсер строгий: если в строке нет даты, времени или дисциплины, или они не
// разбираются, возвращается ошибка. Значит, сайт поменял вёрстку, и лучше
// провалить синхронизацию и оставить в БД старое расписание, чем записать мусор.
func ParseLessons(html, group string) ([]schedule.Lesson, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}

	var (
		lessons []schedule.Lesson
		rowErr  error
	)
	doc.Find("table.table-list tbody tr").EachWithBreak(func(i int, tr *goquery.Selection) bool {
		cells := make(map[string]*goquery.Selection)
		tr.Find("td").Each(func(_ int, td *goquery.Selection) {
			if m := columnRe.FindStringSubmatch(td.AttrOr("x-show", "")); m != nil {
				cells[m[1]] = td
			}
		})

		l, err := parseRow(cells, group)
		if err != nil {
			rowErr = fmt.Errorf("%w #%d: %v", ErrBadRow, i+1, err)
			return false
		}
		lessons = append(lessons, l)
		return true
	})
	if rowErr != nil {
		return nil, rowErr
	}
	return lessons, nil
}

func parseRow(cells map[string]*goquery.Selection, group string) (schedule.Lesson, error) {
	text := func(col string) string {
		if c, ok := cells[col]; ok {
			return clean(c.Text())
		}
		return ""
	}

	date, err := time.Parse("02.01.2006", text("date"))
	if err != nil {
		return schedule.Lesson{}, fmt.Errorf("date %q: %w", text("date"), err)
	}
	start, err := schedule.ParseClock(text("start"))
	if err != nil {
		return schedule.Lesson{}, fmt.Errorf("start: %w", err)
	}
	end, err := schedule.ParseClock(text("end"))
	if err != nil {
		return schedule.Lesson{}, fmt.Errorf("end: %w", err)
	}
	discipline := text("discipline")
	if discipline == "" {
		return schedule.Lesson{}, errors.New("empty discipline")
	}

	return schedule.Lesson{
		Group:      group,
		Date:       schedule.DateOf(date),
		Start:      start,
		End:        end,
		Discipline: discipline,
		TypeTitle:  text("type"),
		Address:    text("address"),
		Room:       parseRoom(cells["room"]),
		Teachers:   parseTeachers(cells["teacher"]),
		Department: text("department"),
	}, nil
}

// parseGroups достаёт названия групп из выпадающего списка поиска:
// <li data-group="ИОП-ИТ-24/2" wire:click="set('ИОП-ИТ-24/2')">.
func parseGroups(html string) ([]string, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(html))
	if err != nil {
		return nil, fmt.Errorf("parse html: %w", err)
	}
	var groups []string
	doc.Find("[data-group]").Each(func(_ int, s *goquery.Selection) {
		if g := clean(s.AttrOr("data-group", "")); g != "" && !slices.Contains(groups, g) {
			groups = append(groups, g)
		}
	})
	return groups, nil
}

// parseRoom возвращает аудиторию, а для онлайн-занятий — ссылку.
func parseRoom(td *goquery.Selection) string {
	if td == nil {
		return ""
	}
	if href, ok := td.Find("a[href]").First().Attr("href"); ok && strings.TrimSpace(href) != "" {
		return strings.TrimSpace(href)
	}
	return undouble(clean(td.Text()))
}

func parseTeachers(td *goquery.Selection) []string {
	if td == nil {
		return nil
	}
	var teachers []string
	td.Find("span").Each(func(_ int, s *goquery.Selection) {
		if name := clean(s.Text()); name != "" {
			teachers = append(teachers, name)
		}
	})
	if len(teachers) == 0 { // вёрстка без <span> — делим по запятым
		for _, name := range strings.Split(clean(td.Text()), ",") {
			if name = clean(name); name != "" {
				teachers = append(teachers, name)
			}
		}
	}
	return teachers
}

// clean схлопывает пробельные символы: на сайте встречается «Басков Е.  П. ».
func clean(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// undouble исправляет баг сайта, который выводит аудиторию дважды подряд:
// «Альфа 5.7Альфа 5.7» → «Альфа 5.7».
func undouble(s string) string {
	r := []rune(s)
	if n := len(r); n > 0 && n%2 == 0 && string(r[:n/2]) == string(r[n/2:]) {
		return string(r[:n/2])
	}
	return s
}
