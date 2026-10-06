.PHONY: build run sync test test-db vet lint up down logs errors

# Переменные из .env для локального запуска без Docker.
ENV = set -a && . ./.env && set +a

build:
	go build -o bin/bot ./cmd/bot

run:
	$(ENV) && go run ./cmd/bot

# Разовая загрузка расписания (без бота).
sync:
	$(ENV) && go run ./cmd/bot -sync-once

test:
	go test -race ./...

# Интеграционные тесты хранилища на Postgres из docker-compose (make up или docker compose up -d db).
test-db:
	$(ENV) && TEST_DATABASE_URL="$$DATABASE_URL" go test -race -count=1 ./internal/storage/...

vet:
	go vet ./...

lint:
	golangci-lint run

up:
	docker compose up -d --build

down:
	docker compose down

logs:
	docker compose logs -f bot

# Последние ошибки и предупреждения из файла лога.
errors:
	@grep -hE '"level":"(ERROR|WARN)"' logs/bot.log | tail -n 30
