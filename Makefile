-include .env
export

.PHONY: dev build test test-race migrate-up migrate-down sqlc check-deps db-up db-down web-dev web-build eval

dev:
	air

# 集成测试依赖 docker-compose 容器（make db-up）；容器没起时自动 skip，
# 只跑纯逻辑单测。
test:
	go vet ./internal/...
	go test ./internal/...

test-race:
	go vet ./internal/...
	go test -race -count=1 ./internal/...

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

# 跑一遍 eval/testset.yaml，和 eval/baseline.json 做回归对比。需要
# JUDGE_MODEL_ID/EVAL_USER_ID 两个环境变量（裁判模型和跑测试用的现有用户）。
eval:
	go run ./cmd/evalrunner --testset eval/testset.yaml --judge-model-id $(JUDGE_MODEL_ID) --user-id $(EVAL_USER_ID) --baseline eval/baseline.json
