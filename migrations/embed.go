// Package migrations встраивает SQL-миграции в бинарник: они применяются при старте
// бота, поэтому в Docker-образ не нужно класть ни goose, ни сами .sql-файлы.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
