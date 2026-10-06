// Package config читает настройки из переменных окружения — единственного источника
// конфигурации. Ошибки собираются все сразу, чтобы не чинить .env по одной строке.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"telegram-schedule/internal/schedule"
)

type Config struct {
	TelegramToken string
	// TelegramAPIURL — адрес Bot API: официальный или свой (self-hosted telegram-bot-api, зеркало).
	TelegramAPIURL string
	// TelegramProxy — прокси только для запросов к Telegram (http, https, socks5, socks5h).
	// nil — напрямую (или через HTTPS_PROXY из окружения).
	TelegramProxy *url.URL

	DatabaseURL string

	DefaultGroup string // группа для новых пользователей, например «ИОП-ИТ-24/2»
	ScheduleURL  string
	MonthsAhead  int // сколько месяцев после текущего загружать

	Location    *time.Location   // часовой пояс университета: «сегодня» и время синхронизации
	SyncTimes   []schedule.Clock // когда обновлять расписание; пусто — только вручную
	SyncOnStart bool             // обновить при старте, если данные старше суток

	StartupMessage bool // при запуске написать администраторам, что бот работает

	AdminIDs   map[int64]bool // могут вызывать /refresh
	AllowedIDs map[int64]bool // если не пусто — бот отвечает только им

	LogLevel      slog.Level
	LogFile       string // JSON-лог в файл; пусто — только консоль
	LogText       bool   // консоль в человекочитаемом виде
	LogMaxSizeMB  int
	LogMaxBackups int
	// DumpDir — куда сохранять сырые ответы сайта, которые не удалось разобрать.
	// Пусто — не сохранять.
	DumpDir string
}

// Load собирает конфиг. getenv передаётся параметром, чтобы тестировать без os.Setenv.
func Load(getenv func(string) string) (Config, error) {
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}
	var errs []error

	cfg := Config{
		TelegramToken:  get("TELEGRAM_TOKEN", ""),
		TelegramAPIURL: strings.TrimRight(get("TELEGRAM_API_URL", "https://api.telegram.org"), "/"),
		DatabaseURL:    get("DATABASE_URL", ""),
		DefaultGroup:   get("DEFAULT_GROUP", "ИОП-ИТ-24/2"),
		ScheduleURL:    get("SCHEDULE_URL", "https://schedule.siriusuniversity.ru"),
	}
	if cfg.TelegramToken == "" {
		errs = append(errs, errors.New("TELEGRAM_TOKEN is required"))
	}
	if u, err := url.Parse(cfg.TelegramAPIURL); err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("TELEGRAM_API_URL: invalid url %q", cfg.TelegramAPIURL))
	}
	if p := get("TELEGRAM_PROXY", ""); p != "" {
		u, err := url.Parse(p)
		switch {
		case err != nil || u.Host == "":
			// Текст значения не выводим: в нём может быть пароль прокси.
			errs = append(errs, errors.New("TELEGRAM_PROXY: invalid url"))
		case !slices.Contains([]string{"http", "https", "socks5", "socks5h"}, u.Scheme):
			errs = append(errs, fmt.Errorf("TELEGRAM_PROXY: unsupported scheme %q (use http, https, socks5, socks5h)", u.Scheme))
		default:
			cfg.TelegramProxy = u
		}
	}
	if cfg.DatabaseURL == "" {
		errs = append(errs, errors.New("DATABASE_URL is required"))
	}
	if u, err := url.Parse(cfg.ScheduleURL); err != nil || u.Scheme == "" || u.Host == "" {
		errs = append(errs, fmt.Errorf("SCHEDULE_URL: invalid url %q", cfg.ScheduleURL))
	}

	var err error
	if cfg.MonthsAhead, err = strconv.Atoi(get("SYNC_MONTHS_AHEAD", "1")); err != nil || cfg.MonthsAhead < 0 || cfg.MonthsAhead > 6 {
		errs = append(errs, errors.New("SYNC_MONTHS_AHEAD must be an integer from 0 to 6"))
	}

	tz := get("TIMEZONE", "Europe/Moscow")
	if cfg.Location, err = time.LoadLocation(tz); err != nil {
		errs = append(errs, fmt.Errorf("TIMEZONE: %w", err))
	}

	if cfg.SyncTimes, err = parseTimes(get("SYNC_TIMES", "06:00")); err != nil {
		errs = append(errs, fmt.Errorf("SYNC_TIMES: %w", err))
	}
	if cfg.SyncOnStart, err = strconv.ParseBool(get("SYNC_ON_START", "true")); err != nil {
		errs = append(errs, errors.New("SYNC_ON_START must be true or false"))
	}
	if cfg.StartupMessage, err = strconv.ParseBool(get("STARTUP_MESSAGE", "true")); err != nil {
		errs = append(errs, errors.New("STARTUP_MESSAGE must be true or false"))
	}

	if cfg.AdminIDs, err = parseIDs(get("ADMIN_IDS", "")); err != nil {
		errs = append(errs, fmt.Errorf("ADMIN_IDS: %w", err))
	}
	if cfg.AllowedIDs, err = parseIDs(get("ALLOWED_USER_IDS", "")); err != nil {
		errs = append(errs, fmt.Errorf("ALLOWED_USER_IDS: %w", err))
	}

	if err := cfg.LogLevel.UnmarshalText([]byte(get("LOG_LEVEL", "info"))); err != nil {
		errs = append(errs, fmt.Errorf("LOG_LEVEL: %w", err))
	}
	cfg.LogFile = get("LOG_FILE", "logs/bot.log")
	if strings.EqualFold(cfg.LogFile, "off") {
		cfg.LogFile = ""
	}
	switch f := get("LOG_FORMAT", "text"); f {
	case "text", "json":
		cfg.LogText = f == "text"
	default:
		errs = append(errs, fmt.Errorf("LOG_FORMAT must be text or json, got %q", f))
	}
	if cfg.LogMaxSizeMB, err = strconv.Atoi(get("LOG_MAX_SIZE_MB", "20")); err != nil || cfg.LogMaxSizeMB < 1 {
		errs = append(errs, errors.New("LOG_MAX_SIZE_MB must be a positive integer"))
	}
	if cfg.LogMaxBackups, err = strconv.Atoi(get("LOG_MAX_BACKUPS", "5")); err != nil || cfg.LogMaxBackups < 0 {
		errs = append(errs, errors.New("LOG_MAX_BACKUPS must be a non-negative integer"))
	}
	cfg.DumpDir = get("DUMP_DIR", "logs/dumps")
	if strings.EqualFold(cfg.DumpDir, "off") {
		cfg.DumpDir = ""
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return cfg, nil
}

// parseTimes разбирает список времён «06:00,14:30». «off» — автообновление выключено.
func parseTimes(s string) ([]schedule.Clock, error) {
	if strings.EqualFold(s, "off") {
		return nil, nil
	}
	var times []schedule.Clock
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		c, err := schedule.ParseClock(part)
		if err != nil {
			return nil, fmt.Errorf("expected HH:MM list or \"off\", got %q", part)
		}
		if !slices.Contains(times, c) {
			times = append(times, c)
		}
	}
	if len(times) == 0 {
		return nil, errors.New(`empty list; use "off" to disable automatic sync`)
	}
	slices.Sort(times)
	return times, nil
}

// parseIDs разбирает список Telegram ID через запятую.
func parseIDs(s string) (map[int64]bool, error) {
	ids := make(map[int64]bool)
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid id %q", part)
		}
		ids[id] = true
	}
	return ids, nil
}
