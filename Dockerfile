# --- сборка ---
FROM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/bot ./cmd/bot

# --- рантайм ---
# distroless/static: только CA-сертификаты и непривилегированный пользователь, без шелла.
# Миграции и база часовых поясов вшиты в бинарник, поэтому кроме него ничего не копируем.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bot /bot
USER nonroot:nonroot
ENTRYPOINT ["/bot"]
