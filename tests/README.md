# Проверка PostgreSQL ядра

Нужны PostgreSQL 17 с `initdb`, `pg_ctl`, `psql`, `createdb` в PATH и Python 3.10+. Тесты используют только стандартную библиотеку Python. Пример ниже — PowerShell из корня репозитория.

Команды создают отдельный временный кластер с доверием локальным подключениям, доступный только на `127.0.0.1:55487`. Он предназначен для синтетических тестов без клиентских данных. Роли и база создаются внутри этого кластера; системный сервис PostgreSQL не изменяется.

```powershell
New-Item -ItemType Directory -Path work -Force | Out-Null
initdb -D work/test-pg -U wms_test_admin -A trust --encoding=UTF8 --locale=C
pg_ctl -D work/test-pg -l work/test-pg.log -o '-h 127.0.0.1 -p 55487' -w start

psql -X -h 127.0.0.1 -p 55487 -U wms_test_admin -d postgres -v ON_ERROR_STOP=1 -c 'CREATE ROLE wms_test_owner NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE; CREATE ROLE wms_test_runtime NOLOGIN; CREATE ROLE wms_test_module NOLOGIN'
createdb -h 127.0.0.1 -p 55487 -U wms_test_admin -O wms_test_owner wms_kernel_test

psql -X -h 127.0.0.1 -p 55487 -U wms_test_admin -d wms_kernel_test -v ON_ERROR_STOP=1 -v runtime_role=wms_test_runtime -v module_role=wms_test_module -c 'SET ROLE wms_test_owner' -f db/migrations/0001_inventory_schema.sql -f db/migrations/0002_inventory_commands.sql -f db/bootstrap/grants.psql -f db/examples/dispatch_quality_schema.sql

python tests/postgres_kernel.py --psql (Get-Command psql).Source --port 55487 --database wms_kernel_test --admin wms_test_admin --report work/postgres-kernel.json

pg_ctl -D work/test-pg -m fast -w stop
```

Каждый внешний процесс должен завершиться с кодом 0; PowerShell не останавливает последовательность автоматически при ненулевом коде внешней программы. Миграции применяются один раз. Повторный прогон самих тестов создаёт новые синтетические объекты; для полностью чистого прогона используйте новый отдельный тестовый кластер/базу.

Проверяются конкурентные сессии, уникальность серии, точные количества, идемпотентность, ограничения прав, подготовка команды под guard, откат при ошибке и сверка агрегатов с историей. Искусственная задержка внутри конкурирующих тестовых транзакций нужна для воспроизведения конкуренции и не используется в коде продукта.

Тесты не проверяют скорость на промышленном объёме, failover, отключение питания и авторизацию HTTP-приложения. Контракты в `contracts/` — архитектурные примеры; основной набор событий склада будет расширяться вместе с прикладными процессами.

## Go API и guards

Для backend предпочтителен `scripts/test-postgres.ps1`: он поднимает отдельный кластер на свободном локальном порту, применяет миграции `0001`–`0005`, выдаёт backend права и проверяет Go API, OIDC, guards, конкуренцию и прежние SQL-сценарии вместе. В отличие от старого примера выше, он сам останавливает кластер в `finally`.

Полный прогон включает контроль истории модулей, потерю подтверждения `COMMIT` и обновление пакета dispatch-quality с сохранением данных. Итоговый summarizer требует выполнения этих сценариев и не засчитывает пропуски как успех.

Нужны Go 1.27.3 и C-компилятор для race detector (на Windows — подходящий MinGW-w64). Команда:

```powershell
./scripts/test-postgres.ps1 -PgBin 'C:/Program Files/PostgreSQL/17/bin' -GoExe 'C:/path/to/go.exe' -PythonExe 'C:/path/to/python.exe'
```

При ручном запуске Go integration tests передайте `WMS_TEST_ADMIN_DSN` и `WMS_TEST_RUNTIME_DSN` для подготовленной локальной тестовой базы. `go test -race ./...` запускает unit tests; `go test -race -tags=integration ./...` включает БД. Итоговый скрипт не считает пропущенные интеграционные тесты успешной проверкой.

Большой набор истории и нагрузка имеют отдельные теги `integration,load`, отдельный жизненный цикл и сохраняемые измерения: [нагрузочные проверки](../docs/load-testing.md). Не смешивайте race detector или параллельные benchmark-процессы с замерами скорости.
