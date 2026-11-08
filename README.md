# Inventory & Order Management API

Небольшой REST API для товаров, складских остатков и заказов. Стек проекта — Go 1.23+, PostgreSQL 16 и Docker Compose.

## Запуск

Из корня проекта выполните команды по порядку. Нужен Docker Compose. `make` для этого не требуется.

```sh
docker compose up -d --wait postgres
docker compose exec -T postgres psql -X -U postgres -d inventory -v ON_ERROR_STOP=1 --single-transaction -f /migrations/001_init.up.sql -f /migrations/002_constraints.up.sql
docker compose up -d --build api
curl http://localhost:8080/health
```

Для пустой базы `make migrate-up` выполняет вторую команду. Если в базе уже применена `001_init`, укажите только `-f /migrations/002_constraints.up.sql`. Если `002` найдёт строки, нарушающие новые ограничения, она целиком откатится. Исправьте такие строки по фактическим данным и повторите запуск миграции. `make migrate-down` удаляет таблицы и данные.

Для запуска API без контейнера используйте `go run ./cmd/api` с доступной PostgreSQL и применёнными миграциями.

| Переменная | Значение по умолчанию |
|---|---|
| `PORT` | `8080` |
| `DATABASE_URL` | `postgres://postgres:postgres@localhost:5432/inventory?sslmode=disable` |

## API

```sh
curl -X POST http://localhost:8080/products \
  -H 'Content-Type: application/json' \
  -d '{"sku":"KB-001","name":"Keyboard","price_cents":4999,"stock":10}'

curl http://localhost:8080/products

curl -X POST http://localhost:8080/orders \
  -H 'Content-Type: application/json' \
  -d '{"customer_email":"user@example.com","items":[{"product_id":1,"quantity":2}]}'

curl http://localhost:8080/orders/1
curl -X POST http://localhost:8080/orders/1/cancel
```

В примерах замените `1` на `id`, который вернуло создание товара или заказа. `GET /orders/{id}` возвращает позиции с ценой на момент покупки. Повторная отмена также возвращает `204`.

Цена передаётся целым числом в минимальных денежных единицах. SKU сравниваются с учётом регистра. Для заказа нужен email без имени получателя и хотя бы одна позиция с положительным количеством. Если один товар указан несколько раз, резервируется суммарное количество, а в заказе сохраняются исходные позиции.

| HTTP | Причина |
|---|---|
| `400` | Ошибка JSON, ID или входных данных |
| `404` | Товар или заказ не найден |
| `409` | SKU уже существует или товара недостаточно |
| `413` | Тело запроса больше 1 МиБ |
| `500` | Внутренняя ошибка. Подробности записываются в логи сервера |

## Тесты

```sh
go test ./...
go vet ./...
```

Чтобы запустить интеграционные тесты с PostgreSQL, укажите `TEST_DATABASE_URL`. Роли нужно право создавать схемы. Тесты применяют миграции во временной схеме и удаляют её по завершении.

```sh
TEST_DATABASE_URL='postgres://postgres:postgres@localhost:5432/inventory?sslmode=disable' go test -count=1 ./...
```

В PowerShell сначала выполните `$env:TEST_DATABASE_URL = 'postgres://postgres:postgres@localhost:5432/inventory?sslmode=disable'`, затем `go test -count=1 ./...`.

Устройство транзакций и ограничения описаны в [технической записке](TECHNICAL_NOTES.md).
