// Package schedule — доменная модель расписания. Не знает ни про Telegram, ни про Postgres,
// ни про сайт университета: всё это адаптеры вокруг него.
package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Kind — тип занятия. Хранится в БД сырым текстом с сайта (lesson_type), а к Kind
// приводится при чтении: если улучшится сопоставление, миграция данных не понадобится.
type Kind string

const (
	KindLecture  Kind = "lecture"
	KindSeminar  Kind = "seminar"
	KindPractice Kind = "practice"
	KindLab      Kind = "lab"
	KindExam     Kind = "exam"
	KindOther    Kind = "other"
)

// ParseKind сопоставляет подпись типа с сайта («Лекции», «Практические занятия»,
// «Экзамен», ...) с Kind. Неизвестное — KindOther, а не ошибка: новый тип на сайте
// не должен ронять синхронизацию.
func ParseKind(title string) Kind {
	t := strings.ToLower(title)
	switch {
	case strings.HasPrefix(t, "лекци"):
		return KindLecture
	case strings.HasPrefix(t, "семинар"):
		return KindSeminar
	case strings.HasPrefix(t, "практическ"):
		return KindPractice
	case strings.HasPrefix(t, "лаборатор"):
		return KindLab
	case strings.HasPrefix(t, "экзамен"), strings.HasPrefix(t, "зач"):
		return KindExam
	default:
		return KindOther
	}
}

// Clock — время суток в минутах от полуночи. Отдельный тип вместо time.Time,
// чтобы не таскать фиктивную дату и часовой пояс там, где важны только часы и минуты.
type Clock int

var ErrInvalidClock = errors.New("invalid clock value")

// ParseClock разбирает строку вида "08:45".
func ParseClock(s string) (Clock, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrInvalidClock, s)
	}
	return Clock(t.Hour()*60 + t.Minute()), nil
}

func (c Clock) String() string {
	return fmt.Sprintf("%02d:%02d", int(c)/60, int(c)%60)
}

// Lesson — одно занятие.
type Lesson struct {
	Group      string
	Date       time.Time // календарная дата, полночь UTC (см. DateOf)
	Start      Clock
	End        Clock
	Discipline string
	TypeTitle  string // подпись типа как на сайте: «Практические занятия»
	Address    string // корпус: «Основной»
	Room       string // аудитория или ссылка на онлайн-занятие
	Teachers   []string
	Department string
}

func (l Lesson) Kind() Kind { return ParseKind(l.TypeTitle) }

// Pair возвращает номер пары по времени начала. Пары в Сириусе идут с 08:45
// по 80 минут с 15-минутными переменами; если начало не попадает в сетку
// (внеучебное мероприятие, сдвинутое занятие), возвращается 0.
func (l Lesson) Pair() int {
	const (
		firstPair = 8*60 + 45
		slot      = 80 + 15
	)
	offset := int(l.Start) - firstPair
	if offset < 0 || offset%slot != 0 {
		return 0
	}
	return offset/slot + 1
}

// DateOf возвращает календарную дату момента t (в его собственном часовом поясе)
// в каноничном виде — полночь UTC. Все даты в домене и в БД (тип DATE) хранятся так,
// поэтому их можно сравнивать через Equal и использовать как ключи.
func DateOf(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// MonthOf возвращает первое число месяца, в который попадает дата.
func MonthOf(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, time.UTC)
}

// MonthRange возвращает первый и последний день месяца, начинающегося с first.
func MonthRange(first time.Time) (from, to time.Time) {
	from = MonthOf(first)
	return from, from.AddDate(0, 1, -1)
}
