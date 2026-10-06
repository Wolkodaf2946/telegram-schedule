package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // база часовых поясов внутри бинарника: Europe/Moscow работает в любом образе

	"telegram-schedule/internal/config"
	"telegram-schedule/internal/logging"
	"telegram-schedule/internal/schedule"
	"telegram-schedule/internal/scraper"
	"telegram-schedule/internal/storage/postgres"
	"telegram-schedule/internal/syncer"
	"telegram-schedule/internal/telegram"
)

func main() {
	syncOnce := flag.Bool("sync-once", false, "загрузить расписание один раз и выйти (для системного cron или отладки)")
	flag.Parse()

	if err := run(*syncOnce); err != nil {
		os.Exit(1)
	}
}

// run возвращает ошибку уже залогированной.
func run(syncOnce bool) (err error) {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error:\n%v\n", err)
		return err
	}
	log, closer, err := logging.New(logging.Options{
		Level:       cfg.LogLevel,
		File:        cfg.LogFile,
		MaxSizeMB:   cfg.LogMaxSizeMB,
		MaxBackups:  cfg.LogMaxBackups,
		ConsoleText: cfg.LogText,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "init logging: %v\n", err)
		return err
	}
	defer closer.Close()
	slog.SetDefault(log)
	// Фатальная ошибка попадает и в файл лога: defer выполняется до closer.Close().
	defer func() {
		if err != nil {
			log.Error("fatal", "err", err)
		}
	}()
	log.Info("logging configured", "file", cfg.LogFile, "log_level", cfg.LogLevel.String(), "dump_dir", cfg.DumpDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	store, err := postgres.Open(dbCtx, cfg.DatabaseURL)
	cancel()
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer store.Close()

	scr, err := scraper.New(cfg.ScheduleURL, scraper.Options{Timeout: time.Minute, Logger: log, DumpDir: cfg.DumpDir})
	if err != nil {
		return err
	}
	updater := syncer.New(scr, store, cfg.DefaultGroup, cfg.MonthsAhead, log)

	if syncOnce {
		results, err := updater.RunAll(ctx)
		if err != nil {
			return err
		}
		var errs []error
		for _, r := range results {
			if r.Err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", r.Group, r.Err))
			}
		}
		return errors.Join(errs...)
	}

	svc, err := schedule.NewService(ctx, store, scr, cfg.DefaultGroup, cfg.Location)
	if err != nil {
		return err
	}
	bot, err := telegram.New(svc, updater, telegram.Options{
		Token:      cfg.TelegramToken,
		Location:   cfg.Location,
		AdminIDs:   cfg.AdminIDs,
		AllowedIDs: cfg.AllowedIDs,

		StartupMessage: cfg.StartupMessage,
		SyncTimes:      cfg.SyncTimes,
	}, log)
	if err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Go(func() {
		updater.Loop(ctx, syncer.Schedule{
			Times:    cfg.SyncTimes,
			Location: cfg.Location,
			OnStart:  cfg.SyncOnStart,
			Retries:  []time.Duration{5 * time.Minute, 15 * time.Minute, 30 * time.Minute, time.Hour},
		})
	})

	log.Info("starting", "default_group", cfg.DefaultGroup, "sync_times", cfg.SyncTimes, "timezone", cfg.Location.String())
	bot.Run(ctx, 10*time.Second)

	wg.Wait() // Loop выходит по отмене ctx; идущая синхронизация ограничена своим таймаутом
	log.Info("shutdown complete")
	return nil
}
