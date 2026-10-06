package scraper

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// maxDumps — сколько последних дампов хранить; старые удаляются.
const maxDumps = 30

var unsafeChars = regexp.MustCompile(`[^\p{L}\p{N}._-]+`)

// dumpOnError сохраняет последний ответ сайта, если операция упала на его разборе.
// Сетевые ошибки и отмена не дампятся — там нечего разбирать. Дамп нужен, чтобы
// починить парсер по реальному ответу, не воспроизводя запрос.
func (s *Scraper) dumpOnError(sess *session, op string, err error) {
	if err == nil || s.dumpDir == "" || sess == nil || sess.last == nil || !isParseError(err) {
		return
	}
	path, dumpErr := writeDump(s.dumpDir, op, err, sess.last)
	if dumpErr != nil {
		s.log.Error("save site response dump", "err", dumpErr)
		return
	}
	s.log.Error("site response could not be parsed, saved dump", "op", op, "err", err, "dump", path)
}

func isParseError(err error) bool {
	return errors.Is(err, ErrUnexpectedPage) || errors.Is(err, ErrUnexpectedReply) ||
		errors.Is(err, ErrBadRow) || errors.Is(err, ErrCountMismatch) ||
		errors.Is(err, ErrGroupNotFound) || errors.Is(err, ErrSessionExpired)
}

func writeDump(dir, op string, cause error, ex *exchange) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ext := ".html"
	if strings.Contains(ex.contentType, "json") {
		ext = ".json"
	}
	name := time.Now().Format("20060102-150405.000") + "_" + strings.Trim(unsafeChars.ReplaceAllString(op, "_"), "_")
	base := filepath.Join(dir, name)

	meta := fmt.Sprintf("op: %s\nerror: %v\nrequest: %s %s\nstatus: %d\ncontent-type: %s\nresponse: %s\n\n--- request body ---\n%s\n",
		op, cause, ex.method, ex.path, ex.status, ex.contentType, filepath.Base(base+ext), ex.reqBody)
	if err := os.WriteFile(base+".txt", []byte(meta), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(base+ext, ex.body, 0o644); err != nil {
		return "", err
	}
	prune(dir)
	return base + ext, nil
}

// prune оставляет в каталоге только maxDumps последних дампов (по имени = по времени).
func prune(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var metas []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".txt") {
			metas = append(metas, strings.TrimSuffix(e.Name(), ".txt"))
		}
	}
	slices.Sort(metas)
	for _, base := range metas[:max(0, len(metas)-maxDumps)] {
		for _, ext := range []string{".txt", ".html", ".json"} {
			_ = os.Remove(filepath.Join(dir, base+ext))
		}
	}
}
