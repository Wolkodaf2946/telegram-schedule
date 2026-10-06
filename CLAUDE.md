# CLAUDE.md

Telegram-бот с расписанием Университета «Сириус» на Go. Наследник проекта
[telegram-bot-sleep](telegram-bot-sleep/) (отдельный модуль, не трогать); новый код написан
с учётом разбора [telegram-bot-sleep/REVIEW.md](telegram-bot-sleep/REVIEW.md) — не возвращай
перечисленные там антипаттерны.

Язык интерфейса — русский, язык логов и ошибок — английский, комментарии в коде — по-русски.

## Команды

```bash
go build ./... && go vet ./... && go test -race ./...
make run | make sync | make test-db | make errors
```

Интеграционные тесты `internal/storage/postgres` пропускаются без `TEST_DATABASE_URL`.

## Архитектура

Зависимости идут только к домену: `telegram`, `syncer`, `storage/postgres`, `scraper` →
`schedule`, `users`.

```
cmd/bot/main.go           конфиг → логи → БД (+миграции) → scraper → syncer → сервисы → telegram
internal/config           env → Config, все ошибки сразу (errors.Join), Warnings — некритичное
internal/logging          slog: консоль + JSON-файл с ротацией; логгер апдейта в ctx; NewJournal
internal/schedule         домен: Lesson, Clock, Group, Service (чтение, выбор группы, поиск; кеши)
internal/users            пользователи и доступ: Touch, заявки, статусы, журнал users.log
internal/scraper          клиент Livewire + парсер HTML, дампы непонятых ответов
internal/syncer           RunGroup/RunAll (блокировка на группу), Loop по SYNC_TIMES
internal/storage/postgres pgx/v5, миграции goose вшиты (migrations/embed.go)
internal/telegram         go-telegram/bot: middleware, хендлеры, календарь, форматирование
```

Правила:
- `tgbotapi`/`go-telegram` — только в `internal/telegram`. Сервисы принимают `ctx` и доменные типы.
- Ошибки — sentinel + `errors.Is`. Единственное сравнение по строке — `isNotModified`
  (ответ Telegram API).
- Callback-данные строятся и разбираются только в [callback.go](internal/telegram/callback.go):
  `day:<gid>:YYYY-MM-DD`, `cal:<gid>:YYYY-MM`, `grp:<gid>`, `acc:<uid>:a|r`,
  `adm:l|v|s:<tab>:<page>[:<uid>[:a|b]]`, `noop`. Лимит 64 байта, поэтому в кнопке id, а не
  название.
- Доступ проверяет middleware `checkAccess` (до всех хендлеров); админские команды оборачиваются
  в `adminOnly`, админские кнопки ещё раз проверяют `IsAdmin`. Админ-панель (`/admin`) —
  в [admin.go](internal/telegram/admin.go): вкладки по статусу, страницы по `users.PageSize`,
  `users.Service.Page` (счётчики + `LIMIT/OFFSET` по индексу `users_status_seen_idx`).
- Скорость: каждый запрос к Telegram дорогой (у пользователя прокси, ~1 с). Одно сообщение на
  команду, `answerAsync` для callback, выбор группы — редактирование, а не новые сообщения.
- Логи в хендлерах — через `b.logger(ctx)`: в нём уже есть update_id/user_id/chat_id.
- Пользователь идентифицируется по `From.ID` (`senderID`), сообщения шлются в `Chat.ID`.
- Даты — `time.Time` в полночь UTC (`schedule.DateOf`), в БД — `DATE`; время пары — `schedule.Clock`.

## Протокол сайта (Laravel Livewire v2)

1. `GET /list` → HTML. Состояние компонента — атрибут `wire:initial-data` (fingerprint +
   serverMemo), CSRF — `livewire_token = '...'` в inline-скрипте. Сессия в cookie.
2. `POST /livewire/message/main`, заголовки `X-Livewire: true`, `X-CSRF-TOKEN`. Тело:
   `{fingerprint, serverMemo, updates}`. Методы: `set(<группа>)`, `addMonth`, `minusMonth`,
   `resetGroup`; ввод в поиск — `syncInput` поля `search`.
3. Ответ: `effects.html` (null, если не изменилось) и частичный `serverMemo` — `data` вливается
   поключево, остальное заменяется.

**serverMemo подписан HMAC от json_encode — порядок ключей обязан сохраняться.** Поэтому
[ordered.go](internal/scraper/ordered.go), а не `map[string]any`.

Сверка: `serverMemo.data.count` = числу строк таблицы, иначе `ErrCountMismatch` и данные в БД
не меняются. Таблица — `table.table-list tbody tr`, колонки по `x-show="getColumn('<имя>')"`.
Список групп при поиске — `li[data-group]`. Сайт дублирует аудиторию («Альфа 5.7Альфа 5.7») —
`undouble`.

Фикстуры в `internal/scraper/testdata` сняты с реального сайта 2026-10-06. Сайт недоступен
из-за рубежа: если парсер сломался — смотреть `logs/dumps/` с сервера.

## Данные

- `lessons` заменяются атомарно по диапазону месяцев группы (`ReplaceLessons`).
- `sync_runs` — журнал; по успешным строкам считается покрытие (`Coverage`): дата вне его —
  «данных нет», внутри без пар — «пар нет».
- `groups` — справочник, id для кнопок.
- `users` — все, кто писал боту: профиль, `status` (active/pending/blocked), `group_id`
  (NULL — группа не выбрана, группы по умолчанию нет).
- Обновляются только группы пользователей со `status = 'active'` (`TrackedGroups`).
