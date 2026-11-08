.PHONY: run test race vet fmt migrate-up migrate-down

PSQL = docker compose exec -T postgres psql -X -U postgres -d inventory -v ON_ERROR_STOP=1 --single-transaction

run:
	go run ./cmd/api

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -w ./cmd ./internal

migrate-up:
	$(PSQL) -f /migrations/001_init.up.sql -f /migrations/002_constraints.up.sql

migrate-down:
	$(PSQL) -f /migrations/002_constraints.down.sql -f /migrations/001_init.down.sql
