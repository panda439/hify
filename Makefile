-include .env
export

.PHONY: dev build migrate-up migrate-down sqlc check-deps db-up db-down web-dev web-build

dev:
	air

build: web-build
	go build -o bin/hify ./cmd/hify

migrate-up:
	go run ./cmd/hify migrate up

migrate-down:
	go run ./cmd/hify migrate down

sqlc:
	cd internal/db && sqlc generate

check-deps:
	./scripts/check-deps.sh

db-up:
	docker compose up -d

db-down:
	docker compose down

web-dev:
	cd web && npm run dev

web-build:
	cd web && npm run build
