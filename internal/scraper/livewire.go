package scraper

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// Протокол Livewire v2, как его использует schedule.siriusuniversity.ru:
//
//  1. GET /list — HTML страницы. В атрибуте wire:initial-data корневого элемента
//     компонента лежит его состояние (fingerprint + serverMemo), в inline-скрипте —
//     CSRF-токен (window.livewire_token). Сессия держится в cookie.
//  2. POST /livewire/message/<name> с телом {fingerprint, serverMemo, updates}.
//     updates — вызовы методов компонента: set(<группа>), addMonth, minusMonth.
//  3. Ответ: {effects: {html, dirty, redirect?}, serverMemo: {...частичный...}}.
//     effects.html — заново отрендеренный компонент (null, если ничего не изменилось),
//     serverMemo нужно влить в текущее состояние: data — поключево, остальное — заменой.

var (
	ErrSessionExpired  = errors.New("livewire session expired (HTTP 419)")
	ErrUnexpectedPage  = errors.New("unexpected page structure")
	ErrUnexpectedReply = errors.New("unexpected livewire reply")
)

var csrfTokenRe = regexp.MustCompile(`livewire_token\s*=\s*['"]([^'"]+)['"]`)

// component — живое состояние Livewire-компонента на стороне клиента.
type component struct {
	fingerprint json.RawMessage // передаётся обратно байт в байт
	name        string
	memo        *object
	html        string // последний отрендеренный HTML компонента
}

// session — одна браузероподобная сессия: cookie jar + CSRF-токен + компонент.
type session struct {
	http    *http.Client
	baseURL *url.URL
	ua      string
	log     *slog.Logger
	csrf    string
	comp    *component
	last    *exchange // последний ответ сайта — для дампа при ошибке разбора
}

// exchange — запрос к сайту и полученный ответ.
type exchange struct {
	method, path string
	reqBody      []byte
	status       int
	contentType  string
	body         []byte
}

func (s *session) open(ctx context.Context) error {
	page, err := s.do(ctx, http.MethodGet, "/list", nil, nil)
	if err != nil {
		return err
	}

	m := csrfTokenRe.FindSubmatch(page)
	if m == nil {
		return fmt.Errorf("%w: CSRF token not found", ErrUnexpectedPage)
	}
	s.csrf = string(m[1])

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(page))
	if err != nil {
		return fmt.Errorf("parse page: %w", err)
	}
	raw, ok := doc.Find("[wire\\:initial-data]").First().Attr("wire:initial-data")
	if !ok {
		return fmt.Errorf("%w: wire:initial-data not found", ErrUnexpectedPage)
	}
	var initial struct {
		Fingerprint json.RawMessage `json:"fingerprint"`
		ServerMemo  *object         `json:"serverMemo"`
	}
	if err := json.Unmarshal([]byte(raw), &initial); err != nil {
		return fmt.Errorf("%w: decode wire:initial-data: %v", ErrUnexpectedPage, err)
	}
	var fp struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(initial.Fingerprint, &fp); err != nil || fp.Name == "" || initial.ServerMemo == nil {
		return fmt.Errorf("%w: incomplete component state", ErrUnexpectedPage)
	}

	s.comp = &component{
		fingerprint: initial.Fingerprint,
		name:        fp.Name,
		memo:        initial.ServerMemo,
		html:        string(page),
	}
	return nil
}

// call вызывает метод компонента (wire:click) и применяет ответ к состоянию.
func (s *session) call(ctx context.Context, method string, params ...any) error {
	if params == nil {
		params = []any{}
	}
	return s.send(ctx, method, map[string]any{
		"type": "callMethod",
		"payload": map[string]any{
			"id":     randomID(),
			"method": method,
			"params": params,
		},
	})
}

// input меняет свойство компонента (wire:model) — так браузер отправляет ввод в поле поиска.
func (s *session) input(ctx context.Context, name, value string) error {
	return s.send(ctx, "input "+name, map[string]any{
		"type": "syncInput",
		"payload": map[string]any{
			"id":    randomID(),
			"name":  name,
			"value": value,
		},
	})
}

func (s *session) send(ctx context.Context, method string, update map[string]any) error {
	body, err := json.Marshal(map[string]any{
		"fingerprint": s.comp.fingerprint,
		"serverMemo":  s.comp.memo,
		"updates":     []any{update},
	})
	if err != nil {
		return fmt.Errorf("encode livewire request: %w", err)
	}

	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "text/html, application/xhtml+xml",
		"X-Livewire":   "true",
		"X-CSRF-TOKEN": s.csrf,
	}
	respBody, err := s.do(ctx, http.MethodPost, "/livewire/message/"+url.PathEscape(s.comp.name), bytes.NewReader(body), headers)
	if err != nil {
		return fmt.Errorf("call %s: %w", method, err)
	}

	var reply struct {
		Effects struct {
			HTML     *string `json:"html"`
			Redirect string  `json:"redirect"`
		} `json:"effects"`
		ServerMemo *object `json:"serverMemo"`
	}
	if err := json.Unmarshal(respBody, &reply); err != nil {
		return fmt.Errorf("%w: call %s: %v", ErrUnexpectedReply, method, err)
	}
	if reply.Effects.Redirect != "" {
		return fmt.Errorf("%w: call %s: redirect to %s", ErrUnexpectedReply, method, reply.Effects.Redirect)
	}
	if reply.ServerMemo == nil {
		return fmt.Errorf("%w: call %s: no serverMemo", ErrUnexpectedReply, method)
	}
	if err := s.comp.mergeMemo(reply.ServerMemo); err != nil {
		return fmt.Errorf("call %s: %w", method, err)
	}
	if reply.Effects.HTML != nil && *reply.Effects.HTML != "" {
		s.comp.html = *reply.Effects.HTML
	}
	return nil
}

// mergeMemo вливает частичный serverMemo из ответа так же, как это делает livewire.js.
func (c *component) mergeMemo(patch *object) error {
	for _, key := range patch.keys {
		val := patch.vals[key]
		if key != "data" {
			c.memo.Set(key, val)
			continue
		}
		data := newObject()
		if raw, ok := c.memo.Get("data"); ok {
			if err := json.Unmarshal(raw, data); err != nil {
				return fmt.Errorf("%w: decode memo data: %v", ErrUnexpectedReply, err)
			}
		}
		var dataPatch object
		if err := json.Unmarshal(val, &dataPatch); err != nil {
			return fmt.Errorf("%w: decode reply data: %v", ErrUnexpectedReply, err)
		}
		for _, k := range dataPatch.keys {
			data.Set(k, dataPatch.vals[k])
		}
		merged, err := json.Marshal(data)
		if err != nil {
			return err
		}
		c.memo.Set("data", merged)
	}
	return nil
}

// data возвращает serverMemo.data.
func (c *component) data() (*object, error) {
	data := newObject()
	if err := c.memo.decodeKey("data", data); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnexpectedReply, err)
	}
	return data, nil
}

func (s *session) do(ctx context.Context, method, path string, body io.Reader, headers map[string]string) ([]byte, error) {
	u := s.baseURL.JoinPath(path)
	var reqBody []byte
	if body != nil {
		var err error
		if reqBody, err = io.ReadAll(body); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", s.ua)
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
	req.Header.Set("Referer", s.baseURL.JoinPath("/list").String())
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	started := time.Now()
	resp, err := s.http.Do(req)
	if err != nil {
		s.log.Warn("site request failed", "method", method, "path", u.Path, "err", err,
			"duration_ms", time.Since(started).Milliseconds())
		return nil, fmt.Errorf("%s %s: %w", method, u.Path, err)
	}
	defer resp.Body.Close()

	// Ответ Livewire со списком групп весит ~600 КБ; 16 МБ — с большим запасом,
	// но защищает от бесконечного тела.
	const maxBody = 16 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%s %s: read body: %w", method, u.Path, err)
	}
	s.last = &exchange{
		method: method, path: u.Path, reqBody: reqBody,
		status: resp.StatusCode, contentType: resp.Header.Get("Content-Type"), body: data,
	}
	level := slog.LevelDebug
	if resp.StatusCode != http.StatusOK {
		level = slog.LevelWarn
	}
	s.log.Log(ctx, level, "site request", "method", method, "path", u.Path, "status", resp.StatusCode,
		"bytes", len(data), "duration_ms", time.Since(started).Milliseconds())
	switch {
	case resp.StatusCode == 419:
		return nil, ErrSessionExpired
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s %s: unexpected status %s", method, u.Path, resp.Status)
	}
	return data, nil
}

// randomID — id апдейта, как его генерирует livewire.js (сервер его только эхом возвращает).
func randomID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// stringOrNumber декодирует значение, которое Livewire отдаёт то строкой ("2026"), то числом.
type stringOrNumber string

func (s *stringOrNumber) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var v string
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		*s = stringOrNumber(v)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*s = stringOrNumber(n.String())
	return nil
}

func (s stringOrNumber) Int() (int, error) {
	return strconv.Atoi(strings.TrimSpace(string(s)))
}
