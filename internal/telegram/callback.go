package telegram

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Формат callback_data (лимит Telegram — 64 байта):
//
//	day:<group_id>:2026-10-06   показать расписание группы на день
//	cal:<group_id>:2026-10      показать календарь группы на месяц
//	grp:<group_id>              выбрать группу
//	noop                        кнопка-заглушка (пустая клетка, заголовок)
//
// Группа зашита в кнопку, поэтому старое сообщение продолжает листать ту группу,
// для которой было открыто, даже если пользователь потом сменил свою.
// Все кнопки строятся через *Data-функции, а разбираются одной parseCallback,
// поэтому формат описан ровно в одном месте.

type action string

const (
	actionDay      action = "day"
	actionCalendar action = "cal"
	actionGroup    action = "grp"
	actionNoop     action = "noop"
)

var errBadCallback = errors.New("malformed callback data")

type callback struct {
	action  action
	groupID int64
	date    time.Time // для day — дата, для cal — первое число месяца
}

func dayData(groupID int64, date time.Time) string {
	return fmt.Sprintf("%s:%d:%s", actionDay, groupID, date.Format(time.DateOnly))
}

func calData(groupID int64, month time.Time) string {
	return fmt.Sprintf("%s:%d:%s", actionCalendar, groupID, month.Format("2006-01"))
}

func groupData(groupID int64) string { return fmt.Sprintf("%s:%d", actionGroup, groupID) }

func noopData() string { return string(actionNoop) }

func parseCallback(data string) (callback, error) {
	bad := func() (callback, error) { return callback{}, fmt.Errorf("%w: %q", errBadCallback, data) }

	if data == string(actionNoop) {
		return callback{action: actionNoop}, nil
	}
	parts := strings.Split(data, ":")
	if len(parts) < 2 {
		return bad()
	}
	id, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || id <= 0 {
		return bad()
	}
	cb := callback{action: action(parts[0]), groupID: id}

	switch cb.action {
	case actionGroup:
		if len(parts) != 2 {
			return bad()
		}
		return cb, nil
	case actionDay, actionCalendar:
		if len(parts) != 3 {
			return bad()
		}
		layout := time.DateOnly
		if cb.action == actionCalendar {
			layout = "2006-01"
		}
		if cb.date, err = time.Parse(layout, parts[2]); err != nil {
			return bad()
		}
		return cb, nil
	default:
		return bad()
	}
}
