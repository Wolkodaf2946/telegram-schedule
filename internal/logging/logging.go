// Package logging настраивает структурные логи (log/slog): консоль + файл с ротацией,
// и хранит логгер с полями текущего запроса в context.Context.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

type Options struct {
	Level slog.Level
	// File — путь к JSON-логу. Пусто — только консоль.
	File string
	// MaxSizeMB и MaxBackups — ротация файла: при превышении размера текущий файл
	// переименовывается в .1 (старые сдвигаются), хранится MaxBackups штук.
	MaxSizeMB  int
	MaxBackups int
	// ConsoleText — консоль в человекочитаемом виде вместо JSON.
	ConsoleText bool
}

// New создаёт логгер. Closer нужно закрыть при завершении программы.
func New(opts Options) (*slog.Logger, io.Closer, error) {
	ho := &slog.HandlerOptions{Level: opts.Level}

	var console slog.Handler = slog.NewJSONHandler(os.Stdout, ho)
	if opts.ConsoleText {
		console = slog.NewTextHandler(os.Stdout, ho)
	}
	if opts.File == "" {
		return slog.New(console), io.NopCloser(nil), nil
	}

	w, err := NewRotatingFile(opts.File, int64(opts.MaxSizeMB)<<20, opts.MaxBackups)
	if err != nil {
		return nil, nil, err
	}
	// В файл всегда пишем JSON: его удобно фильтровать (grep, jq) и разбирать.
	file := slog.NewJSONHandler(w, ho)
	return slog.New(slog.NewMultiHandler(console, file)), w, nil
}

// NewJournal — отдельный журнал событий (например, пользователей) в JSON-файл с ротацией.
// Каждая запись дублируется в mirror (основной лог), если он задан, чтобы события были
// видны и в общей ленте. path пустой — журнал пишется только в mirror.
func NewJournal(path string, maxSizeMB, maxBackups int, mirror *slog.Logger) (*slog.Logger, io.Closer, error) {
	var handlers []slog.Handler
	if mirror != nil {
		handlers = append(handlers, mirror.Handler())
	}
	closer := io.Closer(io.NopCloser(nil))
	if path != "" {
		w, err := NewRotatingFile(path, int64(maxSizeMB)<<20, maxBackups)
		if err != nil {
			return nil, nil, err
		}
		handlers = append(handlers, slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
		closer = w
	}
	return slog.New(slog.NewMultiHandler(handlers...)), closer, nil
}

type ctxKey struct{}

// WithLogger кладёт в ctx логгер с полями текущего запроса (update_id, user_id, ...).
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// From достаёт логгер из ctx или возвращает fallback.
func From(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*slog.Logger); ok {
		return l
	}
	return fallback
}

// RotatingFile — io.WriteCloser с ротацией по размеру. Потокобезопасен.
type RotatingFile struct {
	path       string
	maxSize    int64
	maxBackups int

	mu   sync.Mutex
	f    *os.File
	size int64
}

func NewRotatingFile(path string, maxSize int64, maxBackups int) (*RotatingFile, error) {
	if maxSize <= 0 {
		maxSize = 10 << 20
	}
	if maxBackups < 0 {
		maxBackups = 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	r := &RotatingFile{path: path, maxSize: maxSize, maxBackups: maxBackups}
	if err := r.open(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *RotatingFile) open() error {
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *RotatingFile) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.maxSize && r.size > 0 {
		if err := r.rotate(); err != nil {
			// Не теряем запись из-за неудачной ротации — пишем в текущий файл.
			fmt.Fprintf(os.Stderr, "log rotation failed: %v\n", err)
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *RotatingFile) rotate() error {
	if err := r.f.Close(); err != nil {
		return err
	}
	if r.maxBackups == 0 {
		_ = os.Remove(r.path)
	} else {
		_ = os.Remove(fmt.Sprintf("%s.%d", r.path, r.maxBackups))
		for i := r.maxBackups - 1; i >= 1; i-- {
			_ = os.Rename(fmt.Sprintf("%s.%d", r.path, i), fmt.Sprintf("%s.%d", r.path, i+1))
		}
		if err := os.Rename(r.path, r.path+".1"); err != nil {
			_ = r.open()
			return err
		}
	}
	return r.open()
}

func (r *RotatingFile) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.f.Close()
}
