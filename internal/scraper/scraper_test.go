package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Порядок ключей serverMemo.data в фикстуре list_page.html. Livewire считает checksum
// от json_encode(memo), поэтому клиент обязан вернуть ключи ровно в этом порядке.
var initialDataKeys = []string{
	"addNumMonth", "minusNumMonth", "addNumDay", "minusNumDay", "month", "year", "count",
	"type", "search", "group", "currentRouteName", "lectures", "seminars", "practices",
	"laboratories", "exams", "other", "width", "height",
}

const novemberHTML = `<div wire:id="x"><table class="table-list"><tbody>
<tr><td x-show="getColumn('date').opened">03.11.2026</td><td x-show="getColumn('start').opened">08:45</td>
<td x-show="getColumn('end').opened">10:05</td><td x-show="getColumn('discipline').opened">Матанализ</td>
<td x-show="getColumn('type').opened"><div class="ml-5">Лекции</div></td></tr>
<tr><td x-show="getColumn('date').opened">04.11.2026</td><td x-show="getColumn('start').opened">10:20</td>
<td x-show="getColumn('end').opened">11:40</td><td x-show="getColumn('discipline').opened">Физика</td>
<td x-show="getColumn('type').opened"><div class="ml-5">Семинар</div></td></tr>
</tbody></table></div>`

// fakeSite эмулирует schedule.siriusuniversity.ru на уровне протокола Livewire.
type fakeSite struct {
	t           *testing.T
	page        string
	knownGroups map[string]string // группа -> HTML ответа на set()
	novCount    int               // что сайт заявит в count для ноября
	searchHTML  string            // ответ на ввод в поле поиска
	status      int               // если не 0 — ответ на любой POST

	mu       sync.Mutex
	requests []map[string]json.RawMessage
}

func (f *fakeSite) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/list":
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "s1"})
		io.WriteString(w, f.page)
	case r.Method == http.MethodPost && r.URL.Path == "/livewire/message/main":
		f.handleMessage(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeSite) handleMessage(w http.ResponseWriter, r *http.Request) {
	t := f.t
	if f.status != 0 {
		w.WriteHeader(f.status)
		return
	}
	if r.Header.Get("X-Livewire") != "true" || r.Header.Get("X-CSRF-TOKEN") != "test-csrf-token" {
		t.Errorf("missing livewire headers: %v", r.Header)
	}
	if c, err := r.Cookie("session"); err != nil || c.Value != "s1" {
		t.Errorf("session cookie not sent")
	}

	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.mu.Unlock()

	var memo object
	if err := json.Unmarshal(body["serverMemo"], &memo); err != nil {
		t.Fatal(err)
	}
	if want := []string{"children", "errors", "htmlHash", "data", "dataMeta", "checksum"}; !slices.Equal(memo.keys, want) {
		t.Errorf("serverMemo keys order = %v, want %v", memo.keys, want)
	}
	var data object
	if err := json.Unmarshal(memo.vals["data"], &data); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(data.keys[:len(initialDataKeys)], initialDataKeys) {
		t.Errorf("data keys order changed: %v", data.keys)
	}

	var updates []struct {
		Type    string `json:"type"`
		Payload struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
			Name   string `json:"name"`
			Value  string `json:"value"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(body["updates"], &updates); err != nil || len(updates) != 1 {
		t.Fatalf("bad updates: %s", body["updates"])
	}

	w.Header().Set("Content-Type", "application/json")
	if up := updates[0]; up.Type == "syncInput" {
		if up.Payload.Name != "search" || up.Payload.Value != "ио" {
			t.Errorf("unexpected input %+v", up.Payload)
		}
		writeReply(w, f.searchHTML, `{"htmlHash":"h4","data":{"search":"ио","groupList":[]},"checksum":"c-search"}`)
		return
	}
	switch u := updates[0].Payload; u.Method {
	case "set":
		group, _ := u.Params[0].(string)
		html, ok := f.knownGroups[group]
		if !ok {
			// Сайт не нашёл группу: группа сбрасывается, событий нет.
			fmt.Fprint(w, `{"effects":{"html":null,"dirty":["group"]},"serverMemo":{"data":{"group":"","count":0},"checksum":"c-none"}}`)
			return
		}
		writeReply(w, html, fmt.Sprintf(`{"htmlHash":"h2","data":{"count":93,"group":%q},"checksum":"c-set"}`, group))
	case "addMonth":
		var checksum string
		_ = memo.decodeKey("checksum", &checksum)
		if checksum != "c-set" {
			t.Errorf("addMonth sent stale checksum %q", checksum)
		}
		writeReply(w, novemberHTML, fmt.Sprintf(
			`{"htmlHash":"h3","data":{"addNumMonth":1,"month":{"number":"11","full":"Ноябрь","fullForDisplay":"Ноября"},"year":"2026","count":%d},"checksum":"c-add"}`,
			f.novCount))
	default:
		t.Errorf("unexpected method %q", u.Method)
	}
}

func writeReply(w io.Writer, html, memo string) {
	h, _ := json.Marshal(html)
	fmt.Fprintf(w, `{"effects":{"html":%s,"dirty":["count"]},"serverMemo":%s}`, h, memo)
}

func newFake(t *testing.T) (*fakeSite, *Scraper) {
	f := &fakeSite{
		t:           t,
		page:        readFixture(t, "list_page.html"),
		knownGroups: map[string]string{"С06ББ-25/2": readFixture(t, "other_group.html")},
		novCount:    2,
		searchHTML:  readFixture(t, "search_io.html"),
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	s, err := New(srv.URL, Options{Timeout: 5 * time.Second, DumpDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return f, s
}

func TestFetch(t *testing.T) {
	f, s := newFake(t)

	months, err := s.Fetch(context.Background(), "С06ББ-25/2", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(months) != 2 {
		t.Fatalf("got %d months, want 2", len(months))
	}
	if !months[0].First.Equal(date(2026, time.October, 1)) || len(months[0].Lessons) != 93 {
		t.Errorf("october: %s, %d lessons", months[0].First, len(months[0].Lessons))
	}
	if !months[1].First.Equal(date(2026, time.November, 1)) || len(months[1].Lessons) != 2 {
		t.Errorf("november: %s, %d lessons", months[1].First, len(months[1].Lessons))
	}
	if g := months[1].Lessons[0].Group; g != "С06ББ-25/2" {
		t.Errorf("lesson group = %q", g)
	}

	// Fingerprint должен уходить на сервер без изменений.
	if len(f.requests) != 2 {
		t.Fatalf("got %d livewire requests, want 2", len(f.requests))
	}
	for _, r := range f.requests {
		if !strings.Contains(string(r["fingerprint"]), `"id":"3k6JRdxlLDHDW9jdYnyc"`) {
			t.Errorf("fingerprint changed: %s", r["fingerprint"])
		}
	}
}

func TestFetch_GroupNotFound(t *testing.T) {
	_, s := newFake(t)
	_, err := s.Fetch(context.Background(), "НЕТ-ТАКОЙ", 0)
	if !errors.Is(err, ErrGroupNotFound) {
		t.Fatalf("err = %v, want ErrGroupNotFound", err)
	}
}

func TestFetch_CountMismatch(t *testing.T) {
	f, s := newFake(t)
	f.novCount = 3 // сайт заявляет 3 события, а в таблице 2 — вёрстка поменялась
	_, err := s.Fetch(context.Background(), "С06ББ-25/2", 1)
	if !errors.Is(err, ErrCountMismatch) {
		t.Fatalf("err = %v, want ErrCountMismatch", err)
	}

	// Ответ, который не удалось разобрать, сохранён для отладки.
	entries, _ := os.ReadDir(s.dumpDir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if len(names) != 2 || !strings.HasSuffix(names[0], ".json") || !strings.HasSuffix(names[1], ".txt") {
		t.Fatalf("dump files = %v", names)
	}
	meta, _ := os.ReadFile(filepath.Join(s.dumpDir, names[1]))
	if !strings.Contains(string(meta), "parsed lessons count differs") || !strings.Contains(string(meta), "addMonth") {
		t.Errorf("dump meta:\n%s", meta)
	}
}

func TestFetch_SessionExpired(t *testing.T) {
	f, s := newFake(t)
	f.status = 419
	_, err := s.Fetch(context.Background(), "С06ББ-25/2", 0)
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("err = %v, want ErrSessionExpired", err)
	}
}

func TestObjectRoundTripPreservesOrder(t *testing.T) {
	in := `{"z":1,"a":{"y":"О","b":[1,2]},"m":null}`
	var o object
	if err := json.Unmarshal([]byte(in), &o); err != nil {
		t.Fatal(err)
	}
	o.Set("a", json.RawMessage(`2`)) // замена на месте
	o.Set("new", json.RawMessage(`true`))
	out, err := json.Marshal(&o)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"z":1,"a":2,"m":null,"new":true}`; string(out) != want {
		t.Errorf("got %s, want %s", out, want)
	}
}

func TestSearchGroups(t *testing.T) {
	_, s := newFake(t)
	groups, err := s.SearchGroups(context.Background(), "ио")
	if err != nil {
		t.Fatal(err)
	}
	// Фикстура — реальный ответ сайта на запрос «ио»: 17 групп.
	if len(groups) != 17 {
		t.Fatalf("got %d groups: %q", len(groups), groups)
	}
	for _, want := range []string{"ИОП-ИТ-24/1", "ИОП-ИТ-24/2", "ИОП-ИТ-26/2", "Актуальные задачи информационной безопасности"} {
		if !slices.Contains(groups, want) {
			t.Errorf("missing %q in %q", want, groups)
		}
	}
}

func TestFetch_NetworkErrorIsNotDumped(t *testing.T) {
	dir := t.TempDir()
	s, err := New("http://127.0.0.1:1", Options{Timeout: time.Second, DumpDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(context.Background(), "G", 0); err == nil {
		t.Fatal("expected network error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("network errors must not produce dumps, got %d files", len(entries))
	}
}
