package telegram

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"telegram-schedule/internal/users"
)

// Формат callback_data (лимит Telegram — 64 байта):
//
//	day:<group_id>:2026-10-06   показать расписание группы на день
//	cal:<group_id>:2026-10      показать календарь группы на месяц
//	grp:<group_id>              выбрать группу
//	acc:<user_id>:a|r           принять (a) или отклонить (r) заявку из уведомления админу
//	adm:l:<tab>:<page>                 админ-панель: список вкладки
//	adm:v:<tab>:<page>:<user_id>       админ-панель: карточка пользователя
//	adm:s:<tab>:<page>:<user_id>:a|b   админ-панель: дать доступ (a) или забанить (b)
//	noop                        кнопка-заглушка (пустая клетка, заголовок)
//
// <tab> — вкладка панели: p (заявки), a (активные), b (баны); <page> — номер с нуля.
// Вкладка и страница зашиты в кнопки, чтобы после действия вернуться туда же.
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
	actionAccess   action = "acc"
	actionAdmin    action = "adm"
	actionNoop     action = "noop"
)

// Операции админ-панели.
const (
	adminList = "l"
	adminView = "v"
	adminSet  = "s"
)

var errBadCallback = errors.New("malformed callback data")

type callback struct {
	action  action
	groupID int64     // day, cal, grp
	date    time.Time // day — дата, cal — первое число месяца
	userID  int64     // acc, adm v/s
	approve bool      // acc

	adminOp string       // adm: l, v, s
	tab     users.Status // adm: вкладка
	page    int          // adm: страница
	status  users.Status // adm s: новый статус
}

var (
	tabCodes   = map[users.Status]string{users.StatusPending: "p", users.StatusActive: "a", users.StatusBlocked: "b"}
	tabByCode  = map[string]users.Status{"p": users.StatusPending, "a": users.StatusActive, "b": users.StatusBlocked}
	setCodes   = map[users.Status]string{users.StatusActive: "a", users.StatusBlocked: "b"}
	setByCodes = map[string]users.Status{"a": users.StatusActive, "b": users.StatusBlocked}
)

func dayData(groupID int64, date time.Time) string {
	return fmt.Sprintf("%s:%d:%s", actionDay, groupID, date.Format(time.DateOnly))
}

func calData(groupID int64, month time.Time) string {
	return fmt.Sprintf("%s:%d:%s", actionCalendar, groupID, month.Format("2006-01"))
}

func groupData(groupID int64) string { return fmt.Sprintf("%s:%d", actionGroup, groupID) }

func accessData(userID int64, approve bool) string {
	decision := "r"
	if approve {
		decision = "a"
	}
	return fmt.Sprintf("%s:%d:%s", actionAccess, userID, decision)
}

func adminListData(tab users.Status, page int) string {
	return fmt.Sprintf("%s:%s:%s:%d", actionAdmin, adminList, tabCodes[tab], page)
}

func adminViewData(tab users.Status, page int, userID int64) string {
	return fmt.Sprintf("%s:%s:%s:%d:%d", actionAdmin, adminView, tabCodes[tab], page, userID)
}

func adminSetData(tab users.Status, page int, userID int64, status users.Status) string {
	return fmt.Sprintf("%s:%s:%s:%d:%d:%s", actionAdmin, adminSet, tabCodes[tab], page, userID, setCodes[status])
}

func noopData() string { return string(actionNoop) }

func parseCallback(data string) (callback, error) {
	bad := func() (callback, error) { return callback{}, fmt.Errorf("%w: %q", errBadCallback, data) }
	positive := func(s string) (int64, bool) {
		n, err := strconv.ParseInt(s, 10, 64)
		return n, err == nil && n > 0
	}

	parts := strings.Split(data, ":")
	switch action(parts[0]) {
	case actionNoop:
		if len(parts) != 1 {
			return bad()
		}
		return callback{action: actionNoop}, nil

	case actionGroup:
		if len(parts) != 2 {
			return bad()
		}
		id, ok := positive(parts[1])
		if !ok {
			return bad()
		}
		return callback{action: actionGroup, groupID: id}, nil

	case actionDay, actionCalendar:
		if len(parts) != 3 {
			return bad()
		}
		id, ok := positive(parts[1])
		if !ok {
			return bad()
		}
		layout := time.DateOnly
		if action(parts[0]) == actionCalendar {
			layout = "2006-01"
		}
		date, err := time.Parse(layout, parts[2])
		if err != nil {
			return bad()
		}
		return callback{action: action(parts[0]), groupID: id, date: date}, nil

	case actionAccess:
		if len(parts) != 3 || (parts[2] != "a" && parts[2] != "r") {
			return bad()
		}
		id, ok := positive(parts[1])
		if !ok {
			return bad()
		}
		return callback{action: actionAccess, userID: id, approve: parts[2] == "a"}, nil

	case actionAdmin:
		return parseAdmin(parts, bad, positive)
	}
	return bad()
}

func parseAdmin(parts []string, bad func() (callback, error), positive func(string) (int64, bool)) (callback, error) {
	// adm:<op>:<tab>:<page>[:<user_id>[:<status>]]
	if len(parts) < 4 {
		return bad()
	}
	tab, ok := tabByCode[parts[2]]
	page, err := strconv.Atoi(parts[3])
	if !ok || err != nil || page < 0 || page > 100_000 {
		return bad()
	}
	cb := callback{action: actionAdmin, adminOp: parts[1], tab: tab, page: page}

	switch {
	case cb.adminOp == adminList && len(parts) == 4:
		return cb, nil
	case cb.adminOp == adminView && len(parts) == 5:
		if cb.userID, ok = positive(parts[4]); !ok {
			return bad()
		}
		return cb, nil
	case cb.adminOp == adminSet && len(parts) == 6:
		if cb.userID, ok = positive(parts[4]); !ok {
			return bad()
		}
		if cb.status, ok = setByCodes[parts[5]]; !ok {
			return bad()
		}
		return cb, nil
	}
	return bad()
}
