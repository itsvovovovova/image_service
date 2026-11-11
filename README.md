# Image Service v2

Сервис фоновой обработки изображений. Go отвечает за HTTP API, жизненный цикл задачи и доставку через RabbitMQ. Python/Pillow применяет фильтр и работает с S3-совместимым хранилищем SeaweedFS. PostgreSQL хранит пользователей, задачи и outbox.

## Локальный запуск

Нужны Docker и Docker Compose v2.

```bash
cp .env.example .env
docker compose up -d --build --wait
curl -fsS http://127.0.0.1:8007/readyz
```

Локальные адреса: API на `127.0.0.1:8007`, Grafana на `127.0.0.1:3000` с пользователем `admin` и паролем из `.env`. Остальные компоненты доступны внутри Compose-сети. Значения из `.env.example` предназначены для локального запуска.

## API

```bash
base=http://127.0.0.1:8007
credentials='{"username":"demo","password":"local-demo-password"}'
curl -fsS -H 'Content-Type: application/json' -d "$credentials" "$base/register"
token=$(curl -fsS -H 'Content-Type: application/json' -d "$credentials" "$base/login" | jq -r .token)
base64 -d qa/fixture.base64 > /tmp/image-service-example.png
id=$(curl -fsS -H "Authorization: Bearer $token" -H 'Content-Type: image/png' \
  -H 'Idempotency-Key: example-negative' --data-binary @/tmp/image-service-example.png \
  "$base/task?filter=Negative" | jq -r .task_id)
curl -fsS -H "Authorization: Bearer $token" "$base/status/$id"
# После статуса succeeded
curl -fsS -H "Authorization: Bearer $token" "$base/result/$id" -o result.jpg
```

`POST /task` принимает бинарное тело и фильтр в query. Поддерживаются JPEG, PNG, GIF и WebP без анимации, до 10 МиБ и 20 Мп. Статусы: `queued`, `processing`, `succeeded`, `failed`. Для ошибок обработки возвращается `error_code`. Ожидаемый результат — JPEG.

| Запрос | Ответ |
|---|---|
| `POST /register`, `POST /login` | `201` при регистрации, `200` с JWT при входе |
| `POST /task?filter=Negative` | `202` с ID и заголовком `Location` |
| Повтор запроса с тем же `Idempotency-Key` и содержимым от того же пользователя | `202` с исходным ID |
| Тот же ключ с другим изображением или фильтром | `409` |
| `GET /status/{id}` | `200` со статусом. Для чужой и отсутствующей задачи — `404` |
| `GET /result/{id}` | `200` с JPEG, `409` пока результат недоступен, `404` для чужой или отсутствующей задачи |

Фильтры: Negative, Black & White, Soft Blur, Sharpen Details, Sketch Outline, Contour Drawing, Emboss Effect, Poster Style и Photo Negative.

## Доставка и восстановление

1. API кладёт оригинал в S3, затем записывает задачу и outbox в одной транзакции. Уникальный ключ PostgreSQL обеспечивает идемпотентность при параллельных запросах.
2. Dispatcher захватывает outbox через `FOR UPDATE SKIP LOCKED` и отмечает публикацию после broker confirm. При сбое между confirm и commit возможна повторная доставка.
3. Воркер захватывает задачу через условный `UPDATE` и получает временный lease. Результат хранится под ключом попытки. PostgreSQL принимает результат только от текущей попытки с действующим lease. Воркер подтверждает сообщение после фиксации состояния.
4. Dispatcher возвращает истёкшие задачи в очередь. Временные ошибки получают ограниченные повторы с задержкой. Битый файл переводит задачу в `failed`. Если публикация результата в S3 успела до аварии, следующая попытка использует отдельный объект. Клиент получает результат по ключу, который зафиксирован в PostgreSQL.

Для диагностики сирот старше 24 часов запусти `docker compose run --rm api gc`. Для удаления после проверки списка используй `docker compose run --rm api gc-delete`. Команда проверяет ссылки на оригинал, итоговый результат и текущую попытку по PostgreSQL.

Гарантии рассчитаны на повторную доставку и сохранность томов. При полной потере тома RabbitMQ оператору потребуется перепубликовать незавершённые задачи. Этот стенд рассчитан на одну VM. JWT живёт один час и проверяется по подписи, поэтому Redis для сессий не нужен. Для v2 используются новые таблицы. Историческая FastAPI-версия доступна на коммите [57306d1](https://github.com/itsvovovovova/image_service/tree/57306d1c218f4e113bd1943c8c1b94abb347d732). Перенос её пользователей и изображений в v2 требует отдельного импорта.

## QA и наблюдаемость

```bash
go test -race ./...
python3 -m pip install -r qa/requirements.txt
python3 -m pytest qa
RATE=3 DURATION=30s k6 run --summary-export=k6-summary.json qa/load.js
```

Тест отказа запускается только на отдельном стенде. Он ставит временный SQL-триггер, убеждается, что первый результат записан в S3, завершает воркер до фиксации статуса, затем проверяет восстановление и повторную доставку. В CI lease сокращён для этого сценария. Локально перед запуском пересоздай воркер и dispatcher с тем же lease:

```bash
JOB_LEASE_SECONDS=20 docker compose up -d --wait dispatch worker
bash qa/recovery.sh
```

GitHub Actions публикует JUnit и отчёт k6 в артефактах запуска. Порог smoke-нагрузки проверяет отдельно p95 приёма и p95 до готового результата. Нагрузка использует PNG 16×16. Результат такого прогона описывает именно этот размер входа и окружение раннера.

Grafana показывает глубину очереди, возраст старейшей задачи, p95 обработки и p95 полного цикла. Prometheus содержит правила для старого backlog и отсутствующего воркера. Для демонстрации backlog останови `worker`, создай несколько задач и запусти `worker` снова. Для параллельной обработки можно запустить `docker compose up -d --scale worker=3 worker`.

## Release и staging

Тег `v2.x.y` запускает проверки и публикует два образа в GHCR. Деплой на VM включается переменной репозитория `DEPLOY_ENABLED=true` и использует окружение `staging`. Нужны `DEPLOY_HOST` в форме `user@host`, `DEPLOY_KEY` и `DEPLOY_KNOWN_HOSTS`. На VM создай `~/image-service/.env` с собственными значениями. Для паролей PostgreSQL и RabbitMQ используй URL-безопасные строки, например из `openssl rand -hex 24`. Если пакеты GHCR закрыты, настрой read-only доступ с VM.

Workflow загружает конфигурацию в каталог релиза и запускает Compose с постоянным именем проекта. При провале healthcheck восстанавливает прошлый релиз. Откат использует те же тома, поэтому миграции для последующих релизов должны быть обратно совместимыми. API и Grafana слушают loopback. Для внешнего доступа добавь TLS reverse proxy либо используй SSH-туннель.

Формулировка для резюме после прогона CI: «Реализовал на Go API фоновой обработки изображений с transactional outbox, идемпотентностью и восстановлением после падения воркера. Настроил CI с проверкой повторной доставки, нагрузочным smoke-тестом и наблюдаемостью через Prometheus/Grafana».
