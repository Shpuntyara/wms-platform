# Установщик отдельного сервера WMS

`deploy/linux/bootstrap.sh` устанавливает одну независимую WMS на **Ubuntu
24.04 LTS x86-64**. На управляющем Windows достаточно OpenSSH: Ansible
исполняется с `connection: local`, `hosts: localhost`, `--limit localhost`
внутри выбранной VM. Приватный SSH-ключ контроллера в гостя не копируется.
Инсталляция не использует tenant registry или общую клиентскую базу.

На каждом сервере создаются собственные UUID установки, SQL-пароли,
request/outbox HMAC-ключи, private TLS CA/leaf key и Keycloak realm/client.
OIDC signing keys Keycloak генерирует самостоятельно и сохраняет в своей БД.
API и worker имеют разные непривилегированные SQL logins одной установки;
один процесс обслуживает несколько организаций/складов согласно DB grants.
DB admin secret имеет mode 0400/root. Keycloak получает только один read-only
realm file 0400 с владельцем UID 1000 образа; его родитель на госте остаётся
root-only 0700. Пользователь Caddy может читать TLS leaf key и публичный
сертификат, но не SQL/OIDC/HMAC credentials или private CA key.

## Состав и порты

| Компонент | Способ запуска | Доступ |
|---|---|---|
| WMS API + worker | `wms-api.service`, пользователь `wms` | `127.0.0.1:8080` |
| Background health/metrics | тот же процесс | `127.0.0.1:9091` |
| PostgreSQL 17.11 | официальный Docker image, именованный volume | `127.0.0.1:5432` |
| Keycloak 26.8.1 | официальный Quay image, отдельная БД в локальном PG | `127.0.0.1:8081` |
| Grafana 12.3.0 | официальный image, именованный volume | `127.0.0.1:3000` |
| Caddy | Ubuntu package, systemd | HTTPS `VM_IP:443` |
| Prometheus | Ubuntu package, systemd | только адрес docker0, порт 9090 |
| Лабораторный приёмник событий | `wms-lab-sink.service`, SQLite | `127.0.0.1:8091` |

Образы закреплены тегом **и manifest digest** в `wmsctl.py`; реальный image
ID/RepoDigest записывается в `/var/lib/wms/image-receipts.json`. Caddy,
Prometheus, Docker и Ansible устанавливаются из подписанного Ubuntu apt
репозитория. Их фактические версии необходимо сохранять в evidence VM;
обновление ОС — самостоятельная операция обслуживания.
Образы загружаются последовательно с явным `--platform linux/amd64`.
Временная ошибка или timeout повторяется не более трёх раз для одного
образа (паузы 2/4 секунды, timeout одной попытки 600 секунд). Окончательная
ошибка прерывает установку до запуска контейнеров/изменений БД; при обновлении
ранее работавший API восстанавливается, пока SQL migrations не начинались.

Prometheus читает loopback metrics WMS. Его HTTP API доступен контейнеру
Grafana через docker0 и не слушает адрес локальной сети VM. Caddy публикует
Grafana с ролью **Viewer** за отдельной HTTPS Basic Auth: `wms-viewer`, пароль
`monitor_password` из root-only secret store. Без credentials маршрут
`/monitor/*` возвращает 401. Административный пароль Grafana отдельный.
На сетевом адресе VM должны быть доступны только SSH 22 и HTTPS 443.
Systemd drop-in Prometheus требует/ожидает `docker.service` перед привязкой
docker0 и повторяет неудачный запуск через 5 секунд. После перезапуска VM
проверяются и WMS, и живой monitoring listener. Readiness stat в Grafana
использует instant query с нулём при недоступном scrape/отсутствующей метрике,
чтобы последнее историческое значение `1` не скрывало текущий сбой.

**Лабораторный sink не является ERP-коннектором.** Он проверяет HMAC,
временное окно и повтор UUID, подтверждает событие только после SQLite
COMMIT с `synchronous=FULL`. Он хранит UUID и hash payload, а не бизнес-
обработку ERP. Для клиента замените endpoint и приёмник проверенной
интеграцией; endpoint должен быть HTTPS с проверяемой цепочкой доверия.

## Контракт доверенного release bundle

Контроллер собирает Linux ELF `bin/wms-api` и tar.gz с обычными файлами:

```text
bin/wms-api
db/migrations/*.sql
db/profiles/hardened-v1.sql
db/profiles/onprem-v1.sql
db/bootstrap/onprem-grants.psql
scripts/migrate_core.py
release-manifest.json
```

Можно включить остальные db/scripts/deploy файлы. Имена начинаются
непосредственно с `bin/`, `db/` и т. д., без `./`, абсолютных путей,
symlinks/hardlinks, backslash или `..`. Manifest покрывает каждый обычный
файл, кроме самого manifest:

```json
{"schema":1,"revision":"FULL_40_HEX_GIT_REVISION","files":{"bin/wms-api":"SHA256","db/...":"SHA256"}}
```

Archive SHA256 и ожидаемая ревизия передаются по доверенному каналу
контроллера. Manifest/digest доказывают соответствие выбранному bundle,
но сами по себе не являются подписью издателя. Не принимайте digest с
той же недоверенной страницы, откуда получен неизвестный архив.

## Первая установка

Передайте bundle и `deploy/linux`/`deploy/ansible` с контроллера через SSH
на конкретного гостя. Выполните там:

```sh
sudo bash /srv/wms-installer/deploy/linux/bootstrap.sh \
  --archive /srv/wms-release.tar.gz \
  --sha256 EXPECTED_ARCHIVE_SHA256 \
  --revision FULL_40_HEX_GIT_REVISION \
  --address VM_IP
sudo wmsctl status
```

Первый запуск устанавливает пакеты, проверяет archive/manifest, создаёт
root-only `/etc/wms/secrets.json`, применяет неизменённые core migrations
с историческими checksum receipts, затем historical hardened и forward
onprem profile. SQL получает секреты через stdin, без password в argv.
Установщик не переписывает опубликованные migrations или их checksum.

На новой PG БД обязательны `data_checksums`, `fsync`, `full_page_writes`,
`synchronous_commit=on`. `ignore_checksum_failure`, `zero_damaged_pages`,
`ignore_invalid_pages` выключены. Перед запуском эти настройки проверяются.

TLS CA создаётся только на этом госте; leaf certificate содержит IP SAN.
CA устанавливается в системное доверие гостя для настоящей HTTPS OIDC
discovery/JWKS и outbox delivery. Внешний клиент должен доверять именно
этому публичному CA: экспортируйте **только** `ca.crt` по доверенному SSH
каналу или используйте проверяемый proxy на контроллере. TLS verification
не отключается. Ключ CA и leaf key не экспортируйте в портфолио.

## Обновление одной установки

```sh
sudo wmsctl update --archive /srv/wms-next.tar.gz \
  --sha256 EXPECTED_ARCHIVE_SHA256 --revision FULL_40_HEX_GIT_REVISION \
  --address SAME_VM_IP
sudo wmsctl status
```

Bundle сначала проверяется и распаковывается в отдельный release directory.
Затем прекращаются API/worker записи. Keycloak тоже кратко останавливается
на время согласованного snapshot: создаются отдельные dumps **WMS и Keycloak**,
сохраняются globals и private secrets/TLS configuration. До возобновления
Keycloak вычисляются counts и SHA256 содержимого каждой таблицы обеих БД;
сырой SQL с приватными ключами/паролями не выводится. Keycloak возвращается
в прежнее состояние в `finally` даже при ошибке snapshot; проверяется его
настоящий HTTPS issuer.

Оба dumps восстанавливаются в отдельные временные БД без API/worker/Keycloak
потребителей. Для каждой таблицы сравниваются количество строк и SHA256,
включая WMS business rows/command receipts/grants/signing metadata и Keycloak
realm/client/grants/private signing-key configuration. Дополнительно
проверяются UUID установки, ожидаемый OIDC client и наличие RSA signing key.
Секреты остаются в БД и root-only файлах; receipt содержит только сводные
fingerprints. Удаляются только сгенерированные rehearsal DB.
Далее выполняются проверки durability, forward migrations и атомарное
переключение `/opt/wms/current`; служба запускается и проверяет HTTPS
readiness и background health. Секреты/UUID/issuer/client/volumes сохраняются.
Вторая независимая VM этой операцией не затрагивается.

Ошибки после начала migrations оставляют запись остановленной для
разбора/forward fix. Автоматического запуска старого бинарника поверх
изменённой БД, downgrade SQL или восстановления старого dump нет.
После подтверждённых новых команд откат backup может потерять данные.
Смена IP/issuer отклоняется: это отдельная миграция identity и сертификатов.

## Обслуживание и диагностика

```sh
sudo wmsctl status
sudo wmsctl backup
sudo systemctl status wms-api wms-lab-sink caddy prometheus
sudo journalctl -u wms-api --since '-15 minutes' --no-pager
sudo docker compose -p wms -f /etc/wms/compose.yaml ps
sudo ss -lntp
```

`status` выводит только публичную identity, release hash, реальную readiness,
background snapshot, durability и fingerprint публичного CA. Пароли, JWT,
HMAC и приватные ключи не попадают в receipts. Не добавляйте `env`,
`docker inspect` с environment или содержимое `/etc/wms/*env` в screenshots.

Grafana: `https://VM_IP/monitor/d/wms-operations`. Панели показывают
реальные delivered/pending/dead events, background readiness и возраст
очереди. Prometheus содержит правила для background outage и dead letters;
направление уведомлений/дежурство нужно настроить отдельно. Наличие правил
не означает подключённый канал оповещений или согласованный production SLO.
Для портфолио локальный proxy контроллера может сам добавлять Viewer Basic
Auth к запросам по проверяемому TLS; браузеру не нужны административные
credentials. Не встраивайте пароль в URL или исходники страницы.

`backup` ограниченно останавливает API/worker, кратко quiesces Keycloak на
время snapshot, создаёт root-only `/var/lib/wms/backups/TIMESTAMP/` с
`wms.dump`, `keycloak.dump`, globals, private identity archive и schema-2
restore receipt, затем возвращает работавшие службы. Ошибка dump, restore
или fingerprint проверки не создаёт успешный receipt; `finally` возвращает
Keycloak и API. Обновление после начала SQL migrations по-прежнему не
возвращает старый бинарник и не запускает запись автоматически.

Schema-2 receipt указывает hash каждого dump, границы snapshot, результаты
обоих isolated restores и сверку source/restored fingerprints. Старые
WMS-only receipts не доказывают сохранность OIDC signing identity и не
принимаются как recovery preflight нового обновления. Файлы
`globals.sql` и `identity.tar.gz` также закреплены отдельными SHA256 в receipt;
preflight отвергает отсутствие или изменение любого из этих двух recovery
artifacts, включая TLS/app-signing material. Сами локальные копии
**не зашифрованы**: каталог доступен только root, но находится на том же
виртуальном диске. Для production обязательны шифрованный off-host export,
политика retention и регулярный полный restore rehearsal. Логический dump
не даёт PITR; здесь не настроены WAL archive, HA или нулевой RPO.
Восстановимая точка — момент quiesced snapshot; команды после него требуют
отдельного recovery endpoint/WAL-плана. История Prometheus/Grafana volumes
не входит в этот логический backup; конфигурация готовых панелей сохранена.

Один сервер остаётся единой точкой отказа. Для обновления ОС и контейнеров
используйте отдельное окно и текущий backup/restore receipt. Не запускайте
`docker compose down -v`, prune или пересоздание VM для обычного обновления.

## Проверка установщика

```sh
python3 -m unittest discover -s tests -p 'test*installer*.py'
python3 deploy/linux/verify_installer.py
ansible-playbook -i localhost, -c local deploy/ansible/site.yml --syntax-check
```

Behavior tests проверяют archive/revision/digest/path boundary,
постоянство и независимость секретов, durability/backup gates. Randomized
path property cases используют фиксированный seed для воспроизведения.
Mutation gate требует настоящий assertion failure для каждого mutant;
ошибка импорта, timeout, отсутствующий/skipped test не считаются kill.
Это дополняет полный CI и реальные две VM; unit tests не доказывают
работоспособность Linux packages, OIDC, restore или UI сами по себе.

Первичные источники: [официальный PostgreSQL image](https://hub.docker.com/_/postgres),
[Keycloak containers](https://www.keycloak.org/server/containers),
[Keycloak 26.8.1](https://www.keycloak.org/downloads),
[Grafana Docker](https://grafana.com/docs/grafana/latest/setup-grafana/installation/docker/),
[Caddy TLS](https://caddyserver.com/docs/caddyfile/directives/tls),
[Prometheus configuration](https://prometheus.io/docs/prometheus/latest/configuration/configuration/).
