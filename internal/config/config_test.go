package config

import (
	"strings"
	"testing"

	"telegram-schedule/internal/schedule"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoad_Defaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"TELEGRAM_TOKEN": "t",
		"DATABASE_URL":   "postgres://x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultGroup != "ИОП-ИТ-24/2" || cfg.Location.String() != "Europe/Moscow" || cfg.MonthsAhead != 1 {
		t.Errorf("unexpected defaults: %+v", cfg)
	}
	if len(cfg.SyncTimes) != 1 || cfg.SyncTimes[0] != 6*60 {
		t.Errorf("default sync times = %v, want [06:00]", cfg.SyncTimes)
	}
	if !cfg.SyncOnStart || !cfg.StartupMessage {
		t.Error("SyncOnStart and StartupMessage should default to true")
	}
}

func TestLoad_Custom(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"TELEGRAM_TOKEN":   "t",
		"DATABASE_URL":     "postgres://x",
		"SYNC_TIMES":       " 20:00, 06:30,06:30 ",
		"SYNC_ON_START":    "false",
		"STARTUP_MESSAGE":  "false",
		"ADMIN_IDS":        "1, 2",
		"ALLOWED_USER_IDS": "3",
		"LOG_LEVEL":        "debug",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := []schedule.Clock{6*60 + 30, 20 * 60} // отсортировано, без дублей
	if len(cfg.SyncTimes) != 2 || cfg.SyncTimes[0] != want[0] || cfg.SyncTimes[1] != want[1] {
		t.Errorf("sync times = %v, want %v", cfg.SyncTimes, want)
	}
	if cfg.SyncOnStart || cfg.StartupMessage {
		t.Error("booleans not applied")
	}
	if !cfg.AdminIDs[1] || !cfg.AdminIDs[2] || !cfg.AllowedIDs[3] || len(cfg.AllowedIDs) != 1 {
		t.Errorf("ids: %v %v", cfg.AdminIDs, cfg.AllowedIDs)
	}
}

func TestLoad_SyncOff(t *testing.T) {
	cfg, err := Load(env(map[string]string{"TELEGRAM_TOKEN": "t", "DATABASE_URL": "x", "SYNC_TIMES": "OFF"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.SyncTimes) != 0 {
		t.Errorf("sync times = %v, want none", cfg.SyncTimes)
	}
}

func TestLoad_ReportsAllErrors(t *testing.T) {
	_, err := Load(env(map[string]string{
		"SYNC_TIMES": "6 утра",
		"TIMEZONE":   "Mars/Olympus",
		"ADMIN_IDS":  "abc",
	}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"TELEGRAM_TOKEN", "DATABASE_URL", "SYNC_TIMES", "TIMEZONE", "ADMIN_IDS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}
