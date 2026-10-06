package logging

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "bot.log")
	w, err := NewRotatingFile(path, 10, 2) // 10 байт — ротация почти на каждой записи
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"first-12345\n", "second-1234\n", "third-12345\n", "fourth-1234\n"} {
		if _, err := w.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		return string(b)
	}
	if got := read(path); got != "fourth-1234\n" {
		t.Errorf("current = %q", got)
	}
	if got := read(path + ".1"); got != "third-12345\n" {
		t.Errorf(".1 = %q", got)
	}
	if got := read(path + ".2"); got != "second-1234\n" {
		t.Errorf(".2 = %q", got)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Error("only 2 backups must be kept")
	}
}

func TestNewWritesJSONToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.log")
	log, closer, err := New(Options{Level: slog.LevelInfo, File: path})
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithLogger(context.Background(), log.With("user_id", 42))
	From(ctx, nil).Error("boom", "err", "site is down")
	log.Debug("hidden")
	closer.Close()

	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d lines: %s", len(lines), b)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["msg"] != "boom" || rec["level"] != "ERROR" || rec["user_id"] != float64(42) {
		t.Errorf("record = %v", rec)
	}
}

func TestFromFallback(t *testing.T) {
	fallback := slog.Default()
	if From(context.Background(), fallback) != fallback {
		t.Error("expected fallback logger")
	}
}
